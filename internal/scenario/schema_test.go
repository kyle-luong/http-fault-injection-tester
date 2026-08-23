package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validScenario = `{
  "schema_version": 1,
  "name": "lost-response",
  "fixture": "unsafe-counter",
  "client": {
    "method": "POST",
    "path": "/operation",
    "max_attempts": 2,
    "retry_on_transport_error": true,
    "reuse_idempotency_key": true
  },
  "faults": [
    {"attempt": 1, "stage": "after_upstream_response", "action": "close_client_connection"}
  ],
  "invariant": {"type": "counter_equals", "expected": 1}
}`

func TestParseAcceptsTheCanonicalScenario(t *testing.T) {
	sc, err := Parse([]byte(validScenario))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sc.Name != "lost-response" || sc.Fixture != "unsafe-counter" {
		t.Errorf("parsed scenario = %+v, want the lost-response unsafe scenario", sc)
	}
	if sc.Delta() != DefaultDelta {
		t.Errorf("Delta() = %d, want the default %d", sc.Delta(), DefaultDelta)
	}
	if sc.AttemptTimeout() != 0 {
		t.Errorf("AttemptTimeout() = %s, want 0 when the scenario omits it", sc.AttemptTimeout())
	}
	inv := sc.VerifyInvariant()
	if inv.Expected != 1 {
		t.Errorf("invariant expected = %d, want 1", inv.Expected)
	}
	rules, err := sc.Rules()
	if err != nil {
		t.Fatalf("Rules: %v", err)
	}
	if len(rules) != 1 || rules[0].Attempt != 1 {
		t.Errorf("rules = %+v, want one rule on attempt 1", rules)
	}
	if rules[0].CaseID != "" {
		t.Errorf("rule CaseID = %q, want a wildcard so every case gets the schedule", rules[0].CaseID)
	}
}

func TestParseReadsOptionalFields(t *testing.T) {
	raw := strings.Replace(validScenario,
		`"reuse_idempotency_key": true`,
		`"reuse_idempotency_key": true, "delta": 5, "timeout_ms": 250`, 1)
	sc, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sc.Delta() != 5 {
		t.Errorf("Delta() = %d, want 5", sc.Delta())
	}
	if sc.AttemptTimeout() != 250*time.Millisecond {
		t.Errorf("AttemptTimeout() = %s, want 250ms", sc.AttemptTimeout())
	}
}

// TestParseRejectsInvalidScenarios covers GO-T01. Every case here would
// otherwise produce a run that silently does something other than intended.
func TestParseRejectsInvalidScenarios(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(string) string
		wantMessage string
	}{
		{
			name:        "unknown top level field",
			mutate:      func(s string) string { return strings.Replace(s, `"name":`, `"nmae":`, 1) },
			wantMessage: "unknown field",
		},
		{
			name: "unknown field inside client",
			mutate: func(s string) string {
				return strings.Replace(s, `"max_attempts": 2`, `"max_attemps": 2`, 1)
			},
			wantMessage: "unknown field",
		},
		{
			name: "unknown field inside a fault",
			mutate: func(s string) string {
				return strings.Replace(s, `"attempt": 1,`, `"attempt": 1, "probability": 0.5,`, 1)
			},
			wantMessage: "unknown field",
		},
		{
			name:        "wrong schema version",
			mutate:      func(s string) string { return strings.Replace(s, `"schema_version": 1`, `"schema_version": 2`, 1) },
			wantMessage: "schema_version",
		},
		{
			name:        "missing name",
			mutate:      func(s string) string { return strings.Replace(s, `"name": "lost-response"`, `"name": ""`, 1) },
			wantMessage: "name is required",
		},
		{
			name:        "unknown fixture",
			mutate:      func(s string) string { return strings.Replace(s, `"unsafe-counter"`, `"flaky-counter"`, 1) },
			wantMessage: "fixture mode",
		},
		{
			name:        "unsupported method",
			mutate:      func(s string) string { return strings.Replace(s, `"POST"`, `"PUT"`, 1) },
			wantMessage: "method",
		},
		{
			name:        "relative path",
			mutate:      func(s string) string { return strings.Replace(s, `"/operation"`, `"operation"`, 1) },
			wantMessage: "absolute",
		},
		{
			name:        "reserved control path",
			mutate:      func(s string) string { return strings.Replace(s, `"/operation"`, `"/__test/reset"`, 1) },
			wantMessage: "reserved",
		},
		{
			name:        "zero attempts",
			mutate:      func(s string) string { return strings.Replace(s, `"max_attempts": 2`, `"max_attempts": 0`, 1) },
			wantMessage: "max_attempts",
		},
		{
			name:        "too many attempts",
			mutate:      func(s string) string { return strings.Replace(s, `"max_attempts": 2`, `"max_attempts": 99`, 1) },
			wantMessage: "max_attempts",
		},
		{
			name: "retries disabled with multiple attempts",
			mutate: func(s string) string {
				return strings.Replace(s, `"retry_on_transport_error": true`, `"retry_on_transport_error": false`, 1)
			},
			wantMessage: "retry_on_transport_error",
		},
		{
			name: "delta out of range",
			mutate: func(s string) string {
				return strings.Replace(s, `"max_attempts": 2`, `"max_attempts": 2, "delta": 0`, 1)
			},
			wantMessage: "delta",
		},
		{
			name: "timeout out of range",
			mutate: func(s string) string {
				return strings.Replace(s, `"max_attempts": 2`, `"max_attempts": 2, "timeout_ms": 999999`, 1)
			},
			wantMessage: "timeout_ms",
		},
		{
			name:        "unknown stage",
			mutate:      func(s string) string { return strings.Replace(s, `"after_upstream_response"`, `"mid_flight"`, 1) },
			wantMessage: "stage",
		},
		{
			name:        "unknown action",
			mutate:      func(s string) string { return strings.Replace(s, `"close_client_connection"`, `"corrupt_body"`, 1) },
			wantMessage: "action",
		},
		{
			name:        "action impossible at that stage",
			mutate:      func(s string) string { return strings.Replace(s, `"after_upstream_response"`, `"before_forward"`, 1) },
			wantMessage: "only valid at stage",
		},
		{
			name:        "attempt beyond max_attempts can never fire",
			mutate:      func(s string) string { return strings.Replace(s, `"attempt": 1`, `"attempt": 7`, 1) },
			wantMessage: "exceeds client.max_attempts",
		},
		{
			name:        "attempt zero",
			mutate:      func(s string) string { return strings.Replace(s, `"attempt": 1`, `"attempt": 0`, 1) },
			wantMessage: "attempt",
		},
		{
			name: "delay action without a delay",
			mutate: func(s string) string {
				return strings.Replace(s, `"action": "close_client_connection"`, `"action": "delay_ms"`, 1)
			},
			wantMessage: "delay_ms",
		},
		{
			name: "delay on an action that cannot use one",
			mutate: func(s string) string {
				return strings.Replace(s, `"action": "close_client_connection"`,
					`"action": "close_client_connection", "delay_ms": 10`, 1)
			},
			wantMessage: "must not set a delay",
		},
		{
			name: "two rules for the same attempt and stage",
			mutate: func(s string) string {
				return strings.Replace(s,
					`{"attempt": 1, "stage": "after_upstream_response", "action": "close_client_connection"}`,
					`{"attempt": 1, "stage": "after_upstream_response", "action": "close_client_connection"},`+
						`{"attempt": 1, "stage": "after_upstream_response", "action": "delay_ms", "delay_ms": 5}`, 1)
			},
			wantMessage: "duplicates",
		},
		{
			name:        "unknown invariant type",
			mutate:      func(s string) string { return strings.Replace(s, `"counter_equals"`, `"counter_at_most"`, 1) },
			wantMessage: "invariant type",
		},
		{
			name: "missing expected value",
			mutate: func(s string) string {
				return strings.Replace(s, `"type": "counter_equals", "expected": 1`, `"type": "counter_equals"`, 1)
			},
			wantMessage: "invariant.expected is required",
		},
		{
			name:        "negative expected value",
			mutate:      func(s string) string { return strings.Replace(s, `"expected": 1`, `"expected": -1`, 1) },
			wantMessage: "expected value",
		},
		{
			name:        "trailing content after the document",
			mutate:      func(s string) string { return s + "\n{}" },
			wantMessage: "trailing content",
		},
		{
			name:        "not JSON at all",
			mutate:      func(string) string { return "not json" },
			wantMessage: "invalid scenario JSON",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.mutate(validScenario)))
			if err == nil {
				t.Fatal("Parse accepted an invalid scenario")
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMessage)
			}
		})
	}
}

func TestParseRejectsTooManyFaults(t *testing.T) {
	var faultList strings.Builder
	for i := range MaxFaults + 1 {
		if i > 0 {
			faultList.WriteString(",")
		}
		faultList.WriteString(`{"attempt": 1, "stage": "before_forward", "action": "delay_ms", "delay_ms": 1}`)
	}
	raw := strings.Replace(validScenario,
		`{"attempt": 1, "stage": "after_upstream_response", "action": "close_client_connection"}`,
		faultList.String(), 1)
	if _, err := Parse([]byte(raw)); err == nil {
		t.Fatal("Parse accepted more faults than the bound allows")
	}
}

// TestCheckedInScenariosAreValid keeps the shipped examples honest: a scenario
// file that no longer parses would make the documented demo commands fail.
func TestCheckedInScenariosAreValid(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "scenarios", "*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no scenario files found in scenarios/")
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			sc, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if sc.Name == "" {
				t.Error("scenario has no name")
			}
			if _, err := sc.FixtureMode(); err != nil {
				t.Errorf("FixtureMode: %v", err)
			}
		})
	}
}

func TestLoadReportsMissingFiles(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("Load succeeded for a missing file")
	}
}

func TestLoadRejectsOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	padding := strings.Repeat("x", MaxFileBytes+16)
	if err := os.WriteFile(path, []byte(`{"pad":"`+padding+`"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a file over the size bound")
	}
}
