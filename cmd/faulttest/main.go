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
	"syscall"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/runner"
	"github.com/kyle-luong/http-fault-injection-tester/internal/scenario"
	"github.com/kyle-luong/http-fault-injection-tester/internal/verify"
)

// version is the tool's own version, bumped by hand at milestones.
const version = "0.2.0-M2"

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
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "case-id":
			caseIDGiven = true
		case "parallel":
			parallelGiven = true
		}
	})
	if caseIDGiven && *runs > 1 {
		fmt.Fprintln(stderr, "--case-id names a single case and cannot be combined with --runs greater than 1")
		return exitUsage
	}
	if caseIDGiven && parallelGiven {
		fmt.Fprintln(stderr, "--case-id runs a single case, so --parallel does not apply")
		return exitUsage
	}

	sc, err := scenario.Load(*scenarioPath)
	if err != nil {
		fmt.Fprintf(stderr, "invalid scenario: %v\n", err)
		return exitUsage
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	report(stdout, summary)

	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(ctxErr, context.Canceled) {
		// Reported, but it does not decide the exit code: an interrupted run
		// that already found a violation still found it.
		fmt.Fprintln(stderr, "run interrupted")
	}

	return exitCodeFor(summary)
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

func report(w io.Writer, s runner.Summary) {
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
		reportCase(w, o)
	}
	if omitted > 0 {
		fmt.Fprintf(w, "... and %d further case(s) not shown\n", omitted)
	}
}

func reportCase(w io.Writer, o runner.Outcome) {
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
  faulttest run --scenario PATH --case-id ID [--timeout DURATION]
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

Not yet implemented: --out structured results (GO-05), replay (GO-06).
`)
}
