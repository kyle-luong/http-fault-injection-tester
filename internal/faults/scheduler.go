// Package faults decides which fault to inject, deterministically.
//
// Decisions are keyed by (case ID, attempt number, stage) and never by
// wall-clock or goroutine ordering. That is what makes a recorded decision log
// reproducible: the same schedule replayed against the same controlled fixture
// injects the same faults. It is explicitly not a claim that Go scheduling,
// port allocation, or network timing are reproducible.
package faults

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Stage names an observable point in the proxy's handling of one attempt.
type Stage string

// Supported stages.
const (
	// StageBeforeForward is evaluated after the proxy has the complete client
	// request and before it contacts the upstream. The upstream has not seen
	// the request yet.
	StageBeforeForward Stage = "before_forward"
	// StageAfterUpstreamResponse is evaluated once the upstream response has
	// been fully read. The upstream has done its work; the client has not seen
	// the result yet.
	StageAfterUpstreamResponse Stage = "after_upstream_response"
)

// Action names what the proxy does when a rule matches.
type Action string

// Supported actions. The MVP implements exactly these.
const (
	// ActionRejectConnection closes the client connection without forwarding.
	ActionRejectConnection Action = "reject_connection"
	// ActionCloseClientConnection closes the client connection after a
	// complete upstream response, producing a lost response.
	ActionCloseClientConnection Action = "close_client_connection"
	// ActionDelay sleeps before continuing, which may trip a client deadline.
	ActionDelay Action = "delay_ms"
)

// MaxDelay bounds a single injected delay so a scenario cannot hang a run.
const MaxDelay = 60 * time.Second

// maxRecordedDecisions bounds the in-memory decision log.
const maxRecordedDecisions = 10000

// ParseStage validates a stage name from a scenario file.
func ParseStage(s string) (Stage, error) {
	switch Stage(s) {
	case StageBeforeForward:
		return StageBeforeForward, nil
	case StageAfterUpstreamResponse:
		return StageAfterUpstreamResponse, nil
	default:
		return "", fmt.Errorf("unknown fault stage %q (want %q or %q)",
			s, StageBeforeForward, StageAfterUpstreamResponse)
	}
}

// ParseAction validates an action name from a scenario file.
func ParseAction(s string) (Action, error) {
	switch Action(s) {
	case ActionRejectConnection:
		return ActionRejectConnection, nil
	case ActionCloseClientConnection:
		return ActionCloseClientConnection, nil
	case ActionDelay:
		return ActionDelay, nil
	default:
		return "", fmt.Errorf("unknown fault action %q (want %q, %q or %q)",
			s, ActionRejectConnection, ActionCloseClientConnection, ActionDelay)
	}
}

// Rule matches one attempt at one stage. An empty CaseID matches any case; a
// rule naming a case always wins over a wildcard rule for the same
// (attempt, stage).
type Rule struct {
	CaseID  string
	Attempt int
	Stage   Stage
	Action  Action
	Delay   time.Duration
}

// Validate reports whether the rule is self-consistent and implementable.
func (r Rule) Validate() error {
	if r.Attempt < 1 {
		return fmt.Errorf("fault attempt must be >= 1, got %d", r.Attempt)
	}
	if _, err := ParseStage(string(r.Stage)); err != nil {
		return err
	}
	if _, err := ParseAction(string(r.Action)); err != nil {
		return err
	}

	switch r.Action {
	case ActionRejectConnection:
		if r.Stage != StageBeforeForward {
			return fmt.Errorf("action %q is only valid at stage %q, got %q",
				r.Action, StageBeforeForward, r.Stage)
		}
	case ActionCloseClientConnection:
		if r.Stage != StageAfterUpstreamResponse {
			return fmt.Errorf("action %q is only valid at stage %q, got %q",
				r.Action, StageAfterUpstreamResponse, r.Stage)
		}
	}

	if r.Action == ActionDelay {
		if r.Delay <= 0 {
			return fmt.Errorf("action %q requires a positive delay", ActionDelay)
		}
		if r.Delay > MaxDelay {
			return fmt.Errorf("delay %s exceeds the %s bound", r.Delay, MaxDelay)
		}
	} else if r.Delay != 0 {
		return fmt.Errorf("action %q must not set a delay", r.Action)
	}

	return nil
}

// Decision records one injected fault, in injection order.
type Decision struct {
	Seq     int           `json:"seq"`
	CaseID  string        `json:"case_id"`
	Attempt int           `json:"attempt"`
	Stage   Stage         `json:"stage"`
	Action  Action        `json:"action"`
	Delay   time.Duration `json:"delay_ns,omitempty"`
}

// Schedule matches rules and records the resulting decisions. It is safe for
// concurrent use by multiple cases.
type Schedule struct {
	rules []Rule

	mu        sync.Mutex
	decisions []Decision
	dropped   int
}

// NewSchedule validates the rule set and returns a ready schedule. Two rules
// covering the same (CaseID, Attempt, Stage) are rejected: an ambiguous
// schedule cannot be deterministic.
func NewSchedule(rules []Rule) (*Schedule, error) {
	type key struct {
		caseID  string
		attempt int
		stage   Stage
	}
	seen := make(map[key]struct{}, len(rules))

	for i, r := range rules {
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("fault rule %d: %w", i, err)
		}
		k := key{r.CaseID, r.Attempt, r.Stage}
		if _, dup := seen[k]; dup {
			return nil, fmt.Errorf("fault rule %d duplicates case=%q attempt=%d stage=%q",
				i, r.CaseID, r.Attempt, r.Stage)
		}
		seen[k] = struct{}{}
	}

	return &Schedule{rules: append([]Rule(nil), rules...)}, nil
}

// Decide returns the fault to inject for one attempt at one stage, recording it
// when a rule matches. The lookup is pure except for the decision log.
func (s *Schedule) Decide(caseID string, attempt int, stage Stage) (Decision, bool) {
	matched, ok := s.match(caseID, attempt, stage)
	if !ok {
		return Decision{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	d := Decision{
		Seq:     len(s.decisions) + s.dropped + 1,
		CaseID:  caseID,
		Attempt: attempt,
		Stage:   matched.Stage,
		Action:  matched.Action,
		Delay:   matched.Delay,
	}
	if len(s.decisions) < maxRecordedDecisions {
		s.decisions = append(s.decisions, d)
	} else {
		s.dropped++
	}
	return d, true
}

// match prefers an exact case match over a wildcard rule.
func (s *Schedule) match(caseID string, attempt int, stage Stage) (Rule, bool) {
	var wildcard Rule
	haveWildcard := false

	for _, r := range s.rules {
		if r.Attempt != attempt || r.Stage != stage {
			continue
		}
		if r.CaseID == caseID {
			return r, true
		}
		if r.CaseID == "" && !haveWildcard {
			wildcard, haveWildcard = r, true
		}
	}
	return wildcard, haveWildcard
}

// Decisions returns a copy of the recorded decisions in injection order.
func (s *Schedule) Decisions() []Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Decision(nil), s.decisions...)
}

// DecisionsForCase returns the recorded decisions belonging to one case.
func (s *Schedule) DecisionsForCase(caseID string) []Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Decision, 0, len(s.decisions))
	for _, d := range s.decisions {
		if d.CaseID == caseID {
			out = append(out, d)
		}
	}
	return out
}

// DroppedDecisions reports decisions omitted from the log because the bound was
// reached. A non-zero value means the log is incomplete, which replay must not
// silently ignore.
func (s *Schedule) DroppedDecisions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Sleep waits for d or until ctx is done, reporting ctx's error if it wins.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
