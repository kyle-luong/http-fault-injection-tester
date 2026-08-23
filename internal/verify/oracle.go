// Package verify evaluates a scenario's invariant against observed fixture
// state.
//
// The whole point of the tool lives here: a verdict is derived from state the
// application actually holds, never from the HTTP status the client saw. A
// client that received a transport error has learned nothing about whether the
// operation committed, so only the oracle can decide.
package verify

import (
	"fmt"

	"github.com/kyle-luong/http-fault-injection-tester/internal/fixture"
)

// InvariantType names a supported check. The MVP implements exactly one.
type InvariantType string

// CounterEquals asserts the final counter value for a case.
const CounterEquals InvariantType = "counter_equals"

// MaxExpectedCounter bounds the value a scenario may assert.
const MaxExpectedCounter = 1_000_000

// ParseInvariantType validates an invariant name from a scenario file.
func ParseInvariantType(s string) (InvariantType, error) {
	if InvariantType(s) == CounterEquals {
		return CounterEquals, nil
	}
	return "", fmt.Errorf("unknown invariant type %q (want %q)", s, CounterEquals)
}

// Invariant is the check a scenario asserts after settlement.
type Invariant struct {
	Type     InvariantType `json:"type"`
	Expected int           `json:"expected"`
}

// Validate reports whether the invariant is checkable.
func (i Invariant) Validate() error {
	if _, err := ParseInvariantType(string(i.Type)); err != nil {
		return err
	}
	if i.Expected < 0 || i.Expected > MaxExpectedCounter {
		return fmt.Errorf("invariant expected value must be between 0 and %d, got %d",
			MaxExpectedCounter, i.Expected)
	}
	return nil
}

// Verdict classifies a case outcome. The distinction between Fail and the two
// non-failure outcomes is a hard requirement: a harness that cannot reach the
// oracle has not found an application bug.
type Verdict string

// Possible verdicts.
const (
	// VerdictPass means the invariant held.
	VerdictPass Verdict = "pass"
	// VerdictFail means the application under test violated the invariant.
	VerdictFail Verdict = "fail"
	// VerdictInconclusive means the outcome could not be determined, for
	// example because upstream work had not settled before the deadline.
	VerdictInconclusive Verdict = "inconclusive"
	// VerdictHarnessError means the test infrastructure failed.
	VerdictHarnessError Verdict = "harness_error"
)

// Result is an evaluated invariant.
type Result struct {
	Verdict   Verdict       `json:"verdict"`
	Invariant Invariant     `json:"invariant"`
	Observed  fixture.State `json:"observed_state"`
	Detail    string        `json:"detail"`
}

// Evaluate checks the invariant against observed state.
func Evaluate(inv Invariant, state fixture.State) Result {
	result := Result{Invariant: inv, Observed: state}

	if err := inv.Validate(); err != nil {
		result.Verdict = VerdictHarnessError
		result.Detail = "unusable invariant: " + err.Error()
		return result
	}

	switch inv.Type {
	case CounterEquals:
		if state.Counter == inv.Expected {
			result.Verdict = VerdictPass
			result.Detail = fmt.Sprintf("counter=%d as expected after %d applied operation(s)",
				state.Counter, len(state.Operations))
			return result
		}
		result.Verdict = VerdictFail
		result.Detail = fmt.Sprintf("expected counter=%d; observed counter=%d after %d applied operation(s) %v",
			inv.Expected, state.Counter, len(state.Operations), state.Operations)
		return result
	default:
		result.Verdict = VerdictHarnessError
		result.Detail = fmt.Sprintf("invariant type %q has no evaluator", inv.Type)
		return result
	}
}

// Inconclusive builds a result for an outcome that could not be determined.
func Inconclusive(inv Invariant, state fixture.State, reason string) Result {
	return Result{
		Verdict:   VerdictInconclusive,
		Invariant: inv,
		Observed:  state,
		Detail:    reason,
	}
}

// HarnessError builds a result for a test-infrastructure failure.
func HarnessError(inv Invariant, reason string) Result {
	return Result{
		Verdict:   VerdictHarnessError,
		Invariant: inv,
		Detail:    reason,
	}
}
