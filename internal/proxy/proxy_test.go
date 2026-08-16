package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/faults"
	"github.com/kyle-luong/http-fault-injection-tester/internal/leaktest"
)

// recordingUpstream is a loopback stub that records what the proxy forwarded.
type recordingUpstream struct {
	server *httptest.Server
	calls  atomic.Int64

	lastHeader http.Header
	lastBody   []byte
	lastPath   string
	lastQuery  string
}

func startUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *recordingUpstream {
	t.Helper()
	up := &recordingUpstream{}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.calls.Add(1)
		up.lastHeader = r.Header.Clone()
		up.lastPath = r.URL.Path
		up.lastQuery = r.URL.RawQuery
		body, _ := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodyBytes))
		up.lastBody = body
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(up.server.Close)
	return up
}

func startProxy(t *testing.T, upstream string, rules []faults.Rule) *Proxy {
	t.Helper()
	schedule, err := faults.NewSchedule(rules)
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}
	p, err := Start(Options{Upstream: upstream, Schedule: schedule, UpstreamTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("proxy Start: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("proxy Close: %v", err)
		}
	})
	return p
}

func proxyClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{DisableCompression: true, ForceAttemptHTTP2: false}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

// send issues one request through the proxy with the harness control headers.
func send(t *testing.T, hc *http.Client, baseURL, caseID string, attempt int, body string) (*http.Response, []byte, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/operation", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderCaseID, caseID)
	req.Header.Set(HeaderAttempt, strconv.Itoa(attempt))

	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBodyBytes))
	if readErr != nil {
		return resp, nil, readErr
	}
	return resp, raw, nil
}

func TestNoFaultForwardingPreservesResponse(t *testing.T) {
	leaktest.Check(t)
	up := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		w.Header().Add("X-Multi", "a")
		w.Header().Add("X-Multi", "b")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"created":true}`)
	})
	p := startProxy(t, up.server.URL, nil)
	hc := proxyClient(t)

	resp, body, err := send(t, hc, p.BaseURL(), "case-1", 1, `{"case_id":"case-1"}`)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Upstream"); got != "yes" {
		t.Errorf("X-Upstream = %q, want %q", got, "yes")
	}
	if got := resp.Header.Values("X-Multi"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("X-Multi = %v, want [a b]", got)
	}
	if string(body) != `{"created":true}` {
		t.Errorf("body = %q, want the upstream body verbatim", body)
	}
	if string(up.lastBody) != `{"case_id":"case-1"}` {
		t.Errorf("upstream body = %q, want the client body verbatim", up.lastBody)
	}
	if up.calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", up.calls.Load())
	}
	if stats := p.Stats(); stats.Requests != 1 || stats.Forwarded != 1 || stats.FaultsInjected != 0 {
		t.Errorf("stats = %+v, want 1 request, 1 forwarded, 0 faults", stats)
	}
}

func TestPathAndQueryArePreserved(t *testing.T) {
	leaktest.Check(t)
	up := startUpstream(t, nil)
	p := startProxy(t, up.server.URL, nil)
	hc := proxyClient(t)

	req, err := http.NewRequest(http.MethodPost, p.BaseURL()+"/operation?case_id=case-1&x=2", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(HeaderCaseID, "case-1")
	req.Header.Set(HeaderAttempt, "1")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBodyBytes))

	if up.lastPath != "/operation" {
		t.Errorf("upstream path = %q, want /operation", up.lastPath)
	}
	if up.lastQuery != "case_id=case-1&x=2" {
		t.Errorf("upstream query = %q, want the client query verbatim", up.lastQuery)
	}
}

func TestControlHeadersAreNotForwarded(t *testing.T) {
	leaktest.Check(t)
	up := startUpstream(t, nil)
	p := startProxy(t, up.server.URL, nil)
	hc := proxyClient(t)

	if _, _, err := send(t, hc, p.BaseURL(), "case-1", 1, "{}"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := up.lastHeader.Get(HeaderCaseID); got != "" {
		t.Errorf("upstream saw %s = %q; harness headers must be stripped", HeaderCaseID, got)
	}
	if got := up.lastHeader.Get(HeaderAttempt); got != "" {
		t.Errorf("upstream saw %s = %q; harness headers must be stripped", HeaderAttempt, got)
	}
	if got := up.lastHeader.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json preserved", got)
	}
}

func TestFilterRequestHeadersDropsHopByHopAndControlHeaders(t *testing.T) {
	src := http.Header{
		"Content-Type":      []string{"application/json"},
		"Content-Length":    []string{"2"},
		"Connection":        []string{"X-Custom-Hop, Keep-Alive"},
		"X-Custom-Hop":      []string{"dropped"},
		"Keep-Alive":        []string{"timeout=5"},
		"Te":                []string{"trailers"},
		"Transfer-Encoding": []string{"chunked"},
		"Upgrade":           []string{"websocket"},
		"Proxy-Connection":  []string{"keep-alive"},
		HeaderCaseID:        []string{"case-1"},
		HeaderAttempt:       []string{"1"},
		"X-Keep":            []string{"kept"},
	}

	got := filterRequestHeaders(src)

	for _, dropped := range []string{
		"Connection", "X-Custom-Hop", "Keep-Alive", "Te", "Transfer-Encoding",
		"Upgrade", "Proxy-Connection", "Content-Length", HeaderCaseID, HeaderAttempt,
	} {
		if v := got.Get(dropped); v != "" {
			t.Errorf("header %q survived filtering with value %q", dropped, v)
		}
	}
	if v := got.Get("X-Keep"); v != "kept" {
		t.Errorf("X-Keep = %q, want it preserved", v)
	}
	if v := got.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q, want it preserved", v)
	}
	if src.Get("Connection") == "" {
		t.Error("filterRequestHeaders mutated its input")
	}
}

func TestCopyResponseHeadersDropsHopByHopAndContentLength(t *testing.T) {
	src := http.Header{
		"Content-Type":      []string{"application/json"},
		"Content-Length":    []string{"11"},
		"Connection":        []string{"X-Hop"},
		"X-Hop":             []string{"dropped"},
		"Transfer-Encoding": []string{"chunked"},
		"X-Keep":            []string{"kept"},
	}

	got := copyResponseHeaders(http.Header{}, src)

	for _, dropped := range []string{"Connection", "X-Hop", "Transfer-Encoding", "Content-Length"} {
		if v := got.Get(dropped); v != "" {
			t.Errorf("header %q survived filtering with value %q", dropped, v)
		}
	}
	if v := got.Get("X-Keep"); v != "kept" {
		t.Errorf("X-Keep = %q, want it preserved", v)
	}
}

func TestMissingControlHeadersAreRejected(t *testing.T) {
	leaktest.Check(t)
	up := startUpstream(t, nil)
	p := startProxy(t, up.server.URL, nil)
	hc := proxyClient(t)

	tests := []struct {
		name    string
		caseID  string
		attempt string
	}{
		{"no case id", "", "1"},
		{"no attempt", "case-1", ""},
		{"non numeric attempt", "case-1", "first"},
		{"zero attempt", "case-1", "0"},
		{"negative attempt", "case-1", "-1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, p.BaseURL()+"/operation", strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if tc.caseID != "" {
				req.Header.Set(HeaderCaseID, tc.caseID)
			}
			if tc.attempt != "" {
				req.Header.Set(HeaderAttempt, tc.attempt)
			}
			resp, err := hc.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBodyBytes))

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
	if up.calls.Load() != 0 {
		t.Errorf("upstream was called %d times for rejected requests, want 0", up.calls.Load())
	}
}

func TestOversizedRequestBodyIsRejected(t *testing.T) {
	leaktest.Check(t)
	up := startUpstream(t, nil)
	p := startProxy(t, up.server.URL, nil)
	hc := proxyClient(t)

	oversized := strings.Repeat("x", MaxRequestBodyBytes+1024)
	resp, _, err := send(t, hc, p.BaseURL(), "case-1", 1, oversized)
	if err != nil {
		// Some clients observe the server closing the connection instead of
		// reading the 413; either way the upstream must not be contacted.
		t.Logf("oversized send returned %v", err)
	} else if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
	if up.calls.Load() != 0 {
		t.Errorf("upstream was called %d times with an oversized body, want 0", up.calls.Load())
	}
}

// TestRejectConnectionDoesNotContactUpstream covers GO-AC04: a before_forward
// fault must leave the backend out of the interaction entirely.
func TestRejectConnectionDoesNotContactUpstream(t *testing.T) {
	leaktest.Check(t)
	up := startUpstream(t, nil)
	p := startProxy(t, up.server.URL, []faults.Rule{
		{Attempt: 1, Stage: faults.StageBeforeForward, Action: faults.ActionRejectConnection},
	})
	hc := proxyClient(t)

	if _, _, err := send(t, hc, p.BaseURL(), "case-1", 1, "{}"); err == nil {
		t.Fatal("attempt 1 received a response; the connection should have been destroyed")
	}
	if up.calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0: the request must never be forwarded", up.calls.Load())
	}

	// Attempt 2 has no rule and must pass through normally.
	resp, _, err := send(t, hc, p.BaseURL(), "case-1", 2, "{}")
	if err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("attempt 2 status = %d, want 200", resp.StatusCode)
	}
	if up.calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", up.calls.Load())
	}
	if stats := p.Stats(); stats.Rejected != 1 || stats.FaultsInjected != 1 {
		t.Errorf("stats = %+v, want 1 rejection and 1 injected fault", stats)
	}
}

// TestCloseAfterUpstreamResponseLosesTheResponse covers the central mechanism:
// the upstream completes its work and only then is the client's connection
// destroyed, so the client cannot tell whether the operation was applied.
func TestCloseAfterUpstreamResponseLosesTheResponse(t *testing.T) {
	leaktest.Check(t)
	up := startUpstream(t, nil)
	p := startProxy(t, up.server.URL, []faults.Rule{
		{Attempt: 1, Stage: faults.StageAfterUpstreamResponse, Action: faults.ActionCloseClientConnection},
	})
	hc := proxyClient(t)

	_, _, err := send(t, hc, p.BaseURL(), "case-1", 1, "{}")
	if err == nil {
		t.Fatal("attempt 1 received a response; it should have been lost")
	}
	if up.calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1: the upstream must have run before the drop", up.calls.Load())
	}
	if stats := p.Stats(); stats.Forwarded != 1 || stats.FaultsInjected != 1 || stats.Rejected != 0 {
		t.Errorf("stats = %+v, want 1 forwarded, 1 injected fault, 0 rejected", stats)
	}
}

func TestDelayBeforeForwardDelaysTheUpstreamCall(t *testing.T) {
	leaktest.Check(t)
	up := startUpstream(t, nil)
	p := startProxy(t, up.server.URL, []faults.Rule{
		{Attempt: 1, Stage: faults.StageBeforeForward, Action: faults.ActionDelay, Delay: 120 * time.Millisecond},
	})
	hc := proxyClient(t)

	start := time.Now()
	resp, _, err := send(t, hc, p.BaseURL(), "case-1", 1, "{}")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Errorf("request completed in %s, want at least the injected 120ms delay", elapsed)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200: a delay must not change the response", resp.StatusCode)
	}
}

func TestDelayedClientGivingUpStillLeavesUpstreamApplied(t *testing.T) {
	leaktest.Check(t)
	up := startUpstream(t, nil)
	p := startProxy(t, up.server.URL, []faults.Rule{
		{Attempt: 1, Stage: faults.StageAfterUpstreamResponse, Action: faults.ActionDelay, Delay: 400 * time.Millisecond},
	})

	tr := &http.Transport{DisableCompression: true, ForceAttemptHTTP2: false}
	t.Cleanup(tr.CloseIdleConnections)
	hc := &http.Client{Transport: tr, Timeout: 100 * time.Millisecond}

	if _, _, err := send(t, hc, p.BaseURL(), "case-1", 1, "{}"); err == nil {
		t.Fatal("client received a response despite a delay longer than its timeout")
	}
	if up.calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1: the work happened before the client gave up", up.calls.Load())
	}
}

func TestUpstreamFailureBecomesBadGateway(t *testing.T) {
	leaktest.Check(t)
	up := startUpstream(t, nil)
	p := startProxy(t, up.server.URL, nil)
	hc := proxyClient(t)

	// Stop the upstream so the forwarding attempt cannot connect. A proxy-level
	// failure must be visible as such, not silently turned into a success.
	up.server.Close()

	resp, body, err := send(t, hc, p.BaseURL(), "case-1", 1, "{}")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; body %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Faulttest-Proxy-Error") != "1" {
		t.Error("response is not marked as a proxy-generated error")
	}
}

func TestWaitForQuiescenceWaitsForInFlightUpstreamWork(t *testing.T) {
	leaktest.Check(t)
	release := make(chan struct{})
	up := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{}")
	})
	p := startProxy(t, up.server.URL, nil)
	hc := proxyClient(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = send(t, hc, p.BaseURL(), "case-1", 1, "{}")
	}()

	// Wait until the proxy has actually entered the upstream call.
	deadline := time.Now().Add(2 * time.Second)
	for up.calls.Load() == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("upstream was never called")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Quiescence must not be reported while the upstream is still working.
	shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.WaitForQuiescence(shortCtx, "case-1"); err == nil {
		t.Error("WaitForQuiescence returned while an upstream call was in flight")
	}

	// An unrelated case is quiescent immediately.
	otherCtx, otherCancel := context.WithTimeout(context.Background(), time.Second)
	defer otherCancel()
	if err := p.WaitForQuiescence(otherCtx, "case-other"); err != nil {
		t.Errorf("WaitForQuiescence for an idle case: %v", err)
	}

	close(release)
	<-done

	settleCtx, settleCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer settleCancel()
	if err := p.WaitForQuiescence(settleCtx, "case-1"); err != nil {
		t.Errorf("WaitForQuiescence after completion: %v", err)
	}
}

func TestStartRejectsUnsafeUpstreams(t *testing.T) {
	schedule, err := faults.NewSchedule(nil)
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}

	tests := []struct {
		name     string
		upstream string
	}{
		{"empty", ""},
		{"public IPv4", "http://198.51.100.7:8080"},
		{"private LAN address", "http://192.168.1.10:8080"},
		{"public hostname", "http://example.com"},
		{"hostname that could resolve anywhere", "http://my-service.internal:8080"},
		{"https is out of scope", "https://127.0.0.1:8443"},
		{"non http scheme", "ftp://127.0.0.1:21"},
		{"path is not allowed", "http://127.0.0.1:8080/base"},
		{"no host", "http:///operation"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Start(Options{Upstream: tc.upstream, Schedule: schedule})
			if err == nil {
				_ = p.Close()
				t.Fatalf("Start accepted upstream %q", tc.upstream)
			}
		})
	}
}

func TestStartAcceptsLoopbackUpstreams(t *testing.T) {
	leaktest.Check(t)
	for _, upstream := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		schedule, err := faults.NewSchedule(nil)
		if err != nil {
			t.Fatalf("NewSchedule: %v", err)
		}
		p, err := Start(Options{Upstream: upstream, Schedule: schedule})
		if err != nil {
			t.Errorf("Start(%q): %v", upstream, err)
			continue
		}
		if err := p.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
}

func TestStartRequiresASchedule(t *testing.T) {
	if _, err := Start(Options{Upstream: "http://127.0.0.1:8080"}); err == nil {
		t.Fatal("Start accepted a nil schedule")
	}
}

func TestControlLoopbackOnlyRefusesRemoteAddresses(t *testing.T) {
	if err := controlLoopbackOnly("tcp", "127.0.0.1:8080", nil); err != nil {
		t.Errorf("loopback dial refused: %v", err)
	}
	if err := controlLoopbackOnly("tcp", "[::1]:8080", nil); err != nil {
		t.Errorf("IPv6 loopback dial refused: %v", err)
	}
	if err := controlLoopbackOnly("tcp", "198.51.100.7:8080", nil); err == nil {
		t.Error("dial to a public address was allowed")
	}
	if err := controlLoopbackOnly("tcp", "garbage", nil); err == nil {
		t.Error("malformed dial address was allowed")
	}
}

func TestReadBoundedRejectsOversizedInput(t *testing.T) {
	data, err := readBounded(bytes.NewReader([]byte("hello")), 10)
	if err != nil {
		t.Fatalf("readBounded: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("data = %q, want %q", data, "hello")
	}
	if _, err := readBounded(bytes.NewReader([]byte("hello world")), 5); err == nil {
		t.Error("readBounded accepted input over its limit")
	}
	// Exactly at the limit is allowed.
	if _, err := readBounded(bytes.NewReader([]byte("hello")), 5); err != nil {
		t.Errorf("readBounded at the limit: %v", err)
	}
	if data, err := readBounded(nil, 5); err != nil || data != nil {
		t.Errorf("readBounded(nil) = (%v, %v), want (nil, nil)", data, err)
	}
}
