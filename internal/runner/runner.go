// Package runner wires a validated scenario into running infrastructure and
// executes cases against it.
//
// This milestone executes exactly one case per run; the bounded concurrent
// worker pool arrives with GO-04. The structure anticipates it: all shared
// state (fixture, proxy, schedule, HTTP client) lives in Env and is safe for
// concurrent use, while everything case-specific is derived from the case ID.
//
// Control traffic -- reset and state reads -- goes straight to the fixture and
// never through the proxy. Injected faults must only affect the operation under
// test; an oracle that could itself be faulted would prove nothing.
package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/client"
	"github.com/kyle-luong/http-fault-injection-tester/internal/faults"
	"github.com/kyle-luong/http-fault-injection-tester/internal/fixture"
	"github.com/kyle-luong/http-fault-injection-tester/internal/proxy"
	"github.com/kyle-luong/http-fault-injection-tester/internal/scenario"
	"github.com/kyle-luong/http-fault-injection-tester/internal/verify"
)

// DefaultSettleTimeout bounds how long a case waits for upstream work to finish
// before its state is read.
const DefaultSettleTimeout = 2 * time.Second

// DefaultCaseTimeout matches the PRD's default per-case deadline.
const DefaultCaseTimeout = 5 * time.Second

// MaxCaseTimeout is the PRD's explicit upper bound on a per-case deadline.
const MaxCaseTimeout = 60 * time.Second

// Options configures an Env.
type Options struct {
	// CaseTimeout bounds one case end to end. Zero selects DefaultCaseTimeout.
	CaseTimeout time.Duration
	// SettleTimeout bounds the wait for upstream quiescence. Zero selects
	// DefaultSettleTimeout.
	SettleTimeout time.Duration
}

// Outcome is everything observed for one case.
type Outcome struct {
	CaseID       string            `json:"case_id"`
	Scenario     string            `json:"scenario"`
	Fixture      string            `json:"fixture"`
	Attempts     []client.Attempt  `json:"attempts"`
	Decisions    []faults.Decision `json:"fault_decisions"`
	Verification verify.Result     `json:"verification"`
	Duration     time.Duration     `json:"duration_ns"`
	ProxyStats   proxy.Stats       `json:"proxy_stats"`
}

// Verdict is the case's classification.
func (o Outcome) Verdict() verify.Verdict { return o.Verification.Verdict }

// Env holds the infrastructure shared by every case in one run.
type Env struct {
	scenario  *scenario.Scenario
	fixture   *fixture.Server
	proxy     *proxy.Proxy
	schedule  *faults.Schedule
	http      *http.Client
	policy    client.Policy
	invariant verify.Invariant

	caseTimeout   time.Duration
	settleTimeout time.Duration
}

// Setup starts the fixture and proxy described by the scenario. The caller must
// call Close. An error here is always a harness failure, never a finding about
// the application under test.
func Setup(sc *scenario.Scenario, opts Options) (*Env, error) {
	if sc == nil {
		return nil, errors.New("runner: scenario is required")
	}
	if err := sc.Validate(); err != nil {
		return nil, fmt.Errorf("runner: invalid scenario: %w", err)
	}

	caseTimeout := opts.CaseTimeout
	if caseTimeout <= 0 {
		caseTimeout = DefaultCaseTimeout
	}
	if caseTimeout > MaxCaseTimeout {
		return nil, fmt.Errorf("runner: case timeout %s exceeds the %s bound", caseTimeout, MaxCaseTimeout)
	}
	settleTimeout := opts.SettleTimeout
	if settleTimeout <= 0 {
		settleTimeout = DefaultSettleTimeout
	}

	mode, err := sc.FixtureMode()
	if err != nil {
		return nil, err
	}
	rules, err := sc.Rules()
	if err != nil {
		return nil, err
	}
	schedule, err := faults.NewSchedule(rules)
	if err != nil {
		return nil, err
	}

	fixtureSrv, err := fixture.Start(mode)
	if err != nil {
		return nil, err
	}

	prx, err := proxy.Start(proxy.Options{
		Upstream:        fixtureSrv.BaseURL(),
		Schedule:        schedule,
		UpstreamTimeout: caseTimeout,
	})
	if err != nil {
		_ = fixtureSrv.Close()
		return nil, err
	}

	return &Env{
		scenario: sc,
		fixture:  fixtureSrv,
		proxy:    prx,
		schedule: schedule,
		http:     client.NewHTTPClient(),
		policy: client.Policy{
			Method:                sc.Client.Method,
			Path:                  sc.Client.Path,
			Delta:                 sc.Delta(),
			MaxAttempts:           sc.Client.MaxAttempts,
			RetryOnTransportError: sc.Client.RetryOnTransportError,
			ReuseIdempotencyKey:   sc.Client.ReuseIdempotencyKey,
			AttemptTimeout:        sc.AttemptTimeout(),
		},
		invariant:     sc.VerifyInvariant(),
		caseTimeout:   caseTimeout,
		settleTimeout: settleTimeout,
	}, nil
}

// Fixture exposes the running fixture so tests can assert on backend-side
// observations such as how many requests actually arrived.
func (e *Env) Fixture() *fixture.Server { return e.fixture }

// Proxy exposes the running proxy.
func (e *Env) Proxy() *proxy.Proxy { return e.proxy }

// Close releases every listener, connection, and goroutine Setup started.
func (e *Env) Close() error {
	client.CloseIdleConnections(e.http)
	proxyErr := e.proxy.Close()
	fixtureErr := e.fixture.Close()
	return errors.Join(proxyErr, fixtureErr)
}

// RunCase executes one case and classifies it. It returns an Outcome in every
// path, including harness failures, so a caller always has evidence to report.
func (e *Env) RunCase(ctx context.Context, caseID string) Outcome {
	started := time.Now()
	outcome := Outcome{
		CaseID:   caseID,
		Scenario: e.scenario.Name,
		Fixture:  e.scenario.Fixture,
	}
	finish := func(result verify.Result) Outcome {
		outcome.Verification = result
		outcome.Decisions = e.schedule.DecisionsForCase(caseID)
		outcome.Duration = time.Since(started)
		outcome.ProxyStats = e.proxy.Stats()
		return outcome
	}

	caseCtx, cancel := context.WithTimeout(ctx, e.caseTimeout)
	defer cancel()

	if err := fixture.Reset(caseCtx, e.http, e.fixture.BaseURL(), caseID); err != nil {
		return finish(verify.HarnessError(e.invariant, "could not reset fixture state: "+err.Error()))
	}

	result, err := client.Execute(caseCtx, e.http, e.proxy.BaseURL(), caseID, e.policy)
	outcome.Attempts = result.Attempts
	if err != nil {
		// A blown deadline leaves the outcome unknown rather than proving a
		// harness defect, so the two are reported differently.
		if caseCtx.Err() != nil {
			return finish(verify.Inconclusive(e.invariant, fixture.State{CaseID: caseID},
				"case deadline reached during the attempt sequence: "+err.Error()))
		}
		return finish(verify.HarnessError(e.invariant, "client could not run the attempt sequence: "+err.Error()))
	}

	// Settle before reading state: a read that races the backend is
	// inconclusive, not a bug.
	settleCtx, settleCancel := context.WithTimeout(context.WithoutCancel(ctx), e.settleTimeout)
	defer settleCancel()
	if err := e.proxy.WaitForQuiescence(settleCtx, caseID); err != nil {
		return finish(verify.Inconclusive(e.invariant, fixture.State{CaseID: caseID},
			"upstream work did not settle before the deadline: "+err.Error()))
	}

	stateCtx, stateCancel := context.WithTimeout(context.WithoutCancel(ctx), e.settleTimeout)
	defer stateCancel()
	state, err := fixture.FetchState(stateCtx, e.http, e.fixture.BaseURL(), caseID)
	if err != nil {
		return finish(verify.HarnessError(e.invariant, "could not read fixture state: "+err.Error()))
	}

	// A client that never received a response and whose operation was never
	// applied tells us nothing about the application's retry semantics; the
	// injected faults simply prevented the work. Reporting that as an
	// application failure would be a false positive.
	if !result.Succeeded() && state.Counter < e.invariant.Expected {
		return finish(verify.Inconclusive(e.invariant, state,
			fmt.Sprintf("client exhausted %d attempt(s) without a response and nothing was applied "+
				"(counter=%d, expected=%d)", len(result.Attempts), state.Counter, e.invariant.Expected)))
	}

	return finish(verify.Evaluate(e.invariant, state))
}
