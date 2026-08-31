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
const version = "0.1.0-M1"

// Exit codes.
const (
	exitPass      = 0
	exitInvariant = 1
	exitUsage     = 2
	exitHarness   = 3
)

// defaultCaseID is used until --runs generates per-case IDs in GO-04.
const defaultCaseID = "case-0001"

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
	caseID := fs.String("case-id", defaultCaseID, "case identifier for the single executed case")
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

	outcome := env.RunCase(ctx, *caseID)
	report(stdout, outcome)

	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(ctxErr, context.Canceled) {
		fmt.Fprintln(stderr, "run interrupted")
		return exitHarness
	}

	switch outcome.Verdict() {
	case verify.VerdictPass:
		return exitPass
	case verify.VerdictFail:
		return exitInvariant
	default:
		return exitHarness
	}
}

func report(w io.Writer, o runner.Outcome) {
	fmt.Fprintf(w, "Scenario: %s | Fixture: %s\n", o.Scenario, o.Fixture)

	passed, failed, other := 0, 0, 0
	switch o.Verdict() {
	case verify.VerdictPass:
		passed = 1
	case verify.VerdictFail:
		failed = 1
	default:
		other = 1
	}
	fmt.Fprintf(w, "Cases: 1  Passed: %d  Failed: %d  Inconclusive/infrastructure: %d\n",
		passed, failed, other)

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
  faulttest run --scenario PATH [--timeout DURATION] [--case-id ID]
  faulttest version

The scenario's fixture is started on a loopback address, traffic is forwarded
through a fault-injecting proxy, and the verdict is read from the fixture's
observed state rather than from the client's HTTP status.

Exit codes: 0 pass, 1 invariant failed, 2 invalid input, 3 infrastructure
failure or indeterminate outcome.

Not yet implemented: --runs and --parallel (GO-04), --out and replay (GO-06).
`)
}
