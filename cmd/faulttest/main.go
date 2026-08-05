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
// has not discovered an application bug. Only version and help are wired up at
// this stage; see docs/MILESTONES.md.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is the tool's own version, bumped by hand at milestones.
const version = "0.1.0-M0"

// Exit codes.
const (
	exitPass    = 0
	exitUsage   = 2
	exitHarness = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}

	switch args[0] {
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

func usage(w io.Writer) {
	fmt.Fprint(w, `faulttest -- HTTP fault injection and state-oracle testing

Usage:
  faulttest version

Exit codes: 0 pass, 1 invariant failed, 2 invalid input, 3 infrastructure
failure or indeterminate outcome.

No scenario execution is implemented yet. See docs/MILESTONES.md.
`)
}
