// Package scenario parses and validates the declarative scenario contract.
//
// Parsing is strict on purpose: an unknown key is almost always a typo that
// would silently disable a fault, and a scenario that silently does nothing is
// worse than one that refuses to run. Everything is rejected before any server
// starts, so an invalid scenario can never produce a verdict.
package scenario

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/faults"
	"github.com/kyle-luong/http-fault-injection-tester/internal/fixture"
	"github.com/kyle-luong/http-fault-injection-tester/internal/verify"
)

// SchemaVersion is the only scenario version this build accepts.
const SchemaVersion = 1

// MaxFileBytes bounds a scenario file so a bad path cannot exhaust memory.
const MaxFileBytes = 256 << 10

// MaxFaults bounds the rules one scenario may declare.
const MaxFaults = 32

// DefaultDelta is used when a scenario omits the operation delta.
const DefaultDelta = 1

// Scenario is the v1 scenario contract.
type Scenario struct {
	SchemaVersion int        `json:"schema_version"`
	Name          string     `json:"name"`
	Fixture       string     `json:"fixture"`
	Client        ClientSpec `json:"client"`
	Faults        []Fault    `json:"faults"`
	Invariant     Invariant  `json:"invariant"`
}

// ClientSpec is the scenario's retry policy.
type ClientSpec struct {
	Method                string `json:"method"`
	Path                  string `json:"path"`
	MaxAttempts           int    `json:"max_attempts"`
	RetryOnTransportError bool   `json:"retry_on_transport_error"`
	ReuseIdempotencyKey   bool   `json:"reuse_idempotency_key"`
	// Delta is optional and defaults to DefaultDelta.
	Delta *int `json:"delta,omitempty"`
	// TimeoutMS optionally bounds a single attempt.
	TimeoutMS *int `json:"timeout_ms,omitempty"`
}

// Fault is one scheduled injection.
type Fault struct {
	Attempt int    `json:"attempt"`
	Stage   string `json:"stage"`
	Action  string `json:"action"`
	DelayMS *int   `json:"delay_ms,omitempty"`
}

// Invariant is the state assertion checked after settlement. Expected is a
// pointer so that an omitted field is an error rather than a silent zero.
type Invariant struct {
	Type     string `json:"type"`
	Expected *int   `json:"expected"`
}

// Load reads and validates a scenario file.
func Load(path string) (*Scenario, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open scenario %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read scenario %s: %w", path, err)
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("scenario %s exceeds %d bytes", path, MaxFileBytes)
	}

	sc, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("scenario %s: %w", path, err)
	}
	return sc, nil
}

// Parse strictly decodes and validates scenario JSON.
func Parse(data []byte) (*Scenario, error) {
	var sc Scenario
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("invalid scenario JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("unexpected trailing content after scenario JSON")
	}
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	return &sc, nil
}

// Validate checks every enum, bound, and cross-field relationship.
func (s *Scenario) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (want %d)", s.SchemaVersion, SchemaVersion)
	}
	if s.Name == "" {
		return errors.New("name is required")
	}
	if len(s.Name) > 128 {
		return errors.New("name exceeds 128 bytes")
	}
	if _, err := fixture.ParseMode(s.Fixture); err != nil {
		return err
	}
	if err := s.Client.validate(); err != nil {
		return err
	}
	if err := s.validateFaults(); err != nil {
		return err
	}
	if s.Invariant.Expected == nil {
		return errors.New("invariant.expected is required")
	}
	return verify.Invariant{
		Type:     verify.InvariantType(s.Invariant.Type),
		Expected: *s.Invariant.Expected,
	}.Validate()
}

func (c ClientSpec) validate() error {
	if c.Method != "POST" {
		return fmt.Errorf("client.method %q is not supported; the v1 fixture contract is POST only", c.Method)
	}
	if !strings.HasPrefix(c.Path, "/") {
		return fmt.Errorf("client.path %q must be absolute", c.Path)
	}
	if strings.HasPrefix(c.Path, "/__test/") {
		return fmt.Errorf("client.path %q is reserved for harness control endpoints", c.Path)
	}
	if c.MaxAttempts < 1 {
		return fmt.Errorf("client.max_attempts must be >= 1, got %d", c.MaxAttempts)
	}
	if c.MaxAttempts > 10 {
		return fmt.Errorf("client.max_attempts must be <= 10, got %d", c.MaxAttempts)
	}
	if c.Delta != nil && (*c.Delta < 1 || *c.Delta > 1000) {
		return fmt.Errorf("client.delta must be between 1 and 1000, got %d", *c.Delta)
	}
	if c.TimeoutMS != nil && (*c.TimeoutMS < 1 || *c.TimeoutMS > 60000) {
		return fmt.Errorf("client.timeout_ms must be between 1 and 60000, got %d", *c.TimeoutMS)
	}
	if c.MaxAttempts > 1 && !c.RetryOnTransportError {
		return errors.New("client.max_attempts > 1 requires client.retry_on_transport_error; " +
			"v1 retries only transport errors")
	}
	return nil
}

func (s *Scenario) validateFaults() error {
	if len(s.Faults) > MaxFaults {
		return fmt.Errorf("faults list exceeds %d entries", MaxFaults)
	}
	for i, f := range s.Faults {
		if f.Attempt > s.Client.MaxAttempts {
			return fmt.Errorf("faults[%d]: attempt %d exceeds client.max_attempts %d; the rule could never fire",
				i, f.Attempt, s.Client.MaxAttempts)
		}
		rule, err := f.rule()
		if err != nil {
			return fmt.Errorf("faults[%d]: %w", i, err)
		}
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("faults[%d]: %w", i, err)
		}
	}
	// NewSchedule rejects ambiguous rule sets; do it here so an invalid
	// scenario is refused before anything starts.
	rules, err := s.Rules()
	if err != nil {
		return err
	}
	if _, err := faults.NewSchedule(rules); err != nil {
		return err
	}
	return nil
}

func (f Fault) rule() (faults.Rule, error) {
	stage, err := faults.ParseStage(f.Stage)
	if err != nil {
		return faults.Rule{}, err
	}
	action, err := faults.ParseAction(f.Action)
	if err != nil {
		return faults.Rule{}, err
	}
	rule := faults.Rule{Attempt: f.Attempt, Stage: stage, Action: action}
	if f.DelayMS != nil {
		if *f.DelayMS < 0 {
			return faults.Rule{}, fmt.Errorf("delay_ms must not be negative, got %d", *f.DelayMS)
		}
		rule.Delay = time.Duration(*f.DelayMS) * time.Millisecond
	}
	if action == faults.ActionDelay && f.DelayMS == nil {
		return faults.Rule{}, fmt.Errorf("action %q requires delay_ms", faults.ActionDelay)
	}
	return rule, nil
}

// Rules converts the scenario's faults into scheduler rules. The rules are
// wildcards over case IDs: every case in a run gets the same schedule.
func (s *Scenario) Rules() ([]faults.Rule, error) {
	rules := make([]faults.Rule, 0, len(s.Faults))
	for i, f := range s.Faults {
		rule, err := f.rule()
		if err != nil {
			return nil, fmt.Errorf("faults[%d]: %w", i, err)
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// FixtureMode returns the validated fixture mode.
func (s *Scenario) FixtureMode() (fixture.Mode, error) {
	return fixture.ParseMode(s.Fixture)
}

// Delta returns the configured operation delta or the default.
func (s *Scenario) Delta() int {
	if s.Client.Delta == nil {
		return DefaultDelta
	}
	return *s.Client.Delta
}

// AttemptTimeout returns the configured per-attempt timeout, or zero when the
// scenario leaves it to the caller's deadline.
func (s *Scenario) AttemptTimeout() time.Duration {
	if s.Client.TimeoutMS == nil {
		return 0
	}
	return time.Duration(*s.Client.TimeoutMS) * time.Millisecond
}

// VerifyInvariant returns the validated invariant.
func (s *Scenario) VerifyInvariant() verify.Invariant {
	expected := 0
	if s.Invariant.Expected != nil {
		expected = *s.Invariant.Expected
	}
	return verify.Invariant{
		Type:     verify.InvariantType(s.Invariant.Type),
		Expected: expected,
	}
}
