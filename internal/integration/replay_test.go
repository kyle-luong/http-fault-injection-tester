package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/leaktest"
	"github.com/kyle-luong/http-fault-injection-tester/internal/runner"
	"github.com/kyle-luong/http-fault-injection-tester/internal/trace"
	"github.com/kyle-luong/http-fault-injection-tester/internal/verify"
)

// TestTraceReplayReproducesTheRecordedCase covers GO-AC06 and GO-T08: a case
// recorded to a file, replayed from that file alone on fresh infrastructure,
// must inject the same decisions and reach the same oracle verdict.
//
// The trace goes through the filesystem rather than staying in memory, so the
// test exercises the written artefact an operator would actually replay.
func TestTraceReplayReproducesTheRecordedCase(t *testing.T) {
	scenarios := []struct {
		file          string
		wantVerdict   verify.Verdict
		wantDecisions int
	}{
		{"lost-response.json", verify.VerdictFail, 1},
		{"lost-response-safe.json", verify.VerdictPass, 1},
		{"no-fault-baseline.json", verify.VerdictPass, 0},
		{"reject-before-forward.json", verify.VerdictPass, 1},
	}

	for _, tc := range scenarios {
		t.Run(tc.file, func(t *testing.T) {
			leaktest.Check(t)

			sc := loadScenario(t, tc.file)
			recorded := runOne(t, setupEnv(t, sc, runner.Options{}), "case-0001")
			if recorded.Verdict() != tc.wantVerdict {
				t.Fatalf("recorded verdict = %s, want %s (%s)",
					recorded.Verdict(), tc.wantVerdict, recorded.Verification.Detail)
			}
			if len(recorded.Decisions) != tc.wantDecisions {
				t.Fatalf("recorded %d decision(s), want %d", len(recorded.Decisions), tc.wantDecisions)
			}

			dir := t.TempDir()
			if err := trace.WriteRun(dir, false, "run-test", time.Now().UTC(),
				runner.DefaultCaseTimeout, sc, []runner.Outcome{recorded}); err != nil {
				t.Fatalf("WriteRun: %v", err)
			}
			loaded, err := trace.Load(trace.Path(dir, recorded.CaseID))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			// Everything the replay needs comes from the trace: a fresh
			// fixture and proxy, the deadline the recorded run used, and a
			// schedule built from the recorded decisions rather than from the
			// scenario's rules.
			replayEnv := setupEnv(t, loaded.Scenario, runner.Options{
				CaseTimeout: loaded.CaseTimeout,
				Rules:       loaded.Rules(),
			})
			replayed := runOne(t, replayEnv, loaded.CaseID)

			if diffs := trace.Compare(loaded, replayed); len(diffs) > 0 {
				t.Fatalf("replay did not reproduce the record:\n  %s", strings.Join(diffs, "\n  "))
			}
			if len(replayed.Decisions) != tc.wantDecisions {
				t.Errorf("replay injected %d decision(s), want %d", len(replayed.Decisions), tc.wantDecisions)
			}
		})
	}
}

// TestReplayDetectsATraceThatNoLongerDescribesItsRun is the other half of
// GO-AC06: replay has to be able to *fail*. A trace claiming no faults were
// injected must replay without faults, which for the lost-response scenario
// changes the verdict -- and proves replay is driven by the recorded decisions
// rather than by the scenario's own rules.
func TestReplayDetectsATraceThatNoLongerDescribesItsRun(t *testing.T) {
	leaktest.Check(t)

	sc := loadScenario(t, "lost-response.json")
	recorded := runOne(t, setupEnv(t, sc, runner.Options{}), "case-0001")
	if recorded.Verdict() != verify.VerdictFail {
		t.Fatalf("recorded verdict = %s, want fail", recorded.Verdict())
	}

	doctored, err := trace.New("run-test", time.Now().UTC(), runner.DefaultCaseTimeout, sc, recorded)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	doctored.Decisions = nil

	replayed := runOne(t, setupEnv(t, doctored.Scenario,
		runner.Options{Rules: doctored.Rules()}), "case-0001")
	if len(replayed.Decisions) != 0 {
		t.Fatalf("replay injected %d decision(s) from the scenario; a trace that recorded no "+
			"decisions must replay with none", len(replayed.Decisions))
	}
	if replayed.Verdict() != verify.VerdictPass {
		t.Fatalf("replay verdict = %s, want pass once no fault is injected (%s)",
			replayed.Verdict(), replayed.Verification.Detail)
	}

	diffs := trace.Compare(doctored, replayed)
	if len(diffs) == 0 {
		t.Fatal("Compare accepted a replay that reached a different verdict")
	}
	joined := strings.Join(diffs, "\n")
	for _, want := range []string{"verdict:", "counter:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("differences do not mention %q:\n%s", want, joined)
		}
	}
}

// TestReplayUsesTheRecordedDeadline covers the one configuration value that is
// neither in the scenario nor derivable: the per-case deadline. The scenario
// below injects a 600ms delay, so a 100ms deadline leaves the case
// inconclusive and the default 5s deadline does not. A replay that substituted
// its own deadline would reach a different verdict and then report the
// difference as a failure to reproduce.
func TestReplayUsesTheRecordedDeadline(t *testing.T) {
	leaktest.Check(t)

	sc := loadScenario(t, "delayed-response.json")
	const deadline = 100 * time.Millisecond

	env := setupEnv(t, sc, runner.Options{CaseTimeout: deadline})
	recorded := runOne(t, env, "case-0001")
	if recorded.Verdict() != verify.VerdictInconclusive {
		t.Fatalf("recorded verdict = %s, want inconclusive under a %s deadline (%s)",
			recorded.Verdict(), deadline, recorded.Verification.Detail)
	}

	dir := t.TempDir()
	if err := trace.WriteRun(dir, false, "run-test", time.Now().UTC(), env.CaseTimeout(), sc,
		[]runner.Outcome{recorded}); err != nil {
		t.Fatalf("WriteRun: %v", err)
	}
	loaded, err := trace.Load(trace.Path(dir, recorded.CaseID))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CaseTimeout != deadline {
		t.Fatalf("trace recorded a %s deadline, want %s", loaded.CaseTimeout, deadline)
	}

	replayed := runOne(t, setupEnv(t, loaded.Scenario, runner.Options{
		CaseTimeout: loaded.CaseTimeout,
		Rules:       loaded.Rules(),
	}), loaded.CaseID)
	if diffs := trace.Compare(loaded, replayed); len(diffs) > 0 {
		t.Fatalf("replay did not reproduce the record:\n  %s", strings.Join(diffs, "\n  "))
	}

	// And the deadline is what decided it: the same trace replayed under the
	// default deadline reaches a different verdict.
	under5s := runOne(t, setupEnv(t, loaded.Scenario, runner.Options{Rules: loaded.Rules()}), loaded.CaseID)
	if under5s.Verdict() == recorded.Verdict() {
		t.Errorf("the recorded deadline made no difference to the verdict (%s), so this test "+
			"proves nothing about it", under5s.Verdict())
	}
}
