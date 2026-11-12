// Package proxy implements a loopback-only HTTP/1.1 forwarding proxy that can
// inject connection-level faults at precisely defined stages.
//
// The proxy is deliberately not a general-purpose reverse proxy. It forwards
// one bounded request at a time to one configured loopback upstream, and it
// requires the harness control headers so it cannot be pointed at unrelated
// traffic by accident.
//
// The stage that matters most is StageAfterUpstreamResponse: the proxy has read
// a complete upstream response, so the upstream has finished its work, and only
// then does it destroy the client connection. That reproduces a lost response
// after successful server execution -- the ambiguous completion that makes
// retries unsafe. The proxy never retries anything itself; retry policy belongs
// to the test client.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/faults"
)

// Body bounds. The PRD requires non-streaming bounded bodies so that attempts
// stay replayable and a scenario cannot exhaust memory.
const (
	MaxRequestBodyBytes  = 64 << 10
	MaxResponseBodyBytes = 64 << 10
)

// Harness control headers. The client must set both; the proxy strips them
// before forwarding so the upstream never sees them.
// See docs/decisions/ADR-001-harness-control-headers.md.
const (
	HeaderCaseID  = "X-Faulttest-Case-Id"
	HeaderAttempt = "X-Faulttest-Attempt"
)

// quiescencePollInterval is how often WaitForQuiescence rechecks the in-flight
// count. Polling keeps cancellation simple and costs nothing at these scales.
const quiescencePollInterval = 2 * time.Millisecond

// hopByHopHeaders must not be forwarded in either direction (RFC 9110 7.6.1).
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Options configures a Proxy.
type Options struct {
	// Upstream is the loopback origin to forward to, e.g. http://127.0.0.1:8080.
	Upstream string
	// Schedule decides which faults to inject. Required; pass an empty schedule
	// for no-fault operation.
	Schedule *faults.Schedule
	// UpstreamTimeout bounds how long the proxy waits for upstream response
	// headers. Zero selects a 30s default.
	UpstreamTimeout time.Duration
}

// Stats is a snapshot of proxy counters.
// The tags matter: Stats is serialised into the published result file, which
// is snake_case throughout, and renaming a key after v1 ships would make
// strict parsing reject every result already written.
type Stats struct {
	Requests       int64 `json:"requests"`
	Forwarded      int64 `json:"forwarded"`
	FaultsInjected int64 `json:"faults_injected"`
	Rejected       int64 `json:"rejected"`
}

// Proxy is a running proxy instance.
type Proxy struct {
	upstream  *url.URL
	schedule  *faults.Schedule
	transport *http.Transport
	srv       *http.Server
	baseURL   string

	inflightMu sync.Mutex
	inflight   map[string]int

	requests       atomic.Int64
	forwarded      atomic.Int64
	faultsInjected atomic.Int64
	rejected       atomic.Int64
}

// Start validates the options, binds 127.0.0.1 on an ephemeral port, and serves.
// The caller must call Close.
func Start(opts Options) (*Proxy, error) {
	if opts.Schedule == nil {
		return nil, errors.New("proxy: Schedule is required")
	}
	upstream, err := parseLoopbackUpstream(opts.Upstream)
	if err != nil {
		return nil, err
	}
	timeout := opts.UpstreamTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind proxy listener: %w", err)
	}

	p := &Proxy{
		upstream: upstream,
		schedule: opts.Schedule,
		inflight: make(map[string]int),
		baseURL:  "http://" + ln.Addr().String(),
		transport: &http.Transport{
			// No environment proxy: this tool must never route test traffic
			// through anything the user did not configure here.
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout: 2 * time.Second,
				// Defence in depth: refuse to connect to anything off
				// loopback even if the URL check were bypassed.
				Control: controlLoopbackOnly,
			}).DialContext,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   64,
			MaxConnsPerHost:       128,
			IdleConnTimeout:       20 * time.Second,
			ResponseHeaderTimeout: timeout,
			DisableCompression:    true,
			// HTTP/1.1 only: hard connection control depends on it.
			ForceAttemptHTTP2: false,
		},
	}

	p.srv = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	go func() {
		_ = p.srv.Serve(ln)
	}()

	return p, nil
}

// BaseURL is the proxy's loopback origin.
func (p *Proxy) BaseURL() string { return p.baseURL }

// Stats returns a counter snapshot.
func (p *Proxy) Stats() Stats {
	return Stats{
		Requests:       p.requests.Load(),
		Forwarded:      p.forwarded.Load(),
		FaultsInjected: p.faultsInjected.Load(),
		Rejected:       p.rejected.Load(),
	}
}

// Close stops accepting, waits briefly for in-flight handlers, and releases
// upstream connections so no goroutine outlives the proxy.
func (p *Proxy) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := p.srv.Shutdown(ctx)
	p.transport.CloseIdleConnections()
	if err != nil {
		_ = p.srv.Close()
		return fmt.Errorf("shutdown proxy: %w", err)
	}
	return nil
}

// WaitForQuiescence blocks until no upstream call for caseID is in flight.
//
// This is the settlement boundary the oracle needs: once the proxy holds a
// complete upstream response, the upstream has already applied (or refused) the
// operation, so reading state afterwards cannot race the backend. A timeout
// here means the outcome is unknown, which the caller must report as
// inconclusive rather than as a duplicated side effect.
func (p *Proxy) WaitForQuiescence(ctx context.Context, caseID string) error {
	ticker := time.NewTicker(quiescencePollInterval)
	defer ticker.Stop()

	for {
		p.inflightMu.Lock()
		n := p.inflight[caseID]
		p.inflightMu.Unlock()
		if n == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("case %s still has %d upstream call(s) in flight: %w", caseID, n, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.requests.Add(1)

	if r.ProtoMajor != 1 {
		writeProxyError(w, http.StatusHTTPVersionNotSupported,
			fmt.Sprintf("only HTTP/1.1 is supported, got HTTP/%d.%d", r.ProtoMajor, r.ProtoMinor))
		return
	}

	caseID := r.Header.Get(HeaderCaseID)
	if caseID == "" {
		writeProxyError(w, http.StatusBadRequest, "missing "+HeaderCaseID+" header")
		return
	}
	attempt, err := strconv.Atoi(r.Header.Get(HeaderAttempt))
	if err != nil || attempt < 1 {
		writeProxyError(w, http.StatusBadRequest, "missing or invalid "+HeaderAttempt+" header")
		return
	}

	body, err := readBounded(http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes), MaxRequestBodyBytes)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.Is(err, errBodyTooLarge) {
			writeProxyError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", MaxRequestBodyBytes))
			return
		}
		writeProxyError(w, http.StatusBadRequest, "could not read request body: "+err.Error())
		return
	}

	if decision, ok := p.schedule.Decide(caseID, attempt, faults.StageBeforeForward); ok {
		p.faultsInjected.Add(1)
		switch decision.Action {
		case faults.ActionRejectConnection:
			// The upstream is never contacted: this is the control case that
			// proves a failed attempt had no side effect.
			p.rejected.Add(1)
			p.destroyClientConnection(w)
			return
		case faults.ActionDelay:
			if err := faults.Sleep(r.Context(), decision.Delay); err != nil {
				// The client gave up while we were stalling; nothing to send.
				return
			}
		}
	}

	resp, respBody, err := p.forward(r, body, caseID)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "upstream call failed: "+err.Error())
		return
	}

	// From here the upstream has fully responded, so its state change (if any)
	// has already happened. Anything we do now is a lost or delayed response,
	// never a prevented operation.
	if decision, ok := p.schedule.Decide(caseID, attempt, faults.StageAfterUpstreamResponse); ok {
		p.faultsInjected.Add(1)
		switch decision.Action {
		case faults.ActionCloseClientConnection:
			p.destroyClientConnection(w)
			return
		case faults.ActionDelay:
			if err := faults.Sleep(r.Context(), decision.Delay); err != nil {
				return
			}
		}
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// forward sends one attempt upstream and reads the complete response. The
// in-flight accounting brackets exactly the window during which the upstream
// may still be mutating state.
func (p *Proxy) forward(r *http.Request, body []byte, caseID string) (*http.Response, []byte, error) {
	p.enterInflight(caseID)
	defer p.exitInflight(caseID)

	target := *p.upstream
	target.Path = r.URL.Path
	target.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header = filterRequestHeaders(r.Header)
	req.ContentLength = int64(len(body))
	req.Host = target.Host

	// One client request must forward exactly once. The upstream request
	// carries the client's Idempotency-Key, which makes http.Transport treat it
	// as replayable; a reset or stale pooled connection would then re-apply the
	// operation upstream without the harness knowing, corrupting the very state
	// the oracle reads. Clearing GetBody forbids that replay. See the matching
	// comment in internal/client.
	req.GetBody = nil

	p.forwarded.Add(1)
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBodyBytes))
		_ = resp.Body.Close()
	}()

	respBody, err := readBounded(resp.Body, MaxResponseBodyBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("read upstream response body: %w", err)
	}
	return resp, respBody, nil
}

func (p *Proxy) enterInflight(caseID string) {
	p.inflightMu.Lock()
	p.inflight[caseID]++
	p.inflightMu.Unlock()
}

func (p *Proxy) exitInflight(caseID string) {
	p.inflightMu.Lock()
	if n := p.inflight[caseID] - 1; n > 0 {
		p.inflight[caseID] = n
	} else {
		delete(p.inflight, caseID)
	}
	p.inflightMu.Unlock()
}

// destroyClientConnection takes the client connection away from the HTTP server
// and closes it with a reset, so the client observes an unambiguous transport
// error instead of any response.
//
// An http.ResponseWriter alone cannot do this: the server would still frame and
// flush a response. Hijacking is the supported way to control the connection,
// and it is available because the test path is HTTP/1.1.
func (p *Proxy) destroyClientConnection(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		// Should be unreachable on HTTP/1.1; fail loudly rather than silently
		// returning a success the scenario did not ask for.
		writeProxyError(w, http.StatusInternalServerError, "connection hijacking unavailable")
		return
	}

	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "hijack failed: "+err.Error())
		return
	}
	// Deliberately drop anything buffered: the point is that the client never
	// receives a response.
	_ = buffered

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		// SetLinger(0) makes Close send RST instead of FIN, so the client sees
		// a reset rather than a possibly ambiguous clean EOF.
		_ = tcpConn.SetLinger(0)
	}
	_ = conn.Close()
}

// parseLoopbackUpstream accepts only cleartext HTTP to a literal loopback
// address or "localhost". Hostnames are not resolved, so validation performs no
// DNS lookup and cannot be tricked into touching a public network.
func parseLoopbackUpstream(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("proxy: Upstream is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL %q: %w", raw, err)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("upstream scheme %q is not supported; the MVP is cleartext HTTP/1.1 only", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("upstream URL %q has no host", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("upstream URL %q must not contain a path", raw)
	}

	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return nil, fmt.Errorf("upstream %q is not a loopback address; remote targets are out of scope", host)
		}
	} else if !strings.EqualFold(host, "localhost") {
		return nil, fmt.Errorf("upstream host %q must be a loopback IP or \"localhost\"", host)
	}

	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// controlLoopbackOnly runs after address resolution and before connect.
func controlLoopbackOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("unexpected dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("dial address %q is not an IP", address)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("refusing to dial non-loopback address %s", ip)
	}
	return nil
}

var errBodyTooLarge = errors.New("body exceeds the configured bound")

// readBounded reads at most limit bytes and reports an error if more remain, so
// an oversized body is rejected instead of silently truncated.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}
	return data, nil
}

// filterRequestHeaders copies client headers minus hop-by-hop headers, the
// headers named by Connection, and the harness control headers.
func filterRequestHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	skip := connectionTokens(src)
	skip[http.CanonicalHeaderKey(HeaderCaseID)] = struct{}{}
	skip[http.CanonicalHeaderKey(HeaderAttempt)] = struct{}{}

	for name, values := range src {
		if _, drop := skip[http.CanonicalHeaderKey(name)]; drop {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
	// Content-Length is set from the buffered body by the caller.
	dst.Del("Content-Length")
	return dst
}

// copyResponseHeaders copies upstream headers minus hop-by-hop headers and
// Content-Length, which the caller rewrites for the buffered body.
func copyResponseHeaders(dst, src http.Header) http.Header {
	skip := connectionTokens(src)
	skip[http.CanonicalHeaderKey("Content-Length")] = struct{}{}

	for name, values := range src {
		if _, drop := skip[http.CanonicalHeaderKey(name)]; drop {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
	return dst
}

// connectionTokens returns the always-dropped hop-by-hop headers plus any
// header named in a Connection field value.
func connectionTokens(h http.Header) map[string]struct{} {
	skip := make(map[string]struct{}, len(hopByHopHeaders)+4)
	for _, name := range hopByHopHeaders {
		skip[http.CanonicalHeaderKey(name)] = struct{}{}
	}
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				skip[http.CanonicalHeaderKey(token)] = struct{}{}
			}
		}
	}
	return skip
}

func writeProxyError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Faulttest-Proxy-Error", "1")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, msg)
}
