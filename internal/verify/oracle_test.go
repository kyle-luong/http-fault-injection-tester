package verify

import (
	"strings"
	"testing"

	"github.com/kyle-luong/http-fault-injection-tester/internal/fixture"
)

func TestEvaluatePassesWhenTheCounterMatches(t *testing.T) {
	result := Evaluate(
		Invariant{Type: CounterEquals, Expected: 1},
		fixture.State{CaseID: "case-1", Counter: 1, Operations: []string{"case-1-key"}},
	)
	if result.Verdict != VerdictPass {
		t.Fatalf("verdict = %q, want %q (detail: %s)", result.Verdict, VerdictPass, result.Detail)
	}
	if result.Observed.Counter != 1 {
		t.Errorf("observed state was not carried into the result: %+v", result.Observed)
	}
}

// TestEvaluateFailsOnADuplicatedOperation is the verdict behind GO-AC02: the
// finding comes from observed state, not from the client's HTTP status.
func TestEvaluateFailsOnADuplicatedOperation(t *testing.T) {
	result := Evaluate(
		Invariant{Type: CounterEquals, Expected: 1},
		fixture.State{CaseID: "case-1", Counter: 2, Operations: []string{"case-1-key", "case-1-key"}},
	)
	if result.Verdict != VerdictFail {
		t.Fatalf("verdict = %q, want %q", result.Verdict, VerdictFail)
	}
	for _, want := range []string{"expected counter=1", "observed counter=2"} {
		if !strings.Contains(result.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", result.Detail, want)
		}
	}
}

func TestEvaluateFailsWhenNothingWasApplied(t *testing.T) {
	result := Evaluate(
		Invariant{Type: CounterEquals, Expected: 1},
		fixture.State{CaseID: "case-1", Counter: 0, Operations: []string{}},
	)
	if result.Verdict != VerdictFail {
		t.Errorf("verdict = %q, want %q", result.Verdict, VerdictFail)
	}
}

func TestEvaluateRejectsAnUnusableInvariant(t *testing.T) {
	tests := []struct {
		name string
		inv  Invariant
	}{
		{"unknown type", Invariant{Type: "counter_at_most", Expected: 1}},
		{"empty type", Invariant{Expected: 1}},
		{"negative expectation", Invariant{Type: CounterEquals, Expected: -1}},
		{"expectation over the bound", Invariant{Type: CounterEquals, Expected: MaxExpectedCounter + 1}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := Evaluate(tc.inv, fixture.State{Counter: 1})
			// An unusable invariant is a harness defect, never a finding about
			// the application under test.
			if result.Verdict != VerdictHarnessError {
				t.Errorf("verdict = %q, want %q", result.Verdict, VerdictHarnessError)
			}
		})
	}
}

func TestInvariantValidate(t *testing.T) {
	if err := (Invariant{Type: CounterEquals, Expected: 0}).Validate(); err != nil {
		t.Errorf("expected=0 rejected: %v", err)
	}
	if err := (Invariant{Type: CounterEquals, Expected: MaxExpectedCounter}).Validate(); err != nil {
		t.Errorf("expected at the bound rejected: %v", err)
	}
	if err := (Invariant{Type: "nope", Expected: 1}).Validate(); err == nil {
		t.Error("unknown invariant type accepted")
	}
}

func TestInconclusiveAndHarnessErrorCarryTheirReason(t *testing.T) {
	inv := Invariant{Type: CounterEquals, Expected: 1}

	inconclusive := Inconclusive(inv, fixture.State{CaseID: "case-1"}, "did not settle")
	if inconclusive.Verdict != VerdictInconclusive {
		t.Errorf("verdict = %q, want %q", inconclusive.Verdict, VerdictInconclusive)
	}
	if inconclusive.Detail != "did not settle" {
		t.Errorf("detail = %q, want the supplied reason", inconclusive.Detail)
	}

	harness := HarnessError(inv, "fixture would not start")
	if harness.Verdict != VerdictHarnessError {
		t.Errorf("verdict = %q, want %q", harness.Verdict, VerdictHarnessError)
	}
	if harness.Detail != "fixture would not start" {
		t.Errorf("detail = %q, want the supplied reason", harness.Detail)
	}
}

func TestParseInvariantType(t *testing.T) {
	if _, err := ParseInvariantType("counter_equals"); err != nil {
		t.Errorf("ParseInvariantType(counter_equals): %v", err)
	}
	if _, err := ParseInvariantType("operations_length_equals"); err == nil {
		t.Error("ParseInvariantType accepted an unimplemented invariant")
	}
}
