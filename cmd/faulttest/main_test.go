package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kyle-luong/http-fault-injection-tester/internal/runner"
)

func scenarioPath(name string) string {
	return filepath.Join("..", "..", "scenarios", name)
}

// TestRunExitCodes covers GO-AC10 and GO-T14 at the CLI boundary: an
// application invariant failure and a configuration error must not share an
// exit code, because that distinction is what a CI job acts on.
func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{
			name:     "safe fixture passes",
			args:     []string{"run", "--scenario", scenarioPath("lost-response-safe.json")},
			wantCode: exitPass,
			wantOut:  "verdict=pass",
		},
		{
			name:     "unsafe fixture fails its invariant",
			args:     []string{"run", "--scenario", scenarioPath("lost-response.json")},
			wantCode: exitInvariant,
			wantOut:  "Violation: expected counter=1; observed counter=2",
		},
		{
			name:     "no fault baseline passes",
			args:     []string{"run", "--scenario", scenarioPath("no-fault-baseline.json")},
			wantCode: exitPass,
			wantOut:  "verdict=pass",
		},
		{
			name:     "version",
			args:     []string{"version"},
			wantCode: exitPass,
			wantOut:  "faulttest " + version,
		},
		{
			name:     "help",
			args:     []string{"--help"},
			wantCode: exitPass,
			wantOut:  "Usage:",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s",
					code, tc.wantCode, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.wantOut) {
				t.Errorf("stdout did not contain %q:\n%s", tc.wantOut, stdout.String())
			}
		})
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")

	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"schema_version": 1, "nmae": "typo"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"unknown subcommand", []string{"replay"}},
		{"missing scenario flag", []string{"run"}},
		{"nonexistent scenario file", []string{"run", "--scenario", missing}},
		{"invalid scenario file", []string{"run", "--scenario", bad}},
		{"unknown flag", []string{"run", "--scenario", scenarioPath("lost-response.json"), "--nope"}},
		{"runs below the bound", []string{"run", "--scenario", scenarioPath("lost-response.json"), "--runs", "0"}},
		{"runs past the bound", []string{"run", "--scenario", scenarioPath("lost-response.json"), "--runs", "10001"}},
		{"parallelism below the bound", []string{"run", "--scenario", scenarioPath("lost-response.json"), "--parallel", "0"}},
		{"parallelism past the bound", []string{"run", "--scenario", scenarioPath("lost-response.json"), "--parallel", "65"}},
		{"case id with many runs", []string{"run", "--scenario", scenarioPath("lost-response.json"), "--runs", "2", "--case-id", "c"}},
		{"stray positional argument", []string{"run", "--scenario", scenarioPath("lost-response.json"), "extra"}},
		{"zero timeout", []string{"run", "--scenario", scenarioPath("lost-response.json"), "--timeout", "0s"}},
		{"timeout past the bound", []string{"run", "--scenario", scenarioPath("lost-response.json"), "--timeout", "10m"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != exitUsage {
				t.Errorf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s",
					code, exitUsage, stdout.String(), stderr.String())
			}
		})
	}
}

// TestReportMentionsInjectedFaults keeps the operator-facing output honest: a
// verdict without the fault that produced it is not reproducible.
func TestReportMentionsInjectedFaults(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{"run", "--scenario", scenarioPath("lost-response.json")}, &stdout, &stderr)

	out := stdout.String()
	for _, want := range []string{
		"Scenario: lost-response",
		"Fixture: unsafe-counter",
		"injected: attempt=1 stage=after_upstream_response action=close_client_connection",
		"attempt 1: no response",
		"attempt 2: status 200",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output did not contain %q:\n%s", want, out)
		}
	}
}

// TestOutputCarriesNoRequestPayloads covers the redaction requirement: traces
// and logs must not echo request bodies or credential-bearing headers.
func TestOutputCarriesNoRequestPayloads(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{"run", "--scenario", scenarioPath("lost-response.json")}, &stdout, &stderr)

	combined := stdout.String() + stderr.String()
	for _, forbidden := range []string{"delta", "Idempotency-Key", "authorization", "Authorization"} {
		if strings.Contains(combined, forbidden) {
			t.Errorf("output leaked %q:\n%s", forbidden, combined)
		}
	}
}

func TestCaseIDFlagIsUsed(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"run", "--scenario", scenarioPath("no-fault-baseline.json"), "--case-id", "case-custom",
	}, &stdout, &stderr)
	if code != exitPass {
		t.Fatalf("exit code = %d, want %d; stderr:\n%s", code, exitPass, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Case case-custom:") {
		t.Errorf("output did not use the supplied case ID:\n%s", stdout.String())
	}
}

// TestConcurrentRunReportsAnAggregate covers the reporting half of GO-04: a
// run of many cases reports totals and measured throughput rather than a wall
// of per-case output.
func TestConcurrentRunReportsAnAggregate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"run", "--scenario", scenarioPath("lost-response-safe.json"),
		"--runs", "20", "--parallel", "8",
	}, &stdout, &stderr)

	if code != exitPass {
		t.Fatalf("exit code = %d, want %d; stderr:\n%s", code, exitPass, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Cases: 20  Passed: 20  Failed: 0",
		"Parallelism: 8",
		"cases/sec",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output did not contain %q:\n%s", want, out)
		}
	}
	// Twenty passing cases must not each print their own block.
	if n := strings.Count(out, "Case case-"); n != 0 {
		t.Errorf("printed %d per-case blocks for a run with nothing to explain:\n%s", n, out)
	}
}

// TestConcurrentRunBoundsPerCaseDetail keeps a large failing run readable
// while still naming the failures and returning the finding exit code.
func TestConcurrentRunBoundsPerCaseDetail(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"run", "--scenario", scenarioPath("lost-response.json"),
		"--runs", "25", "--parallel", "8",
	}, &stdout, &stderr)

	if code != exitInvariant {
		t.Fatalf("exit code = %d, want %d; stderr:\n%s", code, exitInvariant, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Cases: 25  Passed: 0  Failed: 25") {
		t.Errorf("output did not report 25 failures:\n%s", out)
	}
	if n := strings.Count(out, "Case case-"); n != maxReportedCases {
		t.Errorf("printed %d per-case blocks, want the cap of %d", n, maxReportedCases)
	}
	if !strings.Contains(out, "further case(s) not shown") {
		t.Errorf("output did not say that cases were omitted:\n%s", out)
	}
}

// TestExitCodePrecedence pins the contract that a real finding is never
// hidden by cases the tool could not establish, including when a run is cut
// short before every case has started.
func TestExitCodePrecedence(t *testing.T) {
	tests := []struct {
		name    string
		summary runner.Summary
		want    int
	}{
		{"all passed", runner.Summary{Passed: 10}, exitPass},
		{"one failed", runner.Summary{Passed: 9, Failed: 1}, exitInvariant},
		{"failure outranks inconclusive", runner.Summary{Failed: 1, Inconclusive: 5}, exitInvariant},
		{"failure outranks an interrupted run", runner.Summary{Failed: 1, NotRun: 400, Canceled: true}, exitInvariant},
		{"inconclusive only", runner.Summary{Passed: 3, Inconclusive: 1}, exitHarness},
		{"infrastructure only", runner.Summary{Passed: 3, HarnessErrors: 1}, exitHarness},
		{"cut short with nothing found", runner.Summary{Passed: 3, NotRun: 97, Canceled: true}, exitHarness},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeFor(tc.summary); got != tc.want {
				t.Errorf("exitCodeFor(%+v) = %d, want %d", tc.summary, got, tc.want)
			}
		})
	}
}
