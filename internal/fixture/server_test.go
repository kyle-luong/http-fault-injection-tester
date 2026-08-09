package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/leaktest"
)

func startFixture(t *testing.T, mode Mode) *Server {
	t.Helper()
	srv, err := Start(mode)
	if err != nil {
		t.Fatalf("Start(%q): %v", mode, err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return srv
}

func testClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{DisableCompression: true, ForceAttemptHTTP2: false}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

// postOperation sends one POST /operation and returns the status and raw body.
func postOperation(t *testing.T, hc *http.Client, baseURL, caseID, key string, delta int) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(OperationRequest{CaseID: caseID, Delta: delta})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/operation", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(IdempotencyKeyHeader, key)
	}

	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("POST /operation: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBodyBytes))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

func TestUnsafeModeAppliesEveryRequest(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeUnsafe)
	hc := testClient(t)
	ctx := context.Background()

	if err := Reset(ctx, hc, srv.BaseURL(), "case-1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// The same idempotency key twice: an unsafe server applies both.
	for i := range 2 {
		if status, body := postOperation(t, hc, srv.BaseURL(), "case-1", "case-1-key", 1); status != http.StatusOK {
			t.Fatalf("request %d: status %d body %s", i, status, body)
		}
	}

	state, err := FetchState(ctx, hc, srv.BaseURL(), "case-1")
	if err != nil {
		t.Fatalf("FetchState: %v", err)
	}
	if state.Counter != 2 {
		t.Errorf("counter = %d, want 2 (unsafe mode must duplicate)", state.Counter)
	}
	if len(state.Operations) != 2 {
		t.Errorf("operations = %v, want 2 entries", state.Operations)
	}
}

func TestSafeModeDeduplicatesByIdempotencyKey(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeSafe)
	hc := testClient(t)
	ctx := context.Background()

	if err := Reset(ctx, hc, srv.BaseURL(), "case-1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	_, first := postOperation(t, hc, srv.BaseURL(), "case-1", "case-1-key", 1)
	status, second := postOperation(t, hc, srv.BaseURL(), "case-1", "case-1-key", 1)
	if status != http.StatusOK {
		t.Fatalf("repeat status %d, want 200", status)
	}

	var firstResp, secondResp OperationResponse
	if err := json.Unmarshal(first, &firstResp); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if err := json.Unmarshal(second, &secondResp); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if secondResp.Counter != firstResp.Counter {
		t.Errorf("repeat returned counter %d, want the original %d", secondResp.Counter, firstResp.Counter)
	}
	if !secondResp.Deduplicated {
		t.Error("repeat was not reported as deduplicated")
	}

	state, err := FetchState(ctx, hc, srv.BaseURL(), "case-1")
	if err != nil {
		t.Fatalf("FetchState: %v", err)
	}
	if state.Counter != 1 {
		t.Errorf("counter = %d, want 1 (safe mode must deduplicate)", state.Counter)
	}
	if len(state.Operations) != 1 {
		t.Errorf("operations = %v, want exactly 1 applied operation", state.Operations)
	}
}

func TestDistinctKeysApplySeparatelyInSafeMode(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeSafe)
	hc := testClient(t)
	ctx := context.Background()

	if err := Reset(ctx, hc, srv.BaseURL(), "case-1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	postOperation(t, hc, srv.BaseURL(), "case-1", "key-a", 1)
	postOperation(t, hc, srv.BaseURL(), "case-1", "key-b", 1)

	state, err := FetchState(ctx, hc, srv.BaseURL(), "case-1")
	if err != nil {
		t.Fatalf("FetchState: %v", err)
	}
	if state.Counter != 2 {
		t.Errorf("counter = %d, want 2: deduplication must not swallow distinct operations", state.Counter)
	}
}

func TestCaseStateIsIsolated(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeUnsafe)
	hc := testClient(t)
	ctx := context.Background()

	for _, id := range []string{"case-1", "case-2"} {
		if err := Reset(ctx, hc, srv.BaseURL(), id); err != nil {
			t.Fatalf("Reset(%s): %v", id, err)
		}
	}
	postOperation(t, hc, srv.BaseURL(), "case-1", "case-1-key", 1)
	postOperation(t, hc, srv.BaseURL(), "case-1", "case-1-key", 1)
	postOperation(t, hc, srv.BaseURL(), "case-2", "case-2-key", 1)

	one, err := FetchState(ctx, hc, srv.BaseURL(), "case-1")
	if err != nil {
		t.Fatalf("FetchState(case-1): %v", err)
	}
	two, err := FetchState(ctx, hc, srv.BaseURL(), "case-2")
	if err != nil {
		t.Fatalf("FetchState(case-2): %v", err)
	}
	if one.Counter != 2 || two.Counter != 1 {
		t.Errorf("counters = (%d, %d), want (2, 1): case state leaked", one.Counter, two.Counter)
	}
	if got := srv.CaseOperationRequests("case-1"); got != 2 {
		t.Errorf("case-1 received %d operation requests, want 2", got)
	}
}

func TestConcurrentCasesDoNotShareState(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeUnsafe)
	hc := testClient(t)
	ctx := context.Background()

	const cases = 32
	var wg sync.WaitGroup
	for i := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("case-%04d", i)
			if err := Reset(ctx, hc, srv.BaseURL(), id); err != nil {
				t.Errorf("Reset(%s): %v", id, err)
				return
			}
			if status, body := postOperation(t, hc, srv.BaseURL(), id, id+"-key", 1); status != http.StatusOK {
				t.Errorf("%s: status %d body %s", id, status, body)
			}
		}()
	}
	wg.Wait()

	for i := range cases {
		id := fmt.Sprintf("case-%04d", i)
		state, err := FetchState(ctx, hc, srv.BaseURL(), id)
		if err != nil {
			t.Fatalf("FetchState(%s): %v", id, err)
		}
		if state.Counter != 1 {
			t.Errorf("%s counter = %d, want 1", id, state.Counter)
		}
	}
	if got := srv.OperationRequests(); got != cases {
		t.Errorf("fixture saw %d operation requests, want %d", got, cases)
	}
}

func TestOperationWithoutResetIsConflict(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeUnsafe)
	hc := testClient(t)

	// A missing reset is a harness defect and must not look like a normal
	// application response.
	status, body := postOperation(t, hc, srv.BaseURL(), "never-reset", "k", 1)
	if status != http.StatusConflict {
		t.Errorf("status = %d, want %d; body %s", status, http.StatusConflict, body)
	}
}

func TestOperationRequiresIdempotencyKey(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeUnsafe)
	hc := testClient(t)

	if err := Reset(context.Background(), hc, srv.BaseURL(), "case-1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if status, body := postOperation(t, hc, srv.BaseURL(), "case-1", "", 1); status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body %s", status, body)
	}
}

func TestStateForUnknownCaseIsNotFound(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeUnsafe)
	hc := testClient(t)

	_, err := FetchState(context.Background(), hc, srv.BaseURL(), "missing")
	if err == nil {
		t.Fatal("FetchState for an unknown case succeeded; want an error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want it to report status 404", err)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeUnsafe)
	hc := testClient(t)

	if err := Reset(context.Background(), hc, srv.BaseURL(), "case-1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// Valid JSON whose padding pushes it past the bound.
	padding := strings.Repeat("x", MaxRequestBodyBytes+1024)
	body := fmt.Sprintf(`{"case_id":"case-1","delta":1,"pad":%q}`, padding)
	req, err := http.NewRequest(http.MethodPost, srv.BaseURL()+"/operation", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(IdempotencyKeyHeader, "case-1-key")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBodyBytes))

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

func TestUnknownJSONFieldsAreRejected(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeUnsafe)
	hc := testClient(t)

	if err := Reset(context.Background(), hc, srv.BaseURL(), "case-1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.BaseURL()+"/operation",
		strings.NewReader(`{"case_id":"case-1","delta":1,"typo":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(IdempotencyKeyHeader, "case-1-key")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBodyBytes))

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown field", resp.StatusCode)
	}
}

func TestInvalidDeltaIsRejected(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeUnsafe)
	hc := testClient(t)

	if err := Reset(context.Background(), hc, srv.BaseURL(), "case-1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, delta := range []int{0, -1, 1001} {
		if status, _ := postOperation(t, hc, srv.BaseURL(), "case-1", "case-1-key", delta); status != http.StatusBadRequest {
			t.Errorf("delta %d: status = %d, want 400", delta, status)
		}
	}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantErr bool
	}{
		{"unsafe-counter", false},
		{"safe-counter", false},
		{"", true},
		{"counter", true},
		{"UNSAFE-COUNTER", true},
	} {
		_, err := ParseMode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseMode(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
	}
}

func TestResetClearsExistingState(t *testing.T) {
	leaktest.Check(t)
	srv := startFixture(t, ModeUnsafe)
	hc := testClient(t)
	ctx := context.Background()

	if err := Reset(ctx, hc, srv.BaseURL(), "case-1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	postOperation(t, hc, srv.BaseURL(), "case-1", "case-1-key", 1)
	if err := Reset(ctx, hc, srv.BaseURL(), "case-1"); err != nil {
		t.Fatalf("second Reset: %v", err)
	}

	state, err := FetchState(ctx, hc, srv.BaseURL(), "case-1")
	if err != nil {
		t.Fatalf("FetchState: %v", err)
	}
	if state.Counter != 0 || len(state.Operations) != 0 {
		t.Errorf("state after reset = %+v, want a zeroed case", state)
	}
}
