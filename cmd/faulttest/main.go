// Command faulttest runs HTTP fault-injection scenarios against a local
// fixture and reports a verdict derived from the fixture's observed state.
//
// Exit codes are part of the contract (docs/PRD.md section 2):
//
//	0  every case invariant passed
//	1  at least one case invariant failed (the application under test is wrong)
//	2  invalid input or configuration
//	3  test infrastructure failure, or an outcome that could not be determined
//
// Separating 1 from 3 is the point: a harness that could not reach the oracle
// has not discovered an application bug.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/report"
	"github.com/kyle-luong/http-fault-injection-tester/internal/runner"
	"github.com/kyle-luong/http-fault-injection-tester/internal/scenario"
	"github.com/kyle-luong/http-fault-injection-tester/internal/trace"
	"github.com/kyle-luong/http-fault-injection-tester/internal/verify"
)

// version is the tool's own version, bumped by hand at milestones.
const version = "0.4.0-M4"

// Exit codes.
const (
	exitPass      = 0
	exitInvariant = 1
	exitUsage     = 2
	exitHarness   = 3
)

// defaultCaseID is only the value shown in the flag's help text. Unless
// --case-id is given explicitly, IDs come from the run's case indexes.
const defaultCaseID = "case-0001"

// maxReportedCases bounds per-case detail so a 10,000-case run cannot bury the
// summary, or the terminal, in output.
const maxReportedCases = 10

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}

	switch args[0] {
	case "run":
		return runScenario(args[1:], stdout, stderr)
	case "replay":
		return replayTrace(args[1:], stdout, stderr)
	case "report":
		return reportSaved(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "faulttest %s\n", version)
		return exitPass
	case "-h", "--help", "help":
		usage(stdout)
		return exitPass
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func runScenario(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scenarioPath := fs.String("scenario", "", "path to a v1 scenario JSON file (required)")
	timeout := fs.Duration("timeout", runner.DefaultCaseTimeout,
		fmt.Sprintf("per-case deadline (max %s)", runner.MaxCaseTimeout))
	caseID := fs.String("case-id", defaultCaseID,
		"run exactly one case under this identifier instead of generated ones")
	runs := fs.Int("runs", runner.DefaultRuns,
		fmt.Sprintf("independent cases to execute, each with isolated state (max %d)", runner.MaxRuns))
	parallel := fs.Int("parallel", runner.DefaultParallel,
		fmt.Sprintf("greatest number of cases in flight at once (max %d)", runner.MaxParallel))
	outPath := fs.String("out", "", "write the structured result JSON to this path")
	tracesDir := fs.String("traces", "", "write one replayable per-case trace file into this directory")
	overwrite := fs.Bool("overwrite", false, "allow --out and --traces to replace existing files")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected positional argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if *scenarioPath == "" {
		fmt.Fprintln(stderr, "--scenario is required")
		return exitUsage
	}
	if *timeout <= 0 || *timeout > runner.MaxCaseTimeout {
		fmt.Fprintf(stderr, "--timeout must be between 1ns and %s\n", runner.MaxCaseTimeout)
		return exitUsage
	}
	if *runs < 1 || *runs > runner.MaxRuns {
		fmt.Fprintf(stderr, "--runs must be between 1 and %d\n", runner.MaxRuns)
		return exitUsage
	}
	if *parallel < 1 || *parallel > runner.MaxParallel {
		fmt.Fprintf(stderr, "--parallel must be between 1 and %d\n", runner.MaxParallel)
		return exitUsage
	}

	// --case-id names one case, so nothing describing a run of many applies
	// to it. Silently ignoring those flags would misreport what was run.
	caseIDGiven, parallelGiven := false, false
	var emptyPath string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "case-id":
			caseIDGiven = true
		case "parallel":
			parallelGiven = true
		case "out", "traces":
			// A flag given as --out="$DIR" with DIR unset would otherwise be
			// indistinguishable from not passing it, and a CI job would be
			// told it succeeded while nothing was written.
			if f.Value.String() == "" {
				emptyPath = f.Name
			}
		}
	})
	if emptyPath != "" {
		fmt.Fprintf(stderr, "--%s was given an empty path\n", emptyPath)
		return exitUsage
	}
	if caseIDGiven && *runs > 1 {
		fmt.Fprintln(stderr, "--case-id names a single case and cannot be combined with --runs greater than 1")
		return exitUsage
	}
	if caseIDGiven && parallelGiven {
		fmt.Fprintln(stderr, "--case-id runs a single case, so --parallel does not apply")
		return exitUsage
	}
	if *overwrite && *outPath == "" && *tracesDir == "" {
		fmt.Fprintln(stderr, "--overwrite applies to --out and --traces, neither of which was given")
		return exitUsage
	}

	// A run owns its trace directory whole, so the result file cannot live
	// there: it would either land on a trace -- destroying the one the run's
	// own Reproduce line points at -- or make the next run refuse the
	// directory it just wrote.
	if *outPath != "" && *tracesDir != "" &&
		filepath.Clean(filepath.Dir(*outPath)) == filepath.Clean(*tracesDir) {
		fmt.Fprintf(stderr, "--out %s is inside the trace directory %s, which a run writes whole\n",
			*outPath, *tracesDir)
		return exitUsage
	}

	// The scenario is loaded before any output path is prepared, so a typo in
	// --scenario does not leave empty directories behind.
	sc, err := scenario.Load(*scenarioPath)
	if err != nil {
		fmt.Fprintf(stderr, "invalid scenario: %v\n", err)
		return exitUsage
	}

	// Checked before the run, not after it: discovering an unusable path once
	// 500 cases have executed would throw away the whole run. This probes
	// writability as well as existence, because a refused directory fails
	// just as late as a taken filename.
	if *outPath != "" {
		if err := report.CheckWritable(*outPath, *overwrite); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return exitUsage
		}
	}
	foreignTraces := 0
	if *tracesDir != "" {
		// Only an operator-supplied identifier can be unusable as a file name;
		// generated ones never are.
		var named []string
		if caseIDGiven {
			named = []string{*caseID}
		}
		foreignTraces, err = trace.CheckWritable(*tracesDir, *overwrite, named)
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return exitUsage
		}
	}

	env, err := runner.Setup(sc, runner.Options{CaseTimeout: *timeout})
	if err != nil {
		fmt.Fprintf(stderr, "could not start test infrastructure: %v\n", err)
		return exitHarness
	}
	defer func() {
		if err := env.Close(); err != nil {
			fmt.Fprintf(stderr, "warning: shutdown reported %v\n", err)
		}
	}()

	// One handler for the whole process, and successive interrupts mean
	// successive things. Installing a second handler later would leave a gap
	// in which a signal is fatal, and would absorb an interrupt that arrived
	// while the run was winding down.
	signals := make(chan os.Signal, 3)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	ctx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	outCtx, stopOutput := context.WithCancel(context.Background())
	defer stopOutput()

	dispatcherDone := make(chan struct{})
	defer close(dispatcherDone)
	go interruptPhases(signals, dispatcherDone, stopRun, stopOutput, func() {
		// The operator is insisting. Put the default disposition back and
		// re-raise, so the process dies the way the shell expects rather than
		// absorbing signals forever.
		signal.Stop(signals)
		if self, err := os.FindProcess(os.Getpid()); err == nil {
			_ = self.Signal(os.Interrupt)
		}
	})

	runID, startedAt := report.NewRunID(), time.Now().UTC()

	var summary runner.Summary
	if caseIDGiven {
		summary = env.RunOne(ctx, *caseID)
	} else {
		summary, err = env.Run(ctx, runner.RunSpec{Runs: *runs, Parallel: *parallel})
		if err != nil {
			fmt.Fprintf(stderr, "could not start the run: %v\n", err)
			return exitUsage
		}
	}
	printSummary(stdout, summary)

	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(ctxErr, context.Canceled) {
		// Reported, but it does not decide the exit code: an interrupted run
		// that already found a violation still found it.
		fmt.Fprintln(stderr, "run interrupted")
	}

	code := exitCodeFor(summary)

	// Writing a trace per case is thousands of files for a large run, so the
	// output phase answers to the second interrupt.
	if *tracesDir != "" {
		if err := trace.WriteRun(outCtx, *tracesDir, *overwrite, runID, startedAt,
			env.CaseTimeout(), sc, summary.Outcomes); err != nil {
			fmt.Fprintf(stderr, "could not write traces: %v\n", err)
			// Traces that were asked for and never arrived leave the run
			// unreproducible, so they do not keep a clean exit code.
			if code == exitPass {
				code = exitHarness
			}
		} else {
			fmt.Fprintf(stdout, "Traces: %s\n", *tracesDir)
			if foreignTraces > 0 {
				// Only the planned case IDs were replaced, so a longer earlier
				// run leaves traces behind that describe something else.
				fmt.Fprintf(stderr, "warning: %s already held %d trace file(s) before this run; "+
					"those not replaced describe a different run\n", *tracesDir, foreignTraces)
			}
			if o, ok := firstViolation(summary); ok {
				fmt.Fprintf(stdout, "Reproduce: faulttest replay --trace %s\n", trace.Path(*tracesDir, o.CaseID))
			}
		}
	}
	if *outPath != "" {
		if err := report.Write(*outPath, *overwrite, report.New(runID, startedAt, summary)); err != nil {
			fmt.Fprintf(stderr, "could not write results: %v\n", err)
			// A result file that was asked for and never arrived must not
			// leave behind the exit code of a clean run.
			if code == exitPass {
				code = exitHarness
			}
		} else {
			fmt.Fprintf(stdout, "Results: %s\n", *outPath)
		}
	}
	return code
}

// firstViolation picks the case worth reproducing: the first the oracle judged
// to be a violation.
//
// Deliberately not "the first that did not pass". An inconclusive case has no
// outcome to reproduce -- after an interrupt every unfinished case is
// inconclusive, and nominating one sends the operator to a replay that reports
// a mismatch by construction, which reads as the tool failing to reproduce
// itself.
func firstViolation(s runner.Summary) (runner.Outcome, bool) {
	for _, o := range s.Outcomes {
		if o.Verdict() == verify.VerdictFail {
			return o, true
		}
	}
	return runner.Outcome{}, false
}

// interruptPhases turns successive interrupts into the three things an operator
// means by them: stop starting work, stop writing output, give up on the
// process. It is kept apart from the signal plumbing so it can be tested
// without sending real signals.
func interruptPhases(signals <-chan os.Signal, done <-chan struct{}, stopRun, stopOutput, giveUp func()) {
	for count := 0; ; {
		select {
		case <-done:
			return
		case _, ok := <-signals:
			if !ok {
				return
			}
		}
		count++
		switch count {
		case 1:
			stopRun()
		case 2:
			stopOutput()
		default:
			giveUp()
			return
		}
	}
}

// replayTrace re-runs one recorded case from its trace alone and reports
// whether the record reproduced.
//
// A mismatch is this tool failing to reproduce itself, not the application
// violating an invariant, so it exits 3 and never 1. Exit 1 continues to mean
// only that the oracle found a violation in the replayed run.
func replayTrace(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tracePath := fs.String("trace", "", "path to a trace file written by run --traces (required)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected positional argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if *tracePath == "" {
		fmt.Fprintln(stderr, "--trace is required")
		return exitUsage
	}
	tr, err := trace.Load(*tracePath)
	if err != nil {
		// The PRD's exit-code table puts a corrupted trace with the
		// infrastructure failures, not with invalid arguments.
		fmt.Fprintf(stderr, "unusable trace: %v\n", err)
		return exitHarness
	}

	// The recorded decisions are the schedule, not the scenario's rules:
	// re-matching the rules would make the comparison compare a thing with
	// itself.
	// Both the deadline and the schedule come from the record. There is no
	// --timeout here on purpose: a case replayed under a different deadline is
	// a different case, and the mismatch would be reported as this tool failing
	// to reproduce itself.
	env, err := runner.Setup(tr.Scenario, runner.Options{
		CaseTimeout: tr.CaseTimeout,
		Rules:       tr.Rules(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "could not start test infrastructure: %v\n", err)
		return exitHarness
	}
	defer func() {
		if err := env.Close(); err != nil {
			fmt.Fprintf(stderr, "warning: shutdown reported %v\n", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(stdout, "Replaying %s from run %s recorded %s, deadline %s\n",
		tr.CaseID, tr.RunID, tr.RecordedAt.Format(time.RFC3339), tr.CaseTimeout)
	summary := env.RunOne(ctx, tr.CaseID)
	printSummary(stdout, summary)

	// An inconclusive case is still compared: a trace that recorded one
	// reproduces when it comes out inconclusive again. A harness error is the
	// replay itself failing, and comparing against it would report a
	// reproducibility problem that is really a broken fixture.
	outcome := summary.Outcomes[0]
	if outcome.Verdict() == verify.VerdictHarnessError {
		fmt.Fprintln(stderr, "replay could not run, so nothing was compared")
		return exitHarness
	}

	if diffs := trace.Compare(tr, outcome); len(diffs) > 0 {
		fmt.Fprintln(stdout, "Replay did not reproduce the recorded case:")
		for _, d := range diffs {
			fmt.Fprintf(stdout, "  %s\n", d)
		}
		return exitHarness
	}
	fmt.Fprintf(stdout, "Replay reproduced the recorded decisions, state and verdict (%s)\n", outcome.Verdict())
	return exitCodeFor(summary)
}

// reportSaved summarises a saved result without executing anything, and
// returns the same exit code the original run did, so a stored result stays
// usable in a pipeline.
func reportSaved(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: faulttest report PATH")
		return exitUsage
	}

	result, err := report.Load(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "invalid result: %v\n", err)
		return exitUsage
	}

	fmt.Fprintf(stdout, "Run %s started %s\n", result.RunID, result.StartedAt.Format(time.RFC3339))
	printSummary(stdout, result.Summary)
	return exitCodeFor(result.Summary)
}

// exitCodeFor maps a finished run onto the exit-code contract. A failed
// invariant is a definite finding and outranks cases the tool could not
// establish, so an interrupted or partly indeterminate run still reports the
// violation it did find.
func exitCodeFor(s runner.Summary) int {
	switch {
	case s.Failed > 0:
		return exitInvariant
	case s.Inconclusive > 0 || s.HarnessErrors > 0 || s.NotRun > 0:
		return exitHarness
	default:
		return exitPass
	}
}

func printSummary(w io.Writer, s runner.Summary) {
	fmt.Fprintf(w, "Scenario: %s | Fixture: %s\n", s.Scenario, s.Fixture)
	fmt.Fprintf(w, "Cases: %d  Passed: %d  Failed: %d  Inconclusive: %d  Infrastructure: %d\n",
		s.Executed, s.Passed, s.Failed, s.Inconclusive, s.HarnessErrors)

	if s.Runs > 1 {
		fmt.Fprintf(w, "Parallelism: %d  Duration: %s  Throughput: %.1f cases/sec\n",
			s.Parallel, s.Duration.Round(time.Millisecond), s.CasesPerSecond())
	}
	if s.NotRun > 0 {
		fmt.Fprintf(w, "Not run: %d (the run ended before they started)\n", s.NotRun)
	}
	if s.DroppedDecisions > 0 {
		fmt.Fprintf(w, "Warning: %d fault decision(s) passed the log bound; "+
			"the recorded fault history is incomplete\n", s.DroppedDecisions)
	}

	// One case is reported in full. A larger run reports only the cases that
	// need explaining, because a wall of passes hides the few that do not.
	detail := s.Outcomes
	if s.Runs > 1 {
		detail = nil
		for _, o := range s.Outcomes {
			if o.Verdict() != verify.VerdictPass {
				detail = append(detail, o)
			}
		}
	}
	omitted := 0
	if len(detail) > maxReportedCases {
		omitted = len(detail) - maxReportedCases
		detail = detail[:maxReportedCases]
	}
	for _, o := range detail {
		printCase(w, o)
	}
	if omitted > 0 {
		fmt.Fprintf(w, "... and %d further case(s) not shown\n", omitted)
	}
}

func printCase(w io.Writer, o runner.Outcome) {
	fmt.Fprintf(w, "Case %s: verdict=%s attempts=%d duration=%s\n",
		o.CaseID, o.Verdict(), len(o.Attempts), o.Duration.Round(time.Millisecond))

	for _, a := range o.Attempts {
		switch {
		case a.TransportError != "":
			fmt.Fprintf(w, "  attempt %d: no response (%s)\n", a.Number, a.TransportError)
		default:
			fmt.Fprintf(w, "  attempt %d: status %d\n", a.Number, a.StatusCode)
		}
	}
	for _, d := range o.Decisions {
		fmt.Fprintf(w, "  injected: attempt=%d stage=%s action=%s\n", d.Attempt, d.Stage, d.Action)
	}

	label := "Result"
	if o.Verdict() == verify.VerdictFail {
		label = "Violation"
	}
	fmt.Fprintf(w, "%s: %s\n", label, o.Verification.Detail)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `faulttest -- HTTP fault injection and state-oracle testing

Usage:
  faulttest run --scenario PATH [--runs N] [--parallel N] [--timeout DURATION]
                                [--out PATH] [--traces DIR] [--overwrite]
  faulttest run --scenario PATH --case-id ID [--timeout DURATION]
                                [--out PATH] [--traces DIR] [--overwrite]
  faulttest replay --trace PATH
  faulttest report PATH
  faulttest version

The scenario's fixture is started on a loopback address, traffic is forwarded
through a fault-injecting proxy, and the verdict is read from the fixture's
observed state rather than from the client's HTTP status.

Each case has its own identifier, its own isolated fixture state and its own
deadline. --parallel bounds how many run at once; it is a limit on real
concurrency, not a target. Interrupting the tool stops new cases from starting
and lets the cases in flight finish as inconclusive.

Exit codes: 0 pass, 1 invariant failed, 2 invalid input, 3 infrastructure
failure or indeterminate outcome. A failed invariant outranks an indeterminate
case, so a real finding is never hidden by one.

--out writes the run's structured result JSON, which records the verdict, the
attempts, the injected fault decisions and the observed state for every case.
It holds no request bodies and no request headers; the only request-side value
recorded is the harness's own idempotency key, which identifies the logical
operation and carries no caller data. An existing path is refused unless
--overwrite is given. "report PATH" prints the same summary from a saved
result without executing anything.

--traces writes one trace file per case into DIR, each self-contained enough
for "replay --trace DIR/case-0001.json" to reconstruct the fixture, inject the
fault decisions that case recorded, and check the replay against the record.
A trace holds the scenario's own configuration, the deadline the recorded run
used, and, for the request, only its method, path and a digest of the payload.
It records no request body and no request header; the request-side values it
does carry are the harness's own operation identifiers, read back from the
fixture's state, which carry no caller data. Digests over the embedded scenario
and the request identity mean a partly edited trace is refused rather than
replayed. Replay reproduces the injected decisions and the oracle's verdict;
it does not reproduce goroutine scheduling, port allocation or timing, so a
scenario whose verdict depends on an injected delay racing a client timeout can
legitimately replay differently. A replay that does not reproduce its record
exits 3: that is this tool failing to reproduce itself, not a finding about the
application under test.
`)
}
