package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/leaktest"
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

func setupEnv(t *testing.T, name string) *Env {
	t.Helper()
	return setupEnvFromScenario(t, loadScenario(t, name))
}

func setupEnvFromScenario(t *testing.T, sc *scenario.Scenario) *Env {
	t.Helper()
	env, err := Setup(sc, Options{})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Close(); err != nil {
			t.Errorf("Env.Close: %v", err)
		}
	})
	return env
}

// TestRunSpecBounds covers the bounds half of GO-AC11: a run size the harness
// cannot support is refused before anything starts.
func TestRunSpecBounds(t *testing.T) {
	tests := []struct {
		name         string
		spec         RunSpec
		wantErr      bool
		wantRuns     int
		wantParallel int
	}{
		{"zero selects the defaults", RunSpec{}, false, DefaultRuns, DefaultParallel},
		{"negative runs", RunSpec{Runs: -1}, true, 0, 0},
		{"runs past the bound", RunSpec{Runs: MaxRuns + 1}, true, 0, 0},
		{"runs at the bound", RunSpec{Runs: MaxRuns}, false, MaxRuns, DefaultParallel},
		{"negative parallelism", RunSpec{Parallel: -1}, true, 0, 0},
		{"parallelism past the bound", RunSpec{Parallel: MaxParallel + 1}, true, 0, 0},
		{"parallelism at the bound", RunSpec{Parallel: MaxParallel}, false, DefaultRuns, MaxParallel},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			err := spec.normalize()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalize() accepted %+v", tc.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalize() rejected %+v: %v", tc.spec, err)
			}
			if spec.Runs != tc.wantRuns || spec.Parallel != tc.wantParallel {
				t.Errorf("normalized to runs=%d parallel=%d, want %d and %d",
					spec.Runs, spec.Parallel, tc.wantRuns, tc.wantParallel)
			}
		})
	}
}

// TestRunIsolatesEveryCase covers GO-AC07: at 1, 8 and 32 simultaneous cases,
// every case keeps its own state, reaches its own verdict, and is attributed
// only the faults injected into it.
func TestRunIsolatesEveryCase(t *testing.T) {
	for _, parallel := range []int{1, 8, 32} {
		t.Run(fmt.Sprintf("parallel=%d", parallel), func(t *testing.T) {
			leaktest.Check(t)
			env := setupEnv(t, "lost-response.json")

			const cases = 32
			summary, err := env.Run(context.Background(), RunSpec{Runs: cases, Parallel: parallel})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if summary.Executed != cases || summary.Failed != cases {
				t.Fatalf("executed=%d failed=%d, want %d and %d (passed=%d inconclusive=%d infrastructure=%d)",
					summary.Executed, summary.Failed, cases, cases,
					summary.Passed, summary.Inconclusive, summary.HarnessErrors)
			}
			if summary.MaxConcurrentCases > parallel {
				t.Errorf("peak concurrency %d exceeded the bound of %d",
					summary.MaxConcurrentCases, parallel)
			}

			for i, o := range summary.Outcomes {
				want := CaseID(i + 1)
				if o.CaseID != want {
					t.Errorf("outcome %d is %s, want %s: ordering must not depend on scheduling",
						i, o.CaseID, want)
				}
				if got := o.Verification.Observed.Counter; got != 2 {
					t.Errorf("%s counter = %d, want 2: case state was not isolated", want, got)
				}
				if len(o.Decisions) != 1 {
					t.Errorf("%s recorded %d decisions, want 1 belonging to it", want, len(o.Decisions))
				}
				for _, d := range o.Decisions {
					if d.CaseID != want {
						t.Errorf("%s was attributed a decision from %s", want, d.CaseID)
					}
				}
			}
		})
	}
}

// TestRunHonoursTheConcurrencyBound checks both directions: the pool never
// exceeds --parallel, and it does genuinely overlap cases rather than quietly
// running them one at a time.
func TestRunHonoursTheConcurrencyBound(t *testing.T) {
	leaktest.Check(t)
	env := setupEnv(t, "lost-response.json")

	const parallel = 4
	summary, err := env.Run(context.Background(), RunSpec{Runs: 32, Parallel: parallel})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if summary.MaxConcurrentCases > parallel {
		t.Errorf("peak concurrency %d exceeded the bound of %d", summary.MaxConcurrentCases, parallel)
	}
	if summary.MaxConcurrentCases < 2 {
		t.Errorf("peak concurrency %d: the pool never overlapped two cases, so the bound proves nothing",
			summary.MaxConcurrentCases)
	}
}

// TestRunCancellationStopsDispatchWithoutMisclassifying covers the
// cancellation half of GO-AC08. A run that is cut short must stop starting
// work, account for every case, and never hand a cut-short case a verdict.
//
// The unsafe fixture is used deliberately: against the safe one every case
// passes, so an assertion that nothing was misclassified would hold no matter
// what the code did.
func TestRunCancellationStopsDispatchWithoutMisclassifying(t *testing.T) {
	leaktest.Check(t)
	env := setupEnv(t, "lost-response.json")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Serial execution over many cases guarantees there is still work left to
	// abandon when the cancellation lands.
	time.AfterFunc(20*time.Millisecond, cancel)

	const cases = 500
	summary, err := env.Run(ctx, RunSpec{Runs: cases, Parallel: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !summary.Canceled {
		t.Error("summary does not record that the run was cancelled")
	}
	if summary.NotRun == 0 {
		t.Error("every case ran, so this test did not exercise cancellation")
	}
	if summary.Executed+summary.NotRun != cases {
		t.Errorf("executed %d and skipped %d, which does not account for all %d cases",
			summary.Executed, summary.NotRun, cases)
	}
	// Every case that ran to completion violates this scenario's invariant,
	// and every case that did not is inconclusive. A pass could only come
	// from crediting a case that never finished.
	if summary.Passed != 0 {
		t.Errorf("%d case(s) passed in a cancelled run against the unsafe fixture", summary.Passed)
	}
	if summary.Executed != summary.Failed+summary.Inconclusive+summary.HarnessErrors {
		t.Errorf("executed=%d does not match failed=%d + inconclusive=%d + infrastructure=%d",
			summary.Executed, summary.Failed, summary.Inconclusive, summary.HarnessErrors)
	}
}

// TestRunCaseCancelledDuringAnAttemptHasNoVerdict is the sharp edge of the
// same rule. The attempt sequence reports a cancelled attempt as a transport
// error rather than raising it, so a case can be cut short and still look
// conclusive to the oracle.
//
// This scenario is built so the trap is unavoidable: the upstream applies the
// operation, the proxy then holds the response, and the cancellation lands
// while the only attempt is in flight. The state left behind matches the
// invariant exactly, so a harness that reads state without asking whether the
// case finished reports a pass for a case that never completed.
func TestRunCaseCancelledDuringAnAttemptHasNoVerdict(t *testing.T) {
	leaktest.Check(t)
	sc, err := scenario.Parse([]byte(`{
	  "schema_version": 1,
	  "name": "cancelled-during-attempt",
	  "fixture": "safe-counter",
	  "client": {
	    "method": "POST",
	    "path": "/operation",
	    "max_attempts": 1,
	    "retry_on_transport_error": false,
	    "reuse_idempotency_key": true
	  },
	  "faults": [
	    {"attempt": 1, "stage": "after_upstream_response", "action": "delay_ms", "delay_ms": 300}
	  ],
	  "invariant": {"type": "counter_equals", "expected": 1}
	}`))
	if err != nil {
		t.Fatalf("parse scenario: %v", err)
	}
	env := setupEnvFromScenario(t, sc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)

	outcome := env.RunCase(ctx, CaseID(1))

	if outcome.Verdict() != verify.VerdictInconclusive {
		t.Errorf("verdict = %q, want %q: a case cut short has no verdict, however "+
			"conclusive the state it reached looks (%s)",
			outcome.Verdict(), verify.VerdictInconclusive, outcome.Verification.Detail)
	}
}

// TestRunIsReproducibleAcrossParallelism is the reproducibility guarantee:
// concurrency changes when cases run, never what happens to them. Decision
// sequence numbers are deliberately excluded, since they record arrival order
// across the whole run and are the one thing parallelism may legitimately
// reorder.
func TestRunIsReproducibleAcrossParallelism(t *testing.T) {
	leaktest.Check(t)

	type caseFacts struct {
		verdict verify.Verdict
		counter int
		faults  string
	}

	collect := func(parallel int) map[string]caseFacts {
		t.Helper()
		env := setupEnv(t, "lost-response.json")
		summary, err := env.Run(context.Background(), RunSpec{Runs: 24, Parallel: parallel})
		if err != nil {
			t.Fatalf("Run at parallel=%d: %v", parallel, err)
		}
		facts := make(map[string]caseFacts, len(summary.Outcomes))
		for _, o := range summary.Outcomes {
			var injected strings.Builder
			for _, d := range o.Decisions {
				fmt.Fprintf(&injected, "attempt=%d/%s/%s;", d.Attempt, d.Stage, d.Action)
			}
			facts[o.CaseID] = caseFacts{
				verdict: o.Verdict(),
				counter: o.Verification.Observed.Counter,
				faults:  injected.String(),
			}
		}
		return facts
	}

	serial := collect(1)
	concurrent := collect(16)

	if len(serial) != 24 {
		t.Fatalf("serial run produced %d cases, want 24", len(serial))
	}
	if !reflect.DeepEqual(serial, concurrent) {
		for id, want := range serial {
			if got := concurrent[id]; got != want {
				t.Errorf("%s: serial %+v, concurrent %+v", id, want, got)
			}
		}
	}
}
