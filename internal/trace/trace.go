// Package trace records one case in enough detail to run it again.
//
// A trace is self-contained: replay reads nothing else, so it carries the
// validated scenario alongside the decisions that were injected and the outcome
// they produced. What it deliberately does not carry is the request itself.
// Method, path and a digest of the payload are enough to show that the same
// input would be sent, and the owner's rule is explicit that a trace may embed
// the harness's own synthetic configuration -- fixture mode, retry policy,
// invariant, numeric delta -- but never a request body, a header or a
// credential. When scenarios grow arbitrary payloads, a trace must reference an
// external scenario by digest instead of embedding it
// (docs/features/M4-trace-replay.md).
//
// Replay injects the *recorded* decisions rather than re-matching the
// scenario's rules. Deriving them again would mean a trace that no longer
// describes its scenario could never be detected, because both sides of the
// comparison would come from the same place.
//
// A trace is input, trusted exactly as far as a scenario file is. The digests
// bind the file to itself, so a partial hand edit -- a changed invariant, a
// swapped fixture mode -- is refused rather than replayed; they are not
// signatures, and anyone who rewrites a trace wholesale can make this tool
// describe a run that never happened. What replay establishes is that the
// embedded configuration, replayed, reaches the recorded outcome.
package trace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/client"
	"github.com/kyle-luong/http-fault-injection-tester/internal/faults"
	"github.com/kyle-luong/http-fault-injection-tester/internal/fixture"
	"github.com/kyle-luong/http-fault-injection-tester/internal/report"
	"github.com/kyle-luong/http-fault-injection-tester/internal/runner"
	"github.com/kyle-luong/http-fault-injection-tester/internal/scenario"
	"github.com/kyle-luong/http-fault-injection-tester/internal/verify"
)

// SchemaVersion is the only trace version this build writes or accepts.
const SchemaVersion = 1

// MaxFileBytes bounds a trace being read back. One case's trace is a scenario
// plus a handful of decisions, so this is generous by orders of magnitude.
const MaxFileBytes = 1 << 20

// Trace is the per-case replay record.
type Trace struct {
	SchemaVersion int       `json:"schema_version"`
	RunID         string    `json:"run_id"`
	CaseID        string    `json:"case_id"`
	RecordedAt    time.Time `json:"recorded_at"`
	// CaseTimeout is the per-case deadline the recorded run enforced. It
	// decides outcomes -- a case replayed under a different deadline is not
	// the same case -- so replay takes it from here rather than from a flag.
	CaseTimeout time.Duration      `json:"case_timeout_ns"`
	Scenario    *scenario.Scenario `json:"scenario"`
	// ScenarioDigest covers the embedded scenario, so that editing part of it
	// and leaving the rest is refused instead of silently replayed.
	ScenarioDigest string            `json:"scenario_digest"`
	Request        Request           `json:"request"`
	Decisions      []faults.Decision `json:"fault_decisions"`
	Expected       Expected          `json:"expected"`
}

// Request is the operation's identity without its content.
type Request struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// PayloadDigest is "sha256:<hex>" over the bytes the client would send.
	PayloadDigest string `json:"payload_digest"`
}

// Expected is what the recorded run observed, and therefore what a replay has
// to produce again.
type Expected struct {
	Verdict verify.Verdict `json:"verdict"`
	// AttemptCount is a count, deliberately not named "attempts": the result
	// schema uses that key for the array of per-attempt records, which a trace
	// does not carry.
	AttemptCount int `json:"attempt_count"`
	// State is what the oracle read. The key is not "observed_state" because
	// "expected.observed_state" reads as a contradiction.
	State fixture.State `json:"state"`
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// digestOf hashes a value's JSON encoding. The encoding is deterministic for
// the types used here because they are structs with fixed field order and no
// maps.
func digestOf(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// New records one finished case.
func New(
	runID string,
	recordedAt time.Time,
	caseTimeout time.Duration,
	sc *scenario.Scenario,
	o runner.Outcome,
) (Trace, error) {
	if sc == nil {
		return Trace{}, errors.New("trace: scenario is required")
	}
	digest, err := client.PayloadDigest(o.CaseID, sc.Delta())
	if err != nil {
		return Trace{}, fmt.Errorf("trace: %w", err)
	}
	scDigest, err := digestOf(sc)
	if err != nil {
		return Trace{}, fmt.Errorf("trace: %w", err)
	}
	decisions := o.Decisions
	if decisions == nil {
		decisions = []faults.Decision{}
	}
	return Trace{
		SchemaVersion:  SchemaVersion,
		RunID:          runID,
		CaseID:         o.CaseID,
		RecordedAt:     recordedAt,
		CaseTimeout:    caseTimeout,
		Scenario:       sc,
		ScenarioDigest: scDigest,
		Request: Request{
			Method:        sc.Client.Method,
			Path:          sc.Client.Path,
			PayloadDigest: digest,
		},
		Decisions: decisions,
		Expected: Expected{
			Verdict:      o.Verdict(),
			AttemptCount: len(o.Attempts),
			State:        o.Verification.Observed,
		},
	}, nil
}

// Path names the trace file for one case inside dir.
func Path(dir, caseID string) string { return filepath.Join(dir, caseID+".json") }

// WriteRun writes one trace per outcome into dir, with the same atomicity and
// the same refusal to overwrite that the result file gets.
func WriteRun(
	dir string,
	overwrite bool,
	runID string,
	recordedAt time.Time,
	caseTimeout time.Duration,
	sc *scenario.Scenario,
	outcomes []runner.Outcome,
) error {
	for i, o := range outcomes {
		tr, err := New(runID, recordedAt, caseTimeout, sc, o)
		if err != nil {
			return err
		}
		if err := checkCaseID(o.CaseID); err != nil {
			return err
		}
		if err := report.WriteJSON(Path(dir, o.CaseID), overwrite, tr); err != nil {
			// Which traces exist matters to whoever has to clean up or
			// replay one, so say how far this got.
			return fmt.Errorf("wrote %d of %d trace(s): %w", i, len(outcomes), err)
		}
	}
	return nil
}

// CheckWritable reports whether dir could hold traces for the given cases, so
// the CLI can refuse before a run rather than discard a finished one. Case IDs
// are known in advance, which is what makes the refusal checkable this early.
// It returns how many trace files the directory already holds. Those are not
// necessarily the ones this run would replace: a shorter run leaves the longer
// previous run's higher-numbered traces in place, and they then sit beside the
// new ones describing a different run entirely. The caller is expected to say
// so rather than present the directory as this run's.
func CheckWritable(dir string, overwrite bool, caseIDs []string) (int, error) {
	for _, id := range caseIDs {
		if err := checkCaseID(id); err != nil {
			return 0, err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("create trace directory %s: %w", dir, err)
	}

	existing, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return 0, fmt.Errorf("inspect trace directory %s: %w", dir, err)
	}
	if !overwrite && len(existing) > 0 {
		return len(existing), fmt.Errorf("%s already holds %d trace file(s), starting with %s; "+
			"pass --overwrite to replace them", dir, len(existing), filepath.Base(existing[0]))
	}

	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return len(existing), fmt.Errorf("cannot write traces to %s: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	return len(existing), os.Remove(name)
}

// checkCaseID refuses an identifier that would not stay inside the trace
// directory as a plain file name. Generated case IDs always would, but
// --case-id is operator input and reaches this as a path component.
func checkCaseID(id string) error {
	if id == "" || id == "." || id == ".." ||
		id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("case id %q cannot be used as a trace file name", id)
	}
	return nil
}

// Load reads a trace file. A trace this build cannot interpret is rejected
// before anything starts: the PRD classifies a corrupted trace as
// infrastructure failure, never as a finding about the application.
func Load(path string) (Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return Trace{}, fmt.Errorf("open trace %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return Trace{}, fmt.Errorf("read trace %s: %w", path, err)
	}
	if len(data) > MaxFileBytes {
		return Trace{}, fmt.Errorf("trace %s exceeds %d bytes", path, MaxFileBytes)
	}

	tr, err := Parse(data)
	if err != nil {
		return Trace{}, fmt.Errorf("trace %s: %w", path, err)
	}
	return tr, nil
}

// Parse strictly decodes and validates trace JSON.
func Parse(data []byte) (Trace, error) {
	var tr Trace
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tr); err != nil {
		return Trace{}, fmt.Errorf("invalid trace JSON: %w", err)
	}
	if dec.More() {
		return Trace{}, errors.New("unexpected trailing content after trace JSON")
	}
	if err := tr.validate(); err != nil {
		return Trace{}, err
	}
	return tr, nil
}

// validate rejects a trace that cannot be replayed, or whose parts do not agree
// with each other. The two digests do the work: they bind the embedded scenario
// and the request identity to the rest of the file, so editing an invariant, a
// fixture mode or a delta and leaving the digest alone is refused rather than
// replayed under a verdict that came from somewhere else. A file rewritten
// wholesale, digests included, is beyond what any field here can detect.
func (t Trace) validate() error {
	if t.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (want %d)", t.SchemaVersion, SchemaVersion)
	}
	if t.RunID == "" {
		return errors.New("run_id is required")
	}
	if t.RecordedAt.IsZero() {
		return errors.New("recorded_at is required")
	}
	if t.CaseTimeout <= 0 || t.CaseTimeout > runner.MaxCaseTimeout {
		return fmt.Errorf("case_timeout_ns must be between 1 and %d (the %s bound), got %d",
			runner.MaxCaseTimeout.Nanoseconds(), runner.MaxCaseTimeout, t.CaseTimeout.Nanoseconds())
	}
	if err := checkCaseID(t.CaseID); err != nil {
		return err
	}
	if t.Scenario == nil {
		return errors.New("scenario is required")
	}
	if err := t.Scenario.Validate(); err != nil {
		return fmt.Errorf("embedded scenario: %w", err)
	}
	if !digestPattern.MatchString(t.ScenarioDigest) {
		return fmt.Errorf("scenario_digest %q is not a sha256 digest", t.ScenarioDigest)
	}
	wantScenario, err := digestOf(t.Scenario)
	if err != nil {
		return err
	}
	if wantScenario != t.ScenarioDigest {
		return errors.New("scenario_digest does not describe the embedded scenario; " +
			"the trace has been edited, or was written by a build whose scenario schema differs")
	}
	if t.Request.Method != t.Scenario.Client.Method || t.Request.Path != t.Scenario.Client.Path {
		return fmt.Errorf("request %s %s does not match the embedded scenario's client policy %s %s",
			t.Request.Method, t.Request.Path, t.Scenario.Client.Method, t.Scenario.Client.Path)
	}
	if !digestPattern.MatchString(t.Request.PayloadDigest) {
		return fmt.Errorf("payload_digest %q is not a sha256 digest", t.Request.PayloadDigest)
	}
	want, err := client.PayloadDigest(t.CaseID, t.Scenario.Delta())
	if err != nil {
		return err
	}
	if want != t.Request.PayloadDigest {
		return errors.New("payload_digest does not describe the request this scenario and case " +
			"would send; the trace has been edited, or the request format changed since it was written")
	}

	for i, d := range t.Decisions {
		if d.CaseID != t.CaseID {
			return fmt.Errorf("fault_decisions[%d] belongs to case %q, not %q", i, d.CaseID, t.CaseID)
		}
		if d.Attempt > t.Scenario.Client.MaxAttempts {
			return fmt.Errorf("fault_decisions[%d]: attempt %d exceeds the policy's %d attempt(s)",
				i, d.Attempt, t.Scenario.Client.MaxAttempts)
		}
	}
	// NewSchedule enforces every rule-level bound and rejects two decisions
	// covering one (case, attempt, stage), which could not have been recorded
	// and could not be replayed unambiguously.
	if _, err := faults.NewSchedule(t.Rules()); err != nil {
		return fmt.Errorf("recorded decisions cannot be replayed: %w", err)
	}

	switch t.Expected.Verdict {
	case verify.VerdictPass, verify.VerdictFail, verify.VerdictInconclusive, verify.VerdictHarnessError:
	default:
		return fmt.Errorf("unknown expected verdict %q", t.Expected.Verdict)
	}
	if t.Expected.AttemptCount < 0 || t.Expected.AttemptCount > t.Scenario.Client.MaxAttempts {
		return fmt.Errorf("expected.attempt_count %d is outside the policy's 0 to %d",
			t.Expected.AttemptCount, t.Scenario.Client.MaxAttempts)
	}
	// An unstarted case carries no state at all, which is why an empty case ID
	// is allowed here; a state naming a different case is not.
	if id := t.Expected.State.CaseID; id != "" && id != t.CaseID {
		return fmt.Errorf("expected.state belongs to case %q, not %q", id, t.CaseID)
	}
	return nil
}

// Rules is the schedule replay injects: exactly the recorded decisions. The
// slice is never nil, because nil would mean "use the scenario's rules" to the
// runner, and a case that was never faulted must replay without faults.
func (t Trace) Rules() []faults.Rule {
	rules := make([]faults.Rule, 0, len(t.Decisions))
	for _, d := range t.Decisions {
		rules = append(rules, faults.Rule{
			CaseID:  d.CaseID,
			Attempt: d.Attempt,
			Stage:   d.Stage,
			Action:  d.Action,
			Delay:   d.Delay,
		})
	}
	return rules
}

// Compare reports every way a replayed outcome differs from the record, in
// plain enough terms to tell a reproducibility failure from a timing-dependent
// scenario. An empty result means the replay reproduced the case.
//
// Seq is deliberately not compared: it numbers decisions across a whole run, so
// a case replayed on its own legitimately starts from one.
func Compare(t Trace, o runner.Outcome) []string {
	var diffs []string

	if len(o.Decisions) != len(t.Decisions) {
		diffs = append(diffs, fmt.Sprintf("injected %d fault decision(s); the trace recorded %d",
			len(o.Decisions), len(t.Decisions)))
	}
	for i := range min(len(o.Decisions), len(t.Decisions)) {
		got, want := o.Decisions[i], t.Decisions[i]
		if got.Attempt != want.Attempt || got.Stage != want.Stage ||
			got.Action != want.Action || got.Delay != want.Delay {
			diffs = append(diffs, fmt.Sprintf(
				"decision %d: injected attempt=%d stage=%s action=%s delay=%s; recorded attempt=%d stage=%s action=%s delay=%s",
				i+1, got.Attempt, got.Stage, got.Action, got.Delay,
				want.Attempt, want.Stage, want.Action, want.Delay))
		}
	}

	if v := o.Verdict(); v != t.Expected.Verdict {
		diffs = append(diffs, fmt.Sprintf("verdict: replayed %s; recorded %s", v, t.Expected.Verdict))
	}
	if len(o.Attempts) != t.Expected.AttemptCount {
		diffs = append(diffs, fmt.Sprintf("attempts: replayed %d; recorded %d",
			len(o.Attempts), t.Expected.AttemptCount))
	}

	observed := o.Verification.Observed
	if observed.Counter != t.Expected.State.Counter {
		diffs = append(diffs, fmt.Sprintf("counter: replayed %d; recorded %d",
			observed.Counter, t.Expected.State.Counter))
	}
	if !slices.Equal(observed.Operations, t.Expected.State.Operations) {
		diffs = append(diffs, fmt.Sprintf("applied operations: replayed %v; recorded %v",
			observed.Operations, t.Expected.State.Operations))
	}
	return diffs
}
