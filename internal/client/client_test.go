package client

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/fixture"
	"github.com/kyle-luong/http-fault-injection-tester/internal/proxy"
)

func basePolicy() Policy {
	return Policy{
		Method:              http.MethodPost,
		Path:                "/operation",
		Delta:               1,
		MaxAttempts:         1,
		ReuseIdempotencyKey: true,
	}
}

func TestPolicyValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Policy)
		wantErr bool
	}{
		{name: "baseline", mutate: func(*Policy) {}},
		{name: "non POST method", mutate: func(p *Policy) { p.Method = http.MethodPut }, wantErr: true},
		{name: "relative path", mutate: func(p *Policy) { p.Path = "operation" }, wantErr: true},
		{name: "empty path", mutate: func(p *Policy) { p.Path = "" }, wantErr: true},
		{name: "zero attempts", mutate: func(p *Policy) { p.MaxAttempts = 0 }, wantErr: true},
		{name: "attempts past the bound", mutate: func(p *Policy) { p.MaxAttempts = MaxAttemptsLimit + 1 }, wantErr: true},
		{name: "zero delta", mutate: func(p *Policy) { p.Delta = 0 }, wantErr: true},
		{name: "delta past the bound", mutate: func(p *Policy) { p.Delta = 1001 }, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := basePolicy()
			tc.mutate(&p)
			if err := p.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestIdempotencyKeyDerivation(t *testing.T) {
	// Reuse is what makes a retry a retry of the same logical operation.
	if got := idempotencyKey("case-1", 1, true); got != "case-1-key" {
		t.Errorf("reused key for attempt 1 = %q, want %q", got, "case-1-key")
	}
	if got := idempotencyKey("case-1", 2, true); got != "case-1-key" {
		t.Errorf("reused key for attempt 2 = %q, want it unchanged", got)
	}
	// Not reusing models a client that presents each retry as new work.
	first := idempotencyKey("case-1", 1, false)
	second := idempotencyKey("case-1", 2, false)
	if first == second {
		t.Errorf("fresh keys for attempts 1 and 2 are both %q, want them to differ", first)
	}
}

// stubUpstream records every request and replies with a fixture-shaped body.
type stubUpstream struct {
	server  *httptest.Server
	calls   atomic.Int64
	keys    []string
	caseIDs []string
	attempt []string
}

func startStub(t *testing.T, status func(call int64) int) *stubUpstream {
	t.Helper()
	stub := &stubUpstream{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := stub.calls.Add(1)
		stub.keys = append(stub.keys, r.Header.Get(fixture.IdempotencyKeyHeader))
		stub.caseIDs = append(stub.caseIDs, r.Header.Get(proxy.HeaderCaseID))
		stub.attempt = append(stub.attempt, r.Header.Get(proxy.HeaderAttempt))

		code := http.StatusOK
		if status != nil {
			code = status(call)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if code >= 200 && code <= 299 {
			_, _ = w.Write([]byte(`{"case_id":"case-1","operation_id":"case-1-key","counter":1,"deduplicated":false}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func TestExecuteSendsControlHeadersAndStopsOnSuccess(t *testing.T) {
	stub := startStub(t, nil)
	hc := NewHTTPClient()
	defer CloseIdleConnections(hc)

	p := basePolicy()
	p.MaxAttempts = 2
	p.RetryOnTransportError = true

	result, err := Execute(context.Background(), hc, stub.server.URL, "case-1", p)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1: a 2xx must end the sequence", len(result.Attempts))
	}
	if !result.Succeeded() {
		t.Error("Succeeded() = false after a 200")
	}
	if stub.calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", stub.calls.Load())
	}
	if stub.caseIDs[0] != "case-1" {
		t.Errorf("%s = %q, want case-1", proxy.HeaderCaseID, stub.caseIDs[0])
	}
	if stub.attempt[0] != "1" {
		t.Errorf("%s = %q, want 1", proxy.HeaderAttempt, stub.attempt[0])
	}
	if stub.keys[0] != "case-1-key" {
		t.Errorf("%s = %q, want case-1-key", fixture.IdempotencyKeyHeader, stub.keys[0])
	}

	last, ok := result.LastAttempt()
	if !ok || last.Response == nil || last.Response.Counter != 1 {
		t.Errorf("last attempt = %+v, want a decoded response with counter 1", last)
	}
}

func TestExecuteDoesNotRetryAnHTTPErrorResponse(t *testing.T) {
	stub := startStub(t, func(int64) int { return http.StatusInternalServerError })
	hc := NewHTTPClient()
	defer CloseIdleConnections(hc)

	p := basePolicy()
	p.MaxAttempts = 3
	p.RetryOnTransportError = true

	result, err := Execute(context.Background(), hc, stub.server.URL, "case-1", p)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// A complete 500 is unambiguous, so v1 does not retry it.
	if len(result.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1", len(result.Attempts))
	}
	if result.Succeeded() {
		t.Error("Succeeded() = true after a 500")
	}
	if got := result.Attempts[0].StatusCode; got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
	if !result.Attempts[0].Failed() {
		t.Error("Failed() = false for a 500 response")
	}
}

func TestExecuteRetriesTransportErrorsUpToTheLimit(t *testing.T) {
	// A closed server produces a connection error on every attempt.
	stub := startStub(t, nil)
	addr := stub.server.URL
	stub.server.Close()

	hc := NewHTTPClient()
	defer CloseIdleConnections(hc)

	p := basePolicy()
	p.MaxAttempts = 3
	p.RetryOnTransportError = true

	result, err := Execute(context.Background(), hc, addr, "case-1", p)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Attempts) != 3 {
		t.Fatalf("attempts = %d, want 3", len(result.Attempts))
	}
	for i, a := range result.Attempts {
		if a.TransportError == "" {
			t.Errorf("attempt %d reported no transport error", i+1)
		}
		if a.Number != i+1 {
			t.Errorf("attempt %d numbered %d", i+1, a.Number)
		}
	}
	if result.Succeeded() {
		t.Error("Succeeded() = true with no successful attempt")
	}
}

func TestExecuteHonoursRetryOnTransportErrorFalse(t *testing.T) {
	stub := startStub(t, nil)
	addr := stub.server.URL
	stub.server.Close()

	hc := NewHTTPClient()
	defer CloseIdleConnections(hc)

	p := basePolicy()
	p.MaxAttempts = 3
	p.RetryOnTransportError = false

	result, err := Execute(context.Background(), hc, addr, "case-1", p)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1 when retries are disabled", len(result.Attempts))
	}
}

func TestExecuteAppliesThePerAttemptTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	// Order matters: the blocked handler must be released before Close waits
	// for it, so this defer is registered last and therefore runs first.
	defer server.Close()
	defer close(release)

	hc := NewHTTPClient()
	defer CloseIdleConnections(hc)

	p := basePolicy()
	p.AttemptTimeout = 80 * time.Millisecond

	start := time.Now()
	result, err := Execute(context.Background(), hc, server.URL, "case-1", p)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Execute took %s; the per-attempt timeout did not fire", elapsed)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].TransportError == "" {
		t.Errorf("attempts = %+v, want one timed-out attempt", result.Attempts)
	}
}

func TestExecuteRejectsAnInvalidPolicy(t *testing.T) {
	hc := NewHTTPClient()
	defer CloseIdleConnections(hc)

	p := basePolicy()
	p.Method = http.MethodGet
	if _, err := Execute(context.Background(), hc, "http://127.0.0.1:1", "case-1", p); err == nil {
		t.Fatal("Execute accepted an invalid policy")
	}
}

func TestExecuteStopsOnACancelledContext(t *testing.T) {
	stub := startStub(t, nil)
	hc := NewHTTPClient()
	defer CloseIdleConnections(hc)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := basePolicy()
	_, err := Execute(ctx, hc, stub.server.URL, "case-1", p)
	if err == nil {
		t.Fatal("Execute ignored a cancelled context")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error = %v, want it to report cancellation", err)
	}
}

func TestNewHTTPClientRefusesRedirects(t *testing.T) {
	hc := NewHTTPClient()
	defer CloseIdleConnections(hc)
	if hc.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil; a redirect could hide which endpoint ran")
	}
	if err := hc.CheckRedirect(nil, nil); err == nil {
		t.Error("CheckRedirect allowed a redirect")
	}
}

// resettingServer is a hand-rolled HTTP/1.1 server that answers the first
// request on a connection normally and then, for the next request on that same
// connection, reads the request in full and resets the connection instead of
// responding.
//
// That models the situation this harness creates on purpose: the operation has
// been applied and only the response is lost. It is also exactly the situation
// in which http.Transport replays a request it believes to be idempotent, so a
// stub server is used rather than httptest, which cannot destroy a connection.
type resettingServer struct {
	baseURL  string
	requests atomic.Int64
}

func startResettingServer(t *testing.T) *resettingServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &resettingServer{baseURL: "http://" + ln.Addr().String()}

	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				srv.serve(conn)
			}()
		}
	}()

	// Waiting on the WaitGroup here proves no goroutine outlives the test.
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return srv
}

func (s *resettingServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	for n := 1; ; n++ {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		s.requests.Add(1)

		if n > 1 {
			// The request has been read in full, so a real server would have
			// applied it by now. Reset so the response never arrives.
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			return
		}

		body := `{"case_id":"case-0001","operation_id":"case-0001-key","counter":1,"deduplicated":false}`
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: "+
			strconv.Itoa(len(body))+"\r\n\r\n"+body)
	}
}

// TestExecuteSendsOneWireRequestPerAttempt pins one logical attempt to exactly
// one request on the wire.
//
// http.Transport silently replays a request on a reused connection when it
// considers it idempotent, and it treats any request carrying an
// Idempotency-Key as such. This harness always sends that header, so without
// the guard in sendOnce the stdlib would re-send an attempt whose response was
// destroyed on purpose: the side effect would be applied twice, one attempt
// number would cover several requests, and the attempt could even be recorded
// as a success.
func TestExecuteSendsOneWireRequestPerAttempt(t *testing.T) {
	srv := startResettingServer(t)
	hc := NewHTTPClient()
	t.Cleanup(func() { CloseIdleConnections(hc) })

	// Warm a pooled connection so the attempt below runs on a reused one,
	// which is the only case in which the transport would replay.
	warm, err := hc.Post(srv.baseURL+"/operation", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("warmup request: %v", err)
	}
	_, _ = io.Copy(io.Discard, warm.Body)
	_ = warm.Body.Close()

	before := srv.requests.Load()

	result, err := Execute(context.Background(), hc, srv.baseURL, "case-0001", basePolicy())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := srv.requests.Load() - before; got != 1 {
		t.Errorf("server saw %d request(s) for one attempt, want 1: the transport replayed a "+
			"request whose side effect had already been applied", got)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("recorded %d attempts, want 1", len(result.Attempts))
	}
	if result.Attempts[0].TransportError == "" {
		t.Errorf("attempt recorded status %d and no transport error: a destroyed response was "+
			"reported as a result", result.Attempts[0].StatusCode)
	}
	if result.Succeeded() {
		t.Error("Execute reported success for an attempt whose response never arrived")
	}
}

// TestExecuteTreatsAnUndecodableResponseAsNoResponse covers the GO-T13
// boundary cases where a response arrives but says nothing usable: an empty
// 200 body, and a 200 body that is not the fixture's contract.
//
// These must look like "no response", not like success. A client that counted
// an empty 200 as a completed operation would let the oracle be bypassed by a
// malformed reply.
func TestExecuteTreatsAnUndecodableResponseAsNoResponse(t *testing.T) {
	cases := map[string]string{
		"empty body":     "",
		"not json":       "nope",
		"foreign object": `{"unexpected":1}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()

			hc := NewHTTPClient()
			defer CloseIdleConnections(hc)

			result, err := Execute(context.Background(), hc, server.URL, "case-1", basePolicy())
			if err != nil {
				t.Fatalf("Execute returned a harness error for an application response: %v", err)
			}
			if len(result.Attempts) != 1 {
				t.Fatalf("attempts = %d, want 1", len(result.Attempts))
			}
			attempt := result.Attempts[0]
			if attempt.TransportError == "" {
				t.Error("an undecodable response was not recorded as a failed attempt")
			}
			if attempt.Response != nil {
				t.Errorf("Response = %+v, want nothing decoded", attempt.Response)
			}
			if result.Succeeded() {
				t.Error("Succeeded() is true for a response that could not be read")
			}
			if !attempt.Failed() {
				t.Error("Failed() is false for a response that could not be read")
			}
		})
	}
}
