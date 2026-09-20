// Package integration holds the end-to-end tests that decide whether the tool
// actually does what the PRD claims.
//
// These tests load the scenario files shipped in scenarios/, so the documented
// demo commands and the test suite cannot drift apart. Each test names the PRD
// acceptance criterion it evidences.
package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/client"
	"github.com/kyle-luong/http-fault-injection-tester/internal/fixture"
	"github.com/kyle-luong/http-fault-injection-tester/internal/leaktest"
	"github.com/kyle-luong/http-fault-injection-tester/internal/runner"
	"github.com/kyle-luong/http-fault-injection-tester/internal/scenario"
	"github.com/kyle-luong/http-fault-injection-tester/internal/verify"
)

func loadScenario(t *testing.T, name string) *scenario.Scenario {
	t.Helper()
	sc, err := scenario.Load(filepath.Join("..", "..", "scenarios", name))
	if err != nil {
		t.Fatalf("load scenario %s: %v", name, err)
	}
	return sc
}

func setupEnv(t *testing.T, sc *scenario.Scenario, opts runner.Options) *runner.Env {
	t.Helper()
	env, err := runner.Setup(sc, opts)
	if err != nil {
		t.Fatalf("runner.Setup: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Close(); err != nil {
			t.Errorf("env.Close: %v", err)
		}
	})
	return env
}

func runOne(t *testing.T, env *runner.Env, caseID string) runner.Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return env.RunCase(ctx, caseID)
}

// TestNoFaultBaseline covers GO-AC01: with no faults injected, both fixture
// modes apply the operation exactly once and neither produces a false failure.
func TestNoFaultBaseline(t *testing.T) {
	for _, mode := range []string{"unsafe-counter", "safe-counter"} {
		t.Run(mode, func(t *testing.T) {
			leaktest.Check(t)
			sc := loadScenario(t, "no-fault-baseline.json")
			sc.Fixture = mode
			env := setupEnv(t, sc, runner.Options{})

			outcome := runOne(t, env, "case-0001")

			if outcome.Verdict() != verify.VerdictPass {
				t.Fatalf("verdict = %q, want %q: %s", outcome.Verdict(), verify.VerdictPass,
					outcome.Verification.Detail)
			}
			if got := outcome.Verification.Observed.Counter; got != 1 {
				t.Errorf("counter = %d, want 1", got)
			}
			if got := len(outcome.Attempts); got != 1 {
				t.Errorf("attempts = %d, want 1 when no fault is injected", got)
			}
			if got := len(outcome.Decisions); got != 0 {
				t.Errorf("injected %d faults, want 0", got)
			}
			if got := env.Fixture().CaseOperationRequests("case-0001"); got != 1 {
				t.Errorf("fixture received %d operation requests, want 1", got)
			}
		})
	}
}

// TestLostResponseDuplicatesOnUnsafeFixture covers GO-AC02 and GO-T03: the
// upstream completes the operation, the response is destroyed in transit, the
// client retries with the same idempotency key, and the unsafe fixture applies
// the operation a second time. The failure is detected from observed state --
// the client's final HTTP status is 200, which on its own looks fine.
func TestLostResponseDuplicatesOnUnsafeFixture(t *testing.T) {
	leaktest.Check(t)
	sc := loadScenario(t, "lost-response.json")
	env := setupEnv(t, sc, runner.Options{})

	outcome := runOne(t, env, "case-0001")

	if outcome.Verdict() != verify.VerdictFail {
		t.Fatalf("verdict = %q, want %q: %s", outcome.Verdict(), verify.VerdictFail,
			outcome.Verification.Detail)
	}

	state := outcome.Verification.Observed
	if state.Counter != 2 {
		t.Errorf("counter = %d, want 2: the retry should have duplicated the side effect", state.Counter)
	}
	if len(state.Operations) != 2 {
		t.Errorf("operations = %v, want 2 applied operations", state.Operations)
	} else if state.Operations[0] != state.Operations[1] {
		t.Errorf("operations = %v, want the same idempotency key applied twice", state.Operations)
	}

	if len(outcome.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(outcome.Attempts))
	}
	first, second := outcome.Attempts[0], outcome.Attempts[1]
	if first.TransportError == "" {
		t.Error("attempt 1 reported a response; the scenario destroys it in transit")
	}
	if first.StatusCode != 0 {
		t.Errorf("attempt 1 status = %d, want 0 (no response reached the client)", first.StatusCode)
	}
	if second.StatusCode != 200 {
		t.Errorf("attempt 2 status = %d, want 200", second.StatusCode)
	}
	if first.IdempotencyKey != second.IdempotencyKey {
		t.Errorf("attempt keys %q and %q differ; the retry must reuse the key",
			first.IdempotencyKey, second.IdempotencyKey)
	}

	// The backend really was reached twice: the duplicate is a server-side
	// effect, not a reporting artefact.
	if got := env.Fixture().CaseOperationRequests("case-0001"); got != 2 {
		t.Errorf("fixture received %d operation requests, want 2", got)
	}

	if len(outcome.Decisions) != 1 {
		t.Fatalf("recorded %d fault decisions, want 1", len(outcome.Decisions))
	}
	decision := outcome.Decisions[0]
	if decision.Attempt != 1 || decision.Stage != "after_upstream_response" ||
		decision.Action != "close_client_connection" {
		t.Errorf("decision = %+v, want the close after the upstream response on attempt 1", decision)
	}
}

// TestLostResponseAppliesOnceOnSafeFixture covers GO-AC03 and GO-T04: the same
// injected fault against an idempotent fixture yields exactly one effect.
func TestLostResponseAppliesOnceOnSafeFixture(t *testing.T) {
	leaktest.Check(t)
	sc := loadScenario(t, "lost-response-safe.json")
	env := setupEnv(t, sc, runner.Options{})

	outcome := runOne(t, env, "case-0001")

	if outcome.Verdict() != verify.VerdictPass {
		t.Fatalf("verdict = %q, want %q: %s", outcome.Verdict(), verify.VerdictPass,
			outcome.Verification.Detail)
	}
	state := outcome.Verification.Observed
	if state.Counter != 1 {
		t.Errorf("counter = %d, want 1: idempotency should have absorbed the retry", state.Counter)
	}
	if len(state.Operations) != 1 {
		t.Errorf("operations = %v, want exactly 1 applied operation", state.Operations)
	}

	// The retry did reach the backend; the backend chose not to apply it twice.
	if got := env.Fixture().CaseOperationRequests("case-0001"); got != 2 {
		t.Errorf("fixture received %d operation requests, want 2", got)
	}
	if len(outcome.Attempts) != 2 {
		t.Errorf("attempts = %d, want 2", len(outcome.Attempts))
	}
}

// TestSameScenarioSeparatesSafeFromUnsafe is the headline demonstration: one
// scenario, two implementations, opposite verdicts.
func TestSameScenarioSeparatesSafeFromUnsafe(t *testing.T) {
	leaktest.Check(t)
	base := loadScenario(t, "lost-response.json")

	verdicts := make(map[string]verify.Verdict)
	counters := make(map[string]int)
	for _, mode := range []string{"unsafe-counter", "safe-counter"} {
		sc := *base
		sc.Fixture = mode
		env := setupEnv(t, &sc, runner.Options{})
		outcome := runOne(t, env, "case-0001")
		verdicts[mode] = outcome.Verdict()
		counters[mode] = outcome.Verification.Observed.Counter
	}

	if verdicts["unsafe-counter"] != verify.VerdictFail {
		t.Errorf("unsafe fixture verdict = %q, want %q", verdicts["unsafe-counter"], verify.VerdictFail)
	}
	if verdicts["safe-counter"] != verify.VerdictPass {
		t.Errorf("safe fixture verdict = %q, want %q", verdicts["safe-counter"], verify.VerdictPass)
	}
	if counters["unsafe-counter"] != 2 || counters["safe-counter"] != 1 {
		t.Errorf("counters = %v, want unsafe 2 and safe 1", counters)
	}
}

// TestRejectBeforeForwardLeavesBackendUntouched covers GO-AC04 and GO-T05: a
// failure before forwarding has no side effect, so the retry is the only
// operation the backend ever performs.
func TestRejectBeforeForwardLeavesBackendUntouched(t *testing.T) {
	leaktest.Check(t)
	sc := loadScenario(t, "reject-before-forward.json")
	env := setupEnv(t, sc, runner.Options{})

	outcome := runOne(t, env, "case-0001")

	if outcome.Verdict() != verify.VerdictPass {
		t.Fatalf("verdict = %q, want %q: %s", outcome.Verdict(), verify.VerdictPass,
			outcome.Verification.Detail)
	}
	if got := outcome.Verification.Observed.Counter; got != 1 {
		t.Errorf("counter = %d, want 1 even on the unsafe fixture: the first attempt never arrived", got)
	}
	// Exactly one request reached the backend, proving the control case.
	if got := env.Fixture().CaseOperationRequests("case-0001"); got != 1 {
		t.Errorf("fixture received %d operation requests, want 1", got)
	}
	if got := env.Proxy().Stats().Forwarded; got != 1 {
		t.Errorf("proxy forwarded %d requests, want 1", got)
	}
}

// TestDelayedResponseBeyondClientTimeout covers GO-AC05 and GO-T06: the client
// times out while the response is stalled, and the verdict still comes from
// state read after settlement.
func TestDelayedResponseBeyondClientTimeout(t *testing.T) {
	leaktest.Check(t)
	sc := loadScenario(t, "delayed-response.json")
	env := setupEnv(t, sc, runner.Options{})

	outcome := runOne(t, env, "case-0001")

	if outcome.Verdict() != verify.VerdictPass {
		t.Fatalf("verdict = %q, want %q: %s", outcome.Verdict(), verify.VerdictPass,
			outcome.Verification.Detail)
	}
	if len(outcome.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2: the first should have timed out", len(outcome.Attempts))
	}
	if outcome.Attempts[0].TransportError == "" {
		t.Error("attempt 1 did not time out despite a delay longer than its timeout")
	}
	if got := outcome.Verification.Observed.Counter; got != 1 {
		t.Errorf("counter = %d, want 1 on the safe fixture", got)
	}
	// Both attempts reached the backend; only one was applied.
	if got := env.Fixture().CaseOperationRequests("case-0001"); got != 2 {
		t.Errorf("fixture received %d operation requests, want 2", got)
	}
}

// TestProxiedAndDirectAgreeOnFinalState covers GO-T02: with no faults, routing
// through the proxy must not change the application's final state.
func TestProxiedAndDirectAgreeOnFinalState(t *testing.T) {
	leaktest.Check(t)
	sc := loadScenario(t, "no-fault-baseline.json")
	env := setupEnv(t, sc, runner.Options{})

	proxied := runOne(t, env, "case-proxied")
	if proxied.Verdict() != verify.VerdictPass {
		t.Fatalf("proxied verdict = %q: %s", proxied.Verdict(), proxied.Verification.Detail)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hc := client.NewHTTPClient()
	defer client.CloseIdleConnections(hc)

	fixtureURL := env.Fixture().BaseURL()
	if err := fixture.Reset(ctx, hc, fixtureURL, "case-direct"); err != nil {
		t.Fatalf("reset direct case: %v", err)
	}
	policy := client.Policy{
		Method:              sc.Client.Method,
		Path:                sc.Client.Path,
		Delta:               sc.Delta(),
		MaxAttempts:         sc.Client.MaxAttempts,
		ReuseIdempotencyKey: sc.Client.ReuseIdempotencyKey,
	}
	if _, err := client.Execute(ctx, hc, fixtureURL, "case-direct", policy); err != nil {
		t.Fatalf("direct execute: %v", err)
	}
	direct, err := fixture.FetchState(ctx, hc, fixtureURL, "case-direct")
	if err != nil {
		t.Fatalf("fetch direct state: %v", err)
	}

	if direct.Counter != proxied.Verification.Observed.Counter {
		t.Errorf("direct counter %d != proxied counter %d; the proxy changed application behaviour",
			direct.Counter, proxied.Verification.Observed.Counter)
	}
	if len(direct.Operations) != len(proxied.Verification.Observed.Operations) {
		t.Errorf("direct applied %d operations, proxied applied %d",
			len(direct.Operations), len(proxied.Verification.Observed.Operations))
	}
}

// TestConcurrentCasesAreIsolated covers the isolation half of GO-AC07: every
// case keeps its own state and reaches the same verdict under concurrency. The
// bounded worker pool that will schedule these cases is GO-04; this test proves
// the shared infrastructure is already safe for it, and it runs under -race.
func TestConcurrentCasesAreIsolated(t *testing.T) {
	for _, cases := range []int{1, 8, 32} {
		t.Run(fmt.Sprintf("cases=%d", cases), func(t *testing.T) {
			leaktest.Check(t)
			sc := loadScenario(t, "lost-response.json")
			env := setupEnv(t, sc, runner.Options{})

			outcomes := make([]runner.Outcome, cases)
			var wg sync.WaitGroup
			for i := range cases {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					outcomes[i] = env.RunCase(ctx, fmt.Sprintf("case-%04d", i))
				}()
			}
			wg.Wait()

			for i, outcome := range outcomes {
				wantID := fmt.Sprintf("case-%04d", i)
				if outcome.CaseID != wantID {
					t.Errorf("outcome %d has case ID %q, want %q", i, outcome.CaseID, wantID)
				}
				if outcome.Verdict() != verify.VerdictFail {
					t.Errorf("%s verdict = %q, want %q: %s",
						wantID, outcome.Verdict(), verify.VerdictFail, outcome.Verification.Detail)
					continue
				}
				if got := outcome.Verification.Observed.Counter; got != 2 {
					t.Errorf("%s counter = %d, want 2: case state was not isolated", wantID, got)
				}
				if len(outcome.Decisions) != 1 {
					t.Errorf("%s recorded %d decisions, want 1 belonging to it", wantID, len(outcome.Decisions))
				}
				for _, d := range outcome.Decisions {
					if d.CaseID != wantID {
						t.Errorf("%s was attributed a decision from %s", wantID, d.CaseID)
					}
				}
			}
		})
	}
}

// TestHarnessFailureIsNotAnInvariantFailure covers GO-AC10: when the harness
// cannot reach the application, it must say so rather than report a finding.
func TestHarnessFailureIsNotAnInvariantFailure(t *testing.T) {
	leaktest.Check(t)
	sc := loadScenario(t, "lost-response.json")
	env, err := runner.Setup(sc, runner.Options{})
	if err != nil {
		t.Fatalf("runner.Setup: %v", err)
	}
	defer func() { _ = env.Close() }()

	// Take the application away; the harness can no longer establish state.
	if err := env.Fixture().Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	outcome := runOne(t, env, "case-0001")

	if outcome.Verdict() != verify.VerdictHarnessError {
		t.Fatalf("verdict = %q, want %q: %s", outcome.Verdict(), verify.VerdictHarnessError,
			outcome.Verification.Detail)
	}
	if outcome.Verification.Detail == "" {
		t.Error("harness error carries no explanation")
	}
}

// TestCaseDeadlineIsRespected covers GO-AC08: a response stalled past the case
// deadline is reported as indeterminate, not as an application failure, and
// everything the attempt started is cleaned up.
func TestCaseDeadlineIsRespected(t *testing.T) {
	leaktest.Check(t)
	sc := loadScenario(t, "delayed-response.json")
	// Stall for longer than the case deadline, and remove the per-attempt
	// timeout so the case deadline is the only thing that can fire.
	delay := 5000
	sc.Faults[0].DelayMS = &delay
	sc.Client.TimeoutMS = nil
	env := setupEnv(t, sc, runner.Options{CaseTimeout: 400 * time.Millisecond})

	started := time.Now()
	outcome := runOne(t, env, "case-0001")
	elapsed := time.Since(started)

	if outcome.Verdict() == verify.VerdictFail {
		t.Fatalf("verdict = %q; a blown deadline must not be reported as an application failure: %s",
			outcome.Verdict(), outcome.Verification.Detail)
	}
	if outcome.Verdict() != verify.VerdictInconclusive {
		t.Errorf("verdict = %q, want %q: %s", outcome.Verdict(), verify.VerdictInconclusive,
			outcome.Verification.Detail)
	}
	// The deadline, not the 5s stall, must bound the case.
	if elapsed > 4*time.Second {
		t.Errorf("case took %s; the deadline should have ended it much sooner", elapsed)
	}
}

// TestHundredCasesAtTwoParallelisms covers GO-T09: 100 independent cases at
// parallelism 1 and 16, with no shared-state contamination.
//
// The backend-side request count is the sharpest assertion here. Each case
// must apply exactly two operations, one lost and one retried, so the fixture
// must see exactly 200 operation requests. A contaminated case, a replayed
// request or a case that leaked into another's state all break that number.
func TestHundredCasesAtTwoParallelisms(t *testing.T) {
	for _, parallel := range []int{1, 16} {
		t.Run(fmt.Sprintf("parallel=%d", parallel), func(t *testing.T) {
			leaktest.Check(t)
			sc := loadScenario(t, "lost-response.json")
			env := setupEnv(t, sc, runner.Options{})

			const cases = 100
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			summary, err := env.Run(ctx, runner.RunSpec{Runs: cases, Parallel: parallel})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if summary.Executed != cases {
				t.Fatalf("executed %d cases, want %d", summary.Executed, cases)
			}
			if summary.Failed != cases {
				t.Fatalf("failed=%d passed=%d inconclusive=%d infrastructure=%d, want %d failures",
					summary.Failed, summary.Passed, summary.Inconclusive,
					summary.HarnessErrors, cases)
			}
			if summary.MaxConcurrentCases > parallel {
				t.Errorf("peak concurrency %d exceeded the bound of %d",
					summary.MaxConcurrentCases, parallel)
			}
			if summary.DroppedDecisions != 0 {
				t.Errorf("%d fault decisions were dropped; the recorded history is incomplete",
					summary.DroppedDecisions)
			}

			for _, o := range summary.Outcomes {
				if got := o.Verification.Observed.Counter; got != 2 {
					t.Errorf("%s counter = %d, want 2: case state was not isolated", o.CaseID, got)
				}
				if got := len(o.Verification.Observed.Operations); got != 2 {
					t.Errorf("%s applied %d operations, want 2", o.CaseID, got)
				}
				if got := env.Fixture().CaseOperationRequests(o.CaseID); got != 2 {
					t.Errorf("%s reached the backend %d times, want 2", o.CaseID, got)
				}
			}

			if got := env.Fixture().OperationRequests(); got != 2*cases {
				t.Errorf("fixture handled %d operation requests in total, want %d",
					got, 2*cases)
			}
		})
	}
}
