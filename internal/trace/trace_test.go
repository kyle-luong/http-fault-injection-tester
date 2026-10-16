package trace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/client"
	"github.com/kyle-luong/http-fault-injection-tester/internal/faults"
	"github.com/kyle-luong/http-fault-injection-tester/internal/fixture"
	"github.com/kyle-luong/http-fault-injection-tester/internal/runner"
	"github.com/kyle-luong/http-fault-injection-tester/internal/scenario"
	"github.com/kyle-luong/http-fault-injection-tester/internal/verify"
)

// recordedOutcome is a finished lost-response case, as the runner reports one.
func recordedOutcome() runner.Outcome {
	return runner.Outcome{
		CaseID:   "case-0001",
		Scenario: "lost-response",
		Fixture:  "unsafe-counter",
		Attempts: []client.Attempt{{Number: 1}, {Number: 2}},
		Decisions: []faults.Decision{{
			Seq:     1,
			CaseID:  "case-0001",
			Attempt: 1,
			Stage:   faults.StageAfterUpstreamResponse,
			Action:  faults.ActionCloseClientConnection,
		}},
		Verification: verify.Result{
			Verdict:   verify.VerdictFail,
			Invariant: verify.Invariant{Type: verify.CounterEquals, Expected: 1},
			Observed: fixture.State{
				CaseID:     "case-0001",
				Counter:    2,
				Operations: []string{"case-0001-key", "case-0001-key"},
			},
		},
	}
}

func loadScenario(t *testing.T, name string) *scenario.Scenario {
	t.Helper()
	sc, err := scenario.Load(filepath.Join("..", "..", "scenarios", name))
	if err != nil {
		t.Fatalf("load scenario %s: %v", name, err)
	}
	return sc
}

func sampleTrace(t *testing.T) Trace {
	t.Helper()
	tr, err := New("run-test", time.Date(2026, 10, 15, 18, 4, 11, 0, time.UTC),
		runner.DefaultCaseTimeout, loadScenario(t, "lost-response.json"), recordedOutcome())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tr
}

// mutate serialises a valid trace, lets f corrupt the JSON, and returns the
// result. Tampering with the encoded form is the only honest way to test the
// parser: a Go value cannot express most of what a file can.
func mutate(t *testing.T, tr Trace, f func(m map[string]any)) []byte {
	t.Helper()
	data, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	f(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	return out
}

func child(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	sub, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is not an object", key)
	}
	return sub
}

func TestWriteRunAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sc := loadScenario(t, "lost-response.json")
	recorded := recordedOutcome()
	recordedAt := time.Date(2026, 10, 15, 18, 4, 11, 0, time.UTC)

	if err := WriteRun(context.Background(), dir, false, "run-test", recordedAt, 3*time.Second, sc,
		[]runner.Outcome{recorded}); err != nil {
		t.Fatalf("WriteRun: %v", err)
	}

	tr, err := Load(Path(dir, "case-0001"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tr.SchemaVersion != SchemaVersion || tr.RunID != "run-test" || tr.CaseID != "case-0001" {
		t.Errorf("identity did not survive the round trip: %+v", tr)
	}
	if !tr.RecordedAt.Equal(recordedAt) {
		t.Errorf("recorded_at = %s, want %s", tr.RecordedAt, recordedAt)
	}
	// The deadline is configuration, not metadata: replay reruns the case
	// under it, so losing it would change the outcome.
	if tr.CaseTimeout != 3*time.Second {
		t.Errorf("case_timeout_ns = %s, want 3s", tr.CaseTimeout)
	}
	if tr.Scenario.Name != "lost-response" || tr.Scenario.Fixture != "unsafe-counter" {
		t.Errorf("embedded scenario did not survive: %+v", tr.Scenario)
	}
	if len(tr.Decisions) != 1 || tr.Decisions[0].Action != faults.ActionCloseClientConnection {
		t.Errorf("decisions did not survive: %+v", tr.Decisions)
	}
	if tr.Expected.Verdict != verify.VerdictFail || tr.Expected.AttemptCount != 2 ||
		tr.Expected.State.Counter != 2 {
		t.Errorf("expected outcome did not survive: %+v", tr.Expected)
	}

	// The digest must describe the bytes the client really sends for this case.
	want, err := client.PayloadDigest("case-0001", sc.Delta())
	if err != nil {
		t.Fatalf("PayloadDigest: %v", err)
	}
	if tr.Request.PayloadDigest != want {
		t.Errorf("payload_digest = %q, want %q", tr.Request.PayloadDigest, want)
	}
}

func TestWriteRunRefusesToOverwriteATrace(t *testing.T) {
	dir := t.TempDir()
	sc := loadScenario(t, "lost-response.json")
	out := []runner.Outcome{recordedOutcome()}
	now := time.Now().UTC()

	timeout := runner.DefaultCaseTimeout
	if err := WriteRun(context.Background(), dir, false, "run-a", now, timeout, sc, out); err != nil {
		t.Fatalf("first WriteRun: %v", err)
	}
	if err := WriteRun(context.Background(), dir, false, "run-b", now, timeout, sc, out); err == nil {
		t.Error("WriteRun replaced an existing trace without --overwrite")
	}
	if err := WriteRun(context.Background(), dir, true, "run-b", now, timeout, sc, out); err != nil {
		t.Fatalf("WriteRun with overwrite: %v", err)
	}
	tr, err := Load(Path(dir, "case-0001"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tr.RunID != "run-b" {
		t.Errorf("run_id = %q, want the overwriting run", tr.RunID)
	}
}

// TestTraceFileNamesStayInsideTheDirectory matters because --case-id is
// operator input and reaches the writer as a path component.
func TestTraceFileNamesStayInsideTheDirectory(t *testing.T) {
	dir := t.TempDir()
	sc := loadScenario(t, "lost-response.json")
	o := recordedOutcome()
	o.CaseID = filepath.Join("..", "escaped")

	if err := WriteRun(context.Background(), dir, false, "run-test", time.Now().UTC(), runner.DefaultCaseTimeout, sc,
		[]runner.Outcome{o}); err == nil {
		t.Error("WriteRun accepted a case ID that leaves the trace directory")
	}
	if _, err := CheckWritable(dir, false, []string{"../escaped"}); err == nil {
		t.Error("CheckWritable accepted a case ID that leaves the trace directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escaped.json")); err == nil {
		t.Error("a trace was written outside the trace directory")
	}
}

// TestCheckWritableRefusesBeforeARun keeps the refusal early: discovering a
// taken path after 10,000 cases have executed throws the run away.
func TestCheckWritableRefusesBeforeARun(t *testing.T) {
	dir := t.TempDir()
	ids := []string{"case-0001", "case-0002"}

	if existing, err := CheckWritable(dir, false, ids); err != nil || existing != 0 {
		t.Fatalf("CheckWritable on an empty directory = (%d, %v)", existing, err)
	}

	// A trace this run would not write still makes the directory describe two
	// runs at once, so it is refused too.
	if err := os.WriteFile(Path(dir, "case-0009"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed trace: %v", err)
	}
	existing, err := CheckWritable(dir, false, ids)
	if err == nil {
		t.Fatal("CheckWritable accepted a directory already holding a trace")
	}
	if existing != 1 {
		t.Errorf("CheckWritable reported %d existing trace(s), want 1", existing)
	}
	if !strings.Contains(err.Error(), "--overwrite") {
		t.Errorf("error did not say how to proceed: %v", err)
	}

	// With --overwrite it proceeds, but the count is reported so the caller can
	// warn that the directory holds traces from another run.
	if existing, err := CheckWritable(dir, true, ids); err != nil || existing != 1 {
		t.Errorf("CheckWritable with overwrite = (%d, %v), want (1, nil)", existing, err)
	}
}

// TestCheckWritableIgnoresInterruptedLitter: a run killed outright leaves the
// temporary file its atomic write was using. That is this package's own litter,
// and a directory holding one must not be refused as though it held a trace --
// a hard kill would otherwise poison the next run.
func TestCheckWritableIgnoresInterruptedLitter(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".tmp-12345.json", ".probe-678"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	existing, err := CheckWritable(dir, false, []string{"case-0001"})
	if err != nil {
		t.Errorf("CheckWritable refused a directory holding only litter: %v", err)
	}
	if existing != 0 {
		t.Errorf("CheckWritable counted %d existing trace(s), want 0", existing)
	}
}

func TestParseRejectsUnusableTraces(t *testing.T) {
	valid := sampleTrace(t)

	cases := []struct {
		name string
		edit func(m map[string]any)
		want string
	}{
		{"unknown field", func(m map[string]any) { m["surprise"] = 1 }, "unknown field"},
		{"unsupported version", func(m map[string]any) { m["schema_version"] = 2 }, "schema_version"},
		{"no run id", func(m map[string]any) { delete(m, "run_id") }, "run_id"},
		{"no case id", func(m map[string]any) { m["case_id"] = "" }, "trace file name"},
		{"case id escapes", func(m map[string]any) { m["case_id"] = "../escaped" }, "trace file name"},
		{"no scenario", func(m map[string]any) { m["scenario"] = nil }, "scenario is required"},
		{"invalid scenario", func(m map[string]any) {
			child(t, m, "scenario")["name"] = ""
		}, "embedded scenario"},
		{"request disagrees with the scenario", func(m map[string]any) {
			child(t, m, "request")["path"] = "/elsewhere"
		}, "does not match the embedded scenario"},
		{"malformed digest", func(m map[string]any) {
			child(t, m, "request")["payload_digest"] = "sha256:short"
		}, "not a sha256 digest"},
		// A well-formed digest of something else. Editing the scenario's delta
		// instead would trip the scenario digest first, so this row edits the
		// payload digest itself; between them the pair binds the case ID and
		// the delta to the rest of the file.
		{"digest describes other bytes", func(m map[string]any) {
			child(t, m, "request")["payload_digest"] = "sha256:" + strings.Repeat("0", 64)
		}, "payload_digest does not describe"},
		{"case id swapped for another", func(m map[string]any) {
			m["case_id"] = "case-0002"
			childOf(t, m, "expected", "state")["case_id"] = "case-0002"
			m["fault_decisions"].([]any)[0].(map[string]any)["case_id"] = "case-0002"
		}, "payload_digest does not describe"},
		{"decision from another case", func(m map[string]any) {
			m["fault_decisions"].([]any)[0].(map[string]any)["case_id"] = "case-0002"
		}, "belongs to case"},
		{"decision beyond the retry policy", func(m map[string]any) {
			m["fault_decisions"].([]any)[0].(map[string]any)["attempt"] = 9
		}, "exceeds the policy"},
		{"two decisions for one stage", func(m map[string]any) {
			list := m["fault_decisions"].([]any)
			m["fault_decisions"] = append(list, list[0])
		}, "cannot be replayed"},
		{"unknown verdict", func(m map[string]any) {
			child(t, m, "expected")["verdict"] = "probably"
		}, "unknown expected verdict"},
		{"attempts beyond the policy", func(m map[string]any) {
			child(t, m, "expected")["attempt_count"] = 7
		}, "expected.attempt_count"},
		{"state from another case", func(m map[string]any) {
			childOf(t, m, "expected", "state")["case_id"] = "case-0002"
		}, "expected.state belongs to case"},
		{"no recorded time", func(m map[string]any) { delete(m, "recorded_at") }, "recorded_at"},
		{"no deadline", func(m map[string]any) { delete(m, "case_timeout_ns") }, "case_timeout_ns"},
		{"deadline past the bound", func(m map[string]any) {
			m["case_timeout_ns"] = int64(runner.MaxCaseTimeout) + 1
		}, "case_timeout_ns"},
		{"malformed scenario digest", func(m map[string]any) {
			m["scenario_digest"] = "sha256:nope"
		}, "scenario_digest"},
		// The three edits below are the ones that matter: each leaves a file
		// that parses, replays and reports a verdict about a configuration
		// nobody recorded. The invariant edit turns a recorded violation into
		// a clean run, and the fixture edit changes which application is under
		// test.
		{"invariant edited", func(m map[string]any) {
			childOf(t, m, "scenario", "invariant")["expected"] = 2
		}, "scenario_digest does not describe"},
		{"fixture swapped", func(m map[string]any) {
			child(t, m, "scenario")["fixture"] = "safe-counter"
		}, "scenario_digest does not describe"},
		{"retry policy edited", func(m map[string]any) {
			childOf(t, m, "scenario", "client")["max_attempts"] = 3
		}, "scenario_digest does not describe"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(mutate(t, valid, tc.edit))
			if err == nil {
				t.Fatal("Parse accepted an unusable trace")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q, so the trace may have been rejected "+
					"for an unrelated reason", err, tc.want)
			}
		})
	}
}

// childOf walks nested objects, which keeps the mutations above readable.
func childOf(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	for _, k := range keys {
		m = child(t, m, k)
	}
	return m
}

func TestParseRejectsCorruptedBytes(t *testing.T) {
	valid := sampleTrace(t)
	good, err := json.Marshal(valid)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	cases := map[string][]byte{
		"empty":            {},
		"not json":         []byte("this is not a trace"),
		"truncated":        good[:len(good)/2],
		"trailing content": append(append([]byte{}, good...), []byte("{}")...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(data); err == nil {
				t.Error("Parse accepted corrupted bytes")
			}
		})
	}
}

func TestLoadRejectsAnOversizedTrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	// Sized by truncation: the boundary is the point, not the bytes.
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(MaxFileBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err = Load(path)
	if err == nil {
		t.Fatal("Load accepted a trace past the size bound")
	}
	// Without this the test passes on "not JSON" and the bound itself is
	// untested: a file of NUL bytes fails to parse either way.
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("Load rejected the file for the wrong reason: %v", err)
	}
}

// TestRulesAreTheRecordedDecisions pins the distinction the runner depends on:
// a nil rule set means "take them from the scenario", so a trace that recorded
// nothing must still hand over a non-nil, empty set.
func TestRulesAreTheRecordedDecisions(t *testing.T) {
	tr := sampleTrace(t)
	rules := tr.Rules()
	if len(rules) != 1 {
		t.Fatalf("Rules() returned %d rules, want 1", len(rules))
	}
	want := faults.Rule{
		CaseID:  "case-0001",
		Attempt: 1,
		Stage:   faults.StageAfterUpstreamResponse,
		Action:  faults.ActionCloseClientConnection,
	}
	if rules[0] != want {
		t.Errorf("Rules()[0] = %+v, want %+v", rules[0], want)
	}

	tr.Decisions = nil
	if empty := tr.Rules(); empty == nil {
		t.Error("Rules() returned nil for a trace with no decisions, which the runner reads " +
			"as a request for the scenario's own faults")
	} else if len(empty) != 0 {
		t.Errorf("Rules() invented %d rule(s)", len(empty))
	}
}

func TestCompareFindsEveryKindOfDisagreement(t *testing.T) {
	tr := sampleTrace(t)

	if diffs := Compare(tr, recordedOutcome()); len(diffs) != 0 {
		t.Fatalf("Compare reported differences against the very outcome recorded: %v", diffs)
	}

	cases := []struct {
		name    string
		corrupt func(o *runner.Outcome)
		want    string
	}{
		{"verdict", func(o *runner.Outcome) { o.Verification.Verdict = verify.VerdictPass }, "verdict:"},
		{"attempts", func(o *runner.Outcome) { o.Attempts = o.Attempts[:1] }, "attempts:"},
		{"counter", func(o *runner.Outcome) { o.Verification.Observed.Counter = 1 }, "counter:"},
		{"applied operations", func(o *runner.Outcome) {
			o.Verification.Observed.Operations = []string{"case-0001-key"}
		}, "applied operations:"},
		{"nothing injected", func(o *runner.Outcome) { o.Decisions = nil }, "fault decision"},
		{"a different fault injected", func(o *runner.Outcome) {
			o.Decisions[0].Action = faults.ActionDelay
			o.Decisions[0].Delay = time.Second
		}, "decision 1:"},
		{"the same fault with a different delay", func(o *runner.Outcome) {
			o.Decisions[0].Delay = 600 * time.Millisecond
		}, "decision 1:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := recordedOutcome()
			tc.corrupt(&o)
			diffs := Compare(tr, o)
			if len(diffs) == 0 {
				t.Fatal("Compare reported no difference")
			}
			if !strings.Contains(strings.Join(diffs, "\n"), tc.want) {
				t.Errorf("differences %v do not mention %q", diffs, tc.want)
			}
		})
	}
}

// TestTraceSchemaIsExactlyThis is the redaction guarantee, and it is an
// allowlist on purpose. A denylist of suspicious field names cannot see a field
// it has never heard of: adding a `payload` or an `x_api_key` to the schema
// would pass one. Every key below is also frozen for v1 by
// DisallowUnknownFields, so pinning the set costs nothing it did not already
// cost.
//
// Permitted request-side content, per docs/features/M4-trace-replay.md: the
// scenario's synthetic numeric delta, and the operation identifiers the oracle
// reads back out of fixture state. Not permitted: request bodies, headers,
// credentials, anything a caller supplied.
func TestTraceSchemaIsExactlyThis(t *testing.T) {
	permitted := map[string]bool{}
	for _, k := range []string{
		"schema_version", "run_id", "case_id", "recorded_at", "case_timeout_ns",
		"scenario", "scenario_digest", "request", "fault_decisions", "expected",
		// scenario
		"name", "fixture", "client", "faults", "invariant",
		// scenario.client
		"method", "path", "max_attempts", "retry_on_transport_error",
		"reuse_idempotency_key", "delta", "timeout_ms",
		// scenario.faults[] and scenario.invariant
		"attempt", "stage", "action", "delay_ms", "type",
		// request
		"payload_digest",
		// fault_decisions[]
		"seq", "delay_ns",
		// expected and expected.state
		"verdict", "attempt_count", "state", "counter", "operations",
	} {
		permitted[k] = true
	}

	data, err := json.Marshal(sampleTrace(t))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k := range allKeys(decoded) {
		if !permitted[k] {
			t.Errorf("trace carries field %q, which is not part of the v1 schema; if it belongs "+
				"there, add it here and check it holds no request or caller data", k)
		}
	}

	// "expected" is shared with scenario.invariant, so the one key that could
	// hide a payload is spelled out rather than left to the set above.
	if strings.Contains(string(data), `"idempotency_key"`) {
		t.Error("trace recorded a request header value")
	}
}

// allKeys collects every object key in a decoded JSON document.
func allKeys(v any) map[string]bool {
	keys := map[string]bool{}
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			keys[k] = true
			for sub := range allKeys(child) {
				keys[sub] = true
			}
		}
	case []any:
		for _, child := range node {
			for sub := range allKeys(child) {
				keys[sub] = true
			}
		}
	}
	return keys
}

// TestWriteRunStopsWhenCancelled covers the interrupted output phase: a
// 10,000-case run has 10,000 files to write, and an operator who interrupts
// again must not have to wait for the rest.
func TestWriteRunStopsWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	sc := loadScenario(t, "lost-response.json")

	outcomes := make([]runner.Outcome, 0, 5)
	for i := range 5 {
		o := recordedOutcome()
		o.CaseID = runner.CaseID(i + 1)
		o.Verification.Observed.CaseID = o.CaseID
		outcomes = append(outcomes, o)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := WriteRun(ctx, dir, false, "run-test", time.Now().UTC(),
		runner.DefaultCaseTimeout, sc, outcomes)
	if err == nil {
		t.Fatal("WriteRun ignored a cancelled context")
	}
	if !strings.Contains(err.Error(), "of 5 trace(s)") {
		t.Errorf("error does not say how far it got: %v", err)
	}

	written, globErr := filepath.Glob(filepath.Join(dir, "*.json"))
	if globErr != nil {
		t.Fatalf("glob: %v", globErr)
	}
	if len(written) != 0 {
		t.Errorf("wrote %d trace(s) under a cancelled context", len(written))
	}
}
