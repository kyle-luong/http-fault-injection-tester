package integration

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/client"
	"github.com/kyle-luong/http-fault-injection-tester/internal/faults"
	"github.com/kyle-luong/http-fault-injection-tester/internal/leaktest"
	"github.com/kyle-luong/http-fault-injection-tester/internal/proxy"
	"github.com/kyle-luong/http-fault-injection-tester/internal/runner"
	"github.com/kyle-luong/http-fault-injection-tester/internal/verify"
)

// TestStateIsNotReadWhileTheProxyStillHoldsARequest covers the GO-T13
// "slow oracle" boundary, and the settlement invariant underneath it: a case
// may not be scored while this proxy is still holding a request that could
// change its state.
//
// The stalled request below is stalled *before* the upstream is contacted, so
// no upstream call is in flight -- only a handler that is still going to make
// one. Settlement used to bracket the upstream round trip alone and would
// report the case settled, letting the oracle read a counter that was about to
// change. The budget here is far too small to wait the stall out, so the case
// must come back indeterminate rather than with a verdict in either direction.
func TestStateIsNotReadWhileTheProxyStillHoldsARequest(t *testing.T) {
	leaktest.Check(t)

	// Attempt 7 is one the client never sends, so the rule matches only the
	// request this test makes by hand. Rules are passed directly because no
	// scenario could declare a fault for an attempt beyond its own policy.
	sc := loadScenario(t, "no-fault-baseline.json")
	env := setupEnv(t, sc, runner.Options{
		Rules: []faults.Rule{{
			Attempt: 7,
			Stage:   faults.StageBeforeForward,
			Action:  faults.ActionDelay,
			Delay:   2 * time.Second,
		}},
		CaseTimeout:   5 * time.Second,
		SettleTimeout: 50 * time.Millisecond,
	})

	const caseID = "case-0001"
	stallCtx, cancelStall := context.WithCancel(context.Background())
	var stalled sync.WaitGroup
	stalled.Add(1)
	go func() {
		defer stalled.Done()
		hc := client.NewHTTPClient()
		defer client.CloseIdleConnections(hc)
		req, err := http.NewRequestWithContext(stallCtx, http.MethodPost,
			env.Proxy().BaseURL()+"/operation", strings.NewReader(`{"case_id":"`+caseID+`","delta":1}`))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(proxy.HeaderCaseID, caseID)
		req.Header.Set(proxy.HeaderAttempt, strconv.Itoa(7))
		resp, err := hc.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	t.Cleanup(func() {
		cancelStall()
		stalled.Wait()
	})

	// Let the stalled request reach the proxy and start its delay.
	deadline := time.Now().Add(2 * time.Second)
	for env.Proxy().Stats().Requests == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if env.Proxy().Stats().Requests == 0 {
		t.Fatal("the stalled request never reached the proxy")
	}

	outcome := runOne(t, env, caseID)

	if outcome.Verdict() != verify.VerdictInconclusive {
		t.Fatalf("verdict = %q, want %q: the proxy was still holding a request for this case: %s",
			outcome.Verdict(), verify.VerdictInconclusive, outcome.Verification.Detail)
	}
	if !strings.Contains(outcome.Verification.Detail, "settle") {
		t.Errorf("detail does not say which deadline ended the case: %q", outcome.Verification.Detail)
	}

	// Without this the test would also pass for a harness that reported every
	// case indeterminate: the same scenario, with nothing held, reaches a
	// verdict.
	clean := runOne(t, setupEnv(t, sc, runner.Options{}), "case-0002")
	if clean.Verdict() != verify.VerdictPass {
		t.Errorf("an unobstructed case came back %q, want pass: %s",
			clean.Verdict(), clean.Verification.Detail)
	}
}
