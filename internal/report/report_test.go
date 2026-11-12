package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/client"
	"github.com/kyle-luong/http-fault-injection-tester/internal/faults"
	"github.com/kyle-luong/http-fault-injection-tester/internal/fixture"
	"github.com/kyle-luong/http-fault-injection-tester/internal/runner"
	"github.com/kyle-luong/http-fault-injection-tester/internal/verify"
)

// sampleResult is a result with every nested shape populated, so a change to
// any of them shows up in the schema test rather than in a user's pipeline.
func sampleResult() Result {
	return New("run-0123456789abcdef", time.Date(2026, 9, 20, 18, 3, 16, 0, time.UTC),
		runner.Summary{
			Scenario:           "lost-response",
			Fixture:            "unsafe-counter",
			Runs:               1,
			Parallel:           1,
			Executed:           1,
			Failed:             1,
			Duration:           5 * time.Millisecond,
			MaxConcurrentCases: 2,
			Outcomes: []runner.Outcome{{
				CaseID:   "case-0001",
				Scenario: "lost-response",
				Fixture:  "unsafe-counter",
				Attempts: []client.Attempt{{
					Number:         1,
					IdempotencyKey: "case-0001-key",
					TransportError: "connection reset by peer",
					Duration:       time.Millisecond,
				}, {
					Number:         2,
					IdempotencyKey: "case-0001-key",
					StatusCode:     200,
					Duration:       time.Millisecond,
					Response: &fixture.OperationResponse{
						CaseID: "case-0001", OperationID: "case-0001-key", Counter: 2,
					},
				}},
				Decisions: []faults.Decision{{
					Seq: 1, CaseID: "case-0001", Attempt: 1,
					Stage: faults.StageAfterUpstreamResponse, Action: faults.ActionCloseClientConnection,
				}},
				Verification: verify.Result{
					Verdict:   verify.VerdictFail,
					Invariant: verify.Invariant{Type: verify.CounterEquals, Expected: 1},
					Observed:  fixture.State{CaseID: "case-0001", Counter: 2, Operations: []string{"case-0001-key"}},
					Detail:    "expected counter=1; observed counter=2",
				},
				Duration: 3 * time.Millisecond,
			}},
		})
}

func TestWriteThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "run.json")
	want := sampleResult()

	if err := Write(path, false, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the result:\n got %+v\nwant %+v", got, want)
	}
}

// TestWriteRefusesToReplaceWithoutOverwrite protects a finished run: a result
// path is easy to reuse by accident, and the run behind it may have taken
// minutes.
func TestWriteRefusesToReplaceWithoutOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.json")
	if err := Write(path, false, sampleResult()); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	if err := Write(path, false, sampleResult()); err == nil {
		t.Error("Write replaced an existing result without --overwrite")
	}
	if err := Write(path, true, sampleResult()); err != nil {
		t.Errorf("Write with overwrite failed: %v", err)
	}
}

func TestParseRejectsUnusableResults(t *testing.T) {
	valid, err := json.Marshal(sampleResult())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	tests := []struct {
		name string
		data string
	}{
		{"empty input", ""},
		{"not JSON", "{"},
		{"wrong schema version", `{"schema_version":99,"run_id":"run-1"}`},
		{"missing schema version", `{"run_id":"run-1"}`},
		{"missing run id", `{"schema_version":1}`},
		{"unknown field", `{"schema_version":1,"run_id":"run-1","surprise":true}`},
		{"trailing content", string(valid) + `{"schema_version":1}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.data)); err == nil {
				t.Errorf("Parse accepted %s", tc.name)
			}
		})
	}
}

func TestLoadRejectsAnOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.json")
	// Sized by truncation rather than by writing the bytes: the point is the
	// boundary, and materialising 64 MiB on every test run is pure waste.
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
	if _, err := Load(path); err == nil {
		t.Error("Load accepted a file past the size bound")
	}
}

// TestResultSchemaIsStable pins the field names a consumer reads. The result
// file is a published interface: renaming a key is a breaking change and must
// fail here rather than quietly in whatever reads it.
func TestResultSchemaIsStable(t *testing.T) {
	raw, err := json.Marshal(sampleResult())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	wantTop := []string{"schema_version", "run_id", "started_at", "summary"}
	if got := len(decoded); got != len(wantTop) {
		t.Errorf("result has %d top-level keys, want %d: %v", got, len(wantTop), keysOf(decoded))
	}
	for _, key := range wantTop {
		if _, ok := decoded[key]; !ok {
			t.Errorf("result is missing top-level key %q", key)
		}
	}

	summary, ok := decoded["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary is not an object")
	}
	for _, key := range []string{
		"scenario", "fixture", "runs", "parallel", "executed", "not_run",
		"passed", "failed", "inconclusive", "harness_errors", "canceled",
		"duration_ns", "max_concurrent_cases", "proxy_stats",
		"dropped_decisions", "outcomes",
	} {
		if _, ok := summary[key]; !ok {
			t.Errorf("summary is missing key %q", key)
		}
	}

	// Pinned by name, not just by presence: these come from another package
	// and would otherwise drift into the published schema unnoticed.
	stats, ok := summary["proxy_stats"].(map[string]any)
	if !ok {
		t.Fatalf("proxy_stats is not an object")
	}
	for _, key := range []string{"requests", "forwarded", "faults_injected", "rejected"} {
		if _, ok := stats[key]; !ok {
			t.Errorf("proxy_stats is missing key %q: %v", key, keysOf(stats))
		}
	}

	outcomes, ok := summary["outcomes"].([]any)
	if !ok || len(outcomes) == 0 {
		t.Fatalf("outcomes is not a non-empty array")
	}
	outcome, ok := outcomes[0].(map[string]any)
	if !ok {
		t.Fatalf("outcome is not an object")
	}
	for _, key := range []string{
		"case_id", "scenario", "fixture", "attempts", "fault_decisions",
		"verification", "duration_ns",
	} {
		if _, ok := outcome[key]; !ok {
			t.Errorf("outcome is missing key %q", key)
		}
	}

	verification, ok := outcome["verification"].(map[string]any)
	if !ok {
		t.Fatalf("verification is not an object")
	}
	for _, key := range []string{"verdict", "invariant", "observed_state", "detail"} {
		if _, ok := verification[key]; !ok {
			t.Errorf("verification is missing key %q", key)
		}
	}
}

// TestResultCarriesNoRequestPayloads covers the redaction half of GO-AC09 at
// the level of the format itself: no key anywhere in a result may look like a
// header, a body or a credential.
//
// The idempotency key is present on purpose. It is the harness's own stable
// identifier for a logical operation, which the PRD requires for traceability,
// and it carries no caller data.
func TestResultCarriesNoRequestPayloads(t *testing.T) {
	raw, err := json.Marshal(sampleResult())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	forbidden := []string{"header", "body", "cookie", "auth", "token", "secret", "password", "credential"}
	for _, key := range allKeys(decoded) {
		lower := strings.ToLower(key)
		for _, bad := range forbidden {
			if strings.Contains(lower, bad) {
				t.Errorf("result contains key %q, which suggests request data that must not be recorded", key)
			}
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// allKeys returns every object key anywhere in a decoded JSON value.
func allKeys(v any) []string {
	switch typed := v.(type) {
	case map[string]any:
		var out []string
		for k, nested := range typed {
			out = append(out, k)
			out = append(out, allKeys(nested)...)
		}
		return out
	case []any:
		var out []string
		for _, nested := range typed {
			out = append(out, allKeys(nested)...)
		}
		return out
	default:
		return nil
	}
}

// TestParseRejectsIncoherentSummaries is the teeth behind `report`'s exit
// code. Each of these files is syntactically fine and establishes nothing, so
// accepting any of them would let a CI gate read a pass out of it.
func TestParseRejectsIncoherentSummaries(t *testing.T) {
	tests := []struct {
		name  string
		mutge func(*Result)
	}{
		{"no scenario", func(r *Result) { r.Summary.Scenario = "" }},
		{"no cases at all", func(r *Result) { r.Summary = runnerSummaryZero() }},
		{"cases unaccounted for", func(r *Result) { r.Summary.Runs = 5 }},
		{"more outcomes than executed", func(r *Result) { r.Summary.Executed = 0; r.Summary.Failed = 0 }},
		{"verdicts do not add up", func(r *Result) { r.Summary.Failed = 0; r.Summary.Passed = 0 }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleResult()
			tc.mutge(&r)
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if _, err := Parse(data); err == nil {
				t.Errorf("Parse accepted a result with %s", tc.name)
			}
		})
	}
}

func runnerSummaryZero() runner.Summary { return runner.Summary{} }
