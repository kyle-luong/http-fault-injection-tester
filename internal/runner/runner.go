// Package runner wires a validated scenario into running infrastructure and
// executes cases against it.
//
// One Env holds everything shared by a run (fixture, proxy, schedule, HTTP
// client), all of it safe for concurrent use. Everything case-specific is
// derived from the case ID, which is derived from the case index, so a run's
// cases are independent by construction rather than by convention.
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
	"sync"
	"sync/atomic"
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
		return outcome
	}

	caseCtx, cancel := context.WithTimeout(ctx, e.caseTimeout)
	defer cancel()

	if err := fixture.Reset(caseCtx, e.http, e.fixture.BaseURL(), caseID); err != nil {
		// A cancelled run and a blown deadline leave the case unstarted rather
		// than proving the harness is broken.
		if caseCtx.Err() != nil {
			return finish(verify.Inconclusive(e.invariant, fixture.State{CaseID: caseID},
				"case did not start before its deadline or the run was cancelled: "+err.Error()))
		}
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

	// The attempt sequence can return without an error even when the case was
	// cut short, because a cancelled attempt is recorded as a transport error
	// rather than raised. A case whose context ended has no verdict, however
	// conclusive the state it happened to reach looks: reporting one would
	// turn an abandoned case into a finding about the application.
	if caseCtx.Err() != nil {
		return finish(verify.Inconclusive(e.invariant, fixture.State{CaseID: caseID},
			"case ended before its attempt sequence completed: "+caseCtx.Err().Error()))
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

// Run-size bounds come from the PRD's CLI contract.
const (
	DefaultRuns     = 1
	MaxRuns         = 10000
	DefaultParallel = 1
	MaxParallel     = 64
)

// RunSpec describes one whole run.
type RunSpec struct {
	// Runs is the number of independent cases. Zero selects DefaultRuns.
	Runs int
	// Parallel is the greatest number of cases in flight at once. Zero
	// selects DefaultParallel. It bounds goroutines, sockets and fixture load
	// together, which is why it is a hard limit rather than a hint.
	Parallel int
}

func (s *RunSpec) normalize() error {
	if s.Runs == 0 {
		s.Runs = DefaultRuns
	}
	if s.Parallel == 0 {
		s.Parallel = DefaultParallel
	}
	if s.Runs < 1 || s.Runs > MaxRuns {
		return fmt.Errorf("runs must be between 1 and %d, got %d", MaxRuns, s.Runs)
	}
	if s.Parallel < 1 || s.Parallel > MaxParallel {
		return fmt.Errorf("parallel must be between 1 and %d, got %d", MaxParallel, s.Parallel)
	}
	return nil
}

// CaseID names the nth case of a run, counting from one. An ID depends on the
// index alone, never on scheduling order, so the same run always covers the
// same set of case IDs at any parallelism.
func CaseID(n int) string { return fmt.Sprintf("case-%04d", n) }

// Summary aggregates a whole run. The counts are classifications rather than
// opinions: Failed means the application violated an invariant, while
// Inconclusive and HarnessErrors cover what the tool could not establish.
type Summary struct {
	Scenario string `json:"scenario"`
	Fixture  string `json:"fixture"`
	Runs     int    `json:"runs"`
	Parallel int    `json:"parallel"`

	Executed      int `json:"executed"`
	NotRun        int `json:"not_run"`
	Passed        int `json:"passed"`
	Failed        int `json:"failed"`
	Inconclusive  int `json:"inconclusive"`
	HarnessErrors int `json:"harness_errors"`

	Canceled bool          `json:"canceled"`
	Duration time.Duration `json:"duration_ns"`

	// MaxConcurrentCases is the greatest number of cases observed in flight at
	// once. It can never exceed Parallel, and reporting it turns the
	// concurrency bound into something a test and an operator can check rather
	// than infer from timing.
	MaxConcurrentCases int `json:"max_concurrent_cases"`

	ProxyStats proxy.Stats `json:"proxy_stats"`
	// DroppedDecisions is non-zero when the run injected more faults than the
	// decision log holds. The verdicts stand, but the recorded fault history
	// is incomplete, which matters to anything that reports or replays it.
	DroppedDecisions int `json:"dropped_decisions"`

	// Outcomes is ordered by case index, never by completion order, so the
	// report does not change shape when parallelism does.
	Outcomes []Outcome `json:"outcomes"`
}

// CasesPerSecond is the measured throughput of the cases that executed.
func (s Summary) CasesPerSecond() float64 {
	if s.Duration <= 0 || s.Executed == 0 {
		return 0
	}
	return float64(s.Executed) / s.Duration.Seconds()
}

// Run executes spec.Runs independent cases, at most spec.Parallel at a time,
// and returns the aggregate. It returns an error only when the run could not
// start; everything a case observed is reported in the Summary.
//
// Cancelling ctx stops further cases from starting. Cases already in flight
// finish as inconclusive rather than as findings, because a case that was cut
// short proves nothing about the application.
func (e *Env) Run(ctx context.Context, spec RunSpec) (Summary, error) {
	if err := spec.normalize(); err != nil {
		return Summary{}, fmt.Errorf("runner: %w", err)
	}

	started := time.Now()
	outcomes := make([]Outcome, spec.Runs)
	ran := make([]bool, spec.Runs)

	// A fixed pool of workers drawing from one channel bounds the goroutines
	// for the whole run. Starting a goroutine per case would make --runs the
	// real concurrency limit and --parallel a decoration.
	workers := spec.Parallel
	if workers > spec.Runs {
		workers = spec.Runs
	}

	indexes := make(chan int)
	var inflight, peak atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for idx := range indexes {
				current := inflight.Add(1)
				for {
					seen := peak.Load()
					if current <= seen || peak.CompareAndSwap(seen, current) {
						break
					}
				}

				// Each index is handed to exactly one worker, so these two
				// writes need no lock and cannot interleave.
				outcomes[idx] = e.RunCase(ctx, CaseID(idx+1))
				ran[idx] = true

				inflight.Add(-1)
			}
		}()
	}

dispatch:
	for i := range spec.Runs {
		select {
		case indexes <- i:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(indexes)
	wg.Wait()

	summary := Summary{
		Scenario: e.scenario.Name,
		Fixture:  e.scenario.Fixture,
		Runs:     spec.Runs,
		Parallel: spec.Parallel,
		Canceled: ctx.Err() != nil,
		Outcomes: make([]Outcome, 0, spec.Runs),
	}
	for i := range spec.Runs {
		if !ran[i] {
			summary.NotRun++
			continue
		}
		summary.Executed++
		summary.Outcomes = append(summary.Outcomes, outcomes[i])
		summary.count(outcomes[i].Verdict())
	}
	summary.Duration = time.Since(started)
	summary.MaxConcurrentCases = int(peak.Load())
	summary.ProxyStats = e.proxy.Stats()
	summary.DroppedDecisions = e.schedule.DroppedDecisions()
	return summary, nil
}

// count classifies one outcome into the summary's totals.
func (s *Summary) count(v verify.Verdict) {
	switch v {
	case verify.VerdictPass:
		s.Passed++
	case verify.VerdictFail:
		s.Failed++
	case verify.VerdictInconclusive:
		s.Inconclusive++
	default:
		s.HarnessErrors++
	}
}

// RunOne executes a single case under an explicit ID and reports it in the
// same shape a larger run uses, so one code path formats every result.
func (e *Env) RunOne(ctx context.Context, caseID string) Summary {
	started := time.Now()
	outcome := e.RunCase(ctx, caseID)

	summary := Summary{
		Scenario: e.scenario.Name,
		Fixture:  e.scenario.Fixture,
		Runs:     1,
		Parallel: 1,
		Executed: 1,
		Canceled: ctx.Err() != nil,
		Outcomes: []Outcome{outcome},

		MaxConcurrentCases: 1,
	}
	summary.count(outcome.Verdict())
	summary.Duration = time.Since(started)
	summary.ProxyStats = e.proxy.Stats()
	summary.DroppedDecisions = e.schedule.DroppedDecisions()
	return summary
}
