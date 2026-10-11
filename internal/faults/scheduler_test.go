package faults

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRuleValidate(t *testing.T) {
	tests := []struct {
		name    string
		rule    Rule
		wantErr bool
	}{
		{
			name: "close after upstream response is valid",
			rule: Rule{Attempt: 1, Stage: StageAfterUpstreamResponse, Action: ActionCloseClientConnection},
		},
		{
			name: "reject before forward is valid",
			rule: Rule{Attempt: 1, Stage: StageBeforeForward, Action: ActionRejectConnection},
		},
		{
			name: "delay is valid at either stage",
			rule: Rule{Attempt: 2, Stage: StageBeforeForward, Action: ActionDelay, Delay: time.Millisecond},
		},
		{
			name:    "attempt zero is impossible",
			rule:    Rule{Attempt: 0, Stage: StageBeforeForward, Action: ActionRejectConnection},
			wantErr: true,
		},
		{
			name:    "negative attempt is impossible",
			rule:    Rule{Attempt: -1, Stage: StageBeforeForward, Action: ActionRejectConnection},
			wantErr: true,
		},
		{
			name:    "unknown stage",
			rule:    Rule{Attempt: 1, Stage: "mid_flight", Action: ActionRejectConnection},
			wantErr: true,
		},
		{
			name:    "unknown action",
			rule:    Rule{Attempt: 1, Stage: StageBeforeForward, Action: "corrupt_body"},
			wantErr: true,
		},
		{
			name:    "reject cannot run after the upstream responded",
			rule:    Rule{Attempt: 1, Stage: StageAfterUpstreamResponse, Action: ActionRejectConnection},
			wantErr: true,
		},
		{
			name:    "close cannot run before forwarding",
			rule:    Rule{Attempt: 1, Stage: StageBeforeForward, Action: ActionCloseClientConnection},
			wantErr: true,
		},
		{
			name:    "delay requires a positive duration",
			rule:    Rule{Attempt: 1, Stage: StageBeforeForward, Action: ActionDelay},
			wantErr: true,
		},
		{
			name:    "delay is bounded",
			rule:    Rule{Attempt: 1, Stage: StageBeforeForward, Action: ActionDelay, Delay: MaxDelay + time.Second},
			wantErr: true,
		},
		{
			name:    "non-delay actions must not set a delay",
			rule:    Rule{Attempt: 1, Stage: StageBeforeForward, Action: ActionRejectConnection, Delay: time.Second},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewScheduleRejectsAmbiguousRules(t *testing.T) {
	// Two rules for the same (case, attempt, stage) would make the injected
	// fault depend on rule order rather than on the schedule's meaning.
	_, err := NewSchedule([]Rule{
		{Attempt: 1, Stage: StageAfterUpstreamResponse, Action: ActionCloseClientConnection},
		{Attempt: 1, Stage: StageAfterUpstreamResponse, Action: ActionDelay, Delay: time.Second},
	})
	if err == nil {
		t.Fatal("NewSchedule accepted two rules for the same attempt and stage")
	}
}

func TestNewScheduleAllowsSameAttemptAtDifferentStages(t *testing.T) {
	if _, err := NewSchedule([]Rule{
		{Attempt: 1, Stage: StageBeforeForward, Action: ActionDelay, Delay: time.Millisecond},
		{Attempt: 1, Stage: StageAfterUpstreamResponse, Action: ActionCloseClientConnection},
	}); err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}
}

func TestNewScheduleValidatesEveryRule(t *testing.T) {
	_, err := NewSchedule([]Rule{
		{Attempt: 1, Stage: StageAfterUpstreamResponse, Action: ActionCloseClientConnection},
		{Attempt: 2, Stage: StageBeforeForward, Action: ActionCloseClientConnection},
	})
	if err == nil {
		t.Fatal("NewSchedule accepted an invalid second rule")
	}
}

func TestDecideMatchesOnlyTheConfiguredAttemptAndStage(t *testing.T) {
	sched, err := NewSchedule([]Rule{
		{Attempt: 1, Stage: StageAfterUpstreamResponse, Action: ActionCloseClientConnection},
	})
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}

	if _, ok := sched.Decide("case-1", 1, StageBeforeForward); ok {
		t.Error("matched before_forward, which no rule covers")
	}
	decision, ok := sched.Decide("case-1", 1, StageAfterUpstreamResponse)
	if !ok {
		t.Fatal("did not match attempt 1 at after_upstream_response")
	}
	if decision.Action != ActionCloseClientConnection {
		t.Errorf("action = %q, want %q", decision.Action, ActionCloseClientConnection)
	}
	if _, ok := sched.Decide("case-1", 2, StageAfterUpstreamResponse); ok {
		t.Error("matched attempt 2, which no rule covers")
	}
}

// TestDecideIsDeterministic covers GO-AC06: the same schedule consulted with
// the same (case, attempt, stage) inputs must always choose the same faults.
func TestDecideIsDeterministic(t *testing.T) {
	rules := []Rule{
		{Attempt: 1, Stage: StageBeforeForward, Action: ActionDelay, Delay: 5 * time.Millisecond},
		{Attempt: 2, Stage: StageAfterUpstreamResponse, Action: ActionCloseClientConnection},
	}
	consults := []struct {
		caseID  string
		attempt int
		stage   Stage
	}{
		{"case-1", 1, StageBeforeForward},
		{"case-1", 1, StageAfterUpstreamResponse},
		{"case-1", 2, StageBeforeForward},
		{"case-1", 2, StageAfterUpstreamResponse},
		{"case-2", 1, StageBeforeForward},
	}

	var reference []Decision
	for run := range 5 {
		sched, err := NewSchedule(rules)
		if err != nil {
			t.Fatalf("NewSchedule: %v", err)
		}
		for _, c := range consults {
			sched.Decide(c.caseID, c.attempt, c.stage)
		}
		got := sched.Decisions()
		if run == 0 {
			reference = got
			continue
		}
		if len(got) != len(reference) {
			t.Fatalf("run %d produced %d decisions, want %d", run, len(got), len(reference))
		}
		for i := range got {
			if got[i] != reference[i] {
				t.Errorf("run %d decision %d = %+v, want %+v", run, i, got[i], reference[i])
			}
		}
	}

	if len(reference) != 3 {
		t.Fatalf("recorded %d decisions, want 3 (two for case-1, one for case-2)", len(reference))
	}
	for i, d := range reference {
		if d.Seq != i+1 {
			t.Errorf("decision %d has Seq %d, want %d", i, d.Seq, i+1)
		}
	}
}

func TestCaseSpecificRuleWinsOverWildcard(t *testing.T) {
	sched, err := NewSchedule([]Rule{
		{Attempt: 1, Stage: StageAfterUpstreamResponse, Action: ActionDelay, Delay: time.Millisecond},
		{CaseID: "case-special", Attempt: 1, Stage: StageAfterUpstreamResponse, Action: ActionCloseClientConnection},
	})
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}

	general, _ := sched.Decide("case-ordinary", 1, StageAfterUpstreamResponse)
	if general.Action != ActionDelay {
		t.Errorf("ordinary case action = %q, want the wildcard %q", general.Action, ActionDelay)
	}
	specific, _ := sched.Decide("case-special", 1, StageAfterUpstreamResponse)
	if specific.Action != ActionCloseClientConnection {
		t.Errorf("special case action = %q, want the case-specific %q",
			specific.Action, ActionCloseClientConnection)
	}
}

func TestDecisionsForCaseFiltersByCase(t *testing.T) {
	sched, err := NewSchedule([]Rule{
		{Attempt: 1, Stage: StageAfterUpstreamResponse, Action: ActionCloseClientConnection},
	})
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}
	sched.Decide("case-1", 1, StageAfterUpstreamResponse)
	sched.Decide("case-2", 1, StageAfterUpstreamResponse)

	only := sched.DecisionsForCase("case-1")
	if len(only) != 1 || only[0].CaseID != "case-1" {
		t.Fatalf("DecisionsForCase(case-1) = %+v, want exactly case-1's decision", only)
	}
	if got := len(sched.Decisions()); got != 2 {
		t.Errorf("Decisions() returned %d entries, want 2", got)
	}
	if got := sched.DroppedDecisions(); got != 0 {
		t.Errorf("DroppedDecisions() = %d, want 0", got)
	}
}

func TestDecisionLogIsBounded(t *testing.T) {
	sched, err := NewSchedule([]Rule{
		{Attempt: 1, Stage: StageBeforeForward, Action: ActionRejectConnection},
	})
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}
	for range maxRecordedDecisions + 10 {
		sched.Decide("case-1", 1, StageBeforeForward)
	}
	if got := len(sched.Decisions()); got != maxRecordedDecisions {
		t.Errorf("recorded %d decisions, want the bound %d", got, maxRecordedDecisions)
	}
	if got := sched.DroppedDecisions(); got != 10 {
		t.Errorf("DroppedDecisions() = %d, want 10 so replay can tell the log is incomplete", got)
	}
}

// TestPerCaseDecisionsSurviveTheLogBound is the defect the bound above used to
// cause. A case whose decisions fell past the whole-run bound recorded an empty
// list, and an empty list does not read as "truncated" -- it reads as "no fault
// was injected". A trace written from one replayed clean and reported that it
// had reproduced the run.
//
// Two faults per case and 10,000 cases is inside the documented CLI bounds, so
// this was reachable without any hand-editing.
func TestPerCaseDecisionsSurviveTheLogBound(t *testing.T) {
	sched, err := NewSchedule([]Rule{
		{Attempt: 1, Stage: StageBeforeForward, Action: ActionRejectConnection},
	})
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}

	const cases = maxRecordedDecisions + 100
	for i := range cases {
		sched.Decide(fmt.Sprintf("case-%05d", i+1), 1, StageBeforeForward)
	}

	if got := sched.DroppedDecisions(); got != 100 {
		t.Fatalf("DroppedDecisions() = %d, want 100", got)
	}
	// The last case is well past the bound, which is exactly where the record
	// used to be lost.
	last := fmt.Sprintf("case-%05d", cases)
	decisions := sched.DecisionsForCase(last)
	if len(decisions) != 1 {
		t.Fatalf("DecisionsForCase(%s) returned %d decisions, want 1; a case past the "+
			"whole-run log bound still has its own history", last, len(decisions))
	}
	if decisions[0].Action != ActionRejectConnection || decisions[0].Attempt != 1 {
		t.Errorf("DecisionsForCase(%s)[0] = %+v, want the injected rule", last, decisions[0])
	}
}

func TestConcurrentDecideIsSafe(t *testing.T) {
	sched, err := NewSchedule([]Rule{
		{Attempt: 1, Stage: StageAfterUpstreamResponse, Action: ActionCloseClientConnection},
	})
	if err != nil {
		t.Fatalf("NewSchedule: %v", err)
	}

	const workers = 16
	done := make(chan struct{})
	for i := range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			sched.Decide("case-1", 1, StageAfterUpstreamResponse)
			sched.Decide("case-1", 1, StageBeforeForward)
			_ = sched.Decisions()
			_ = sched.DecisionsForCase("case-1")
			_ = i
		}()
	}
	for range workers {
		<-done
	}
	if got := len(sched.Decisions()); got != workers {
		t.Errorf("recorded %d decisions, want %d", got, workers)
	}
}

func TestParseStageAndAction(t *testing.T) {
	if _, err := ParseStage("before_forward"); err != nil {
		t.Errorf("ParseStage(before_forward): %v", err)
	}
	if _, err := ParseStage("after_upstream_response"); err != nil {
		t.Errorf("ParseStage(after_upstream_response): %v", err)
	}
	if _, err := ParseStage("during_forward"); err == nil {
		t.Error("ParseStage accepted an unknown stage")
	}
	for _, name := range []string{"reject_connection", "close_client_connection", "delay_ms"} {
		if _, err := ParseAction(name); err != nil {
			t.Errorf("ParseAction(%s): %v", name, err)
		}
	}
	if _, err := ParseAction("truncate_body"); err == nil {
		t.Error("ParseAction accepted an unimplemented action")
	}
}

func TestSleepRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Hour); err == nil {
		t.Fatal("Sleep ignored a cancelled context")
	}

	if err := Sleep(context.Background(), 0); err != nil {
		t.Errorf("Sleep(0) = %v, want nil", err)
	}
	start := time.Now()
	if err := Sleep(context.Background(), 10*time.Millisecond); err != nil {
		t.Errorf("Sleep: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Errorf("Sleep returned after %s, want at least 10ms", elapsed)
	}
}
