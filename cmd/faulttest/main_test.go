package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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
		{"report without a path", []string{"report"}},
		{"report with two paths", []string{"report", "a", "b"}},
		{"report of a missing file", []string{"report", missing}},
		{"report of an invalid file", []string{"report", bad}},
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

// TestRunWritesResultsAndReportReadsThemBack covers GO-T14 for the saved
// result: a run can hand its verdict to a later command, and that command
// reproduces the same summary and the same exit code without executing
// anything.
func TestRunWritesResultsAndReportReadsThemBack(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nested", "run.json")

	var runOut, runErr bytes.Buffer
	code := run([]string{
		"run", "--scenario", scenarioPath("lost-response.json"),
		"--runs", "4", "--parallel", "2", "--out", out,
	}, &runOut, &runErr)
	if code != exitInvariant {
		t.Fatalf("run exit code = %d, want %d; stderr:\n%s", code, exitInvariant, runErr.String())
	}
	if !strings.Contains(runOut.String(), "Results: "+out) {
		t.Errorf("run did not report where it wrote results:\n%s", runOut.String())
	}

	var repOut, repErr bytes.Buffer
	if code := run([]string{"report", out}, &repOut, &repErr); code != exitInvariant {
		t.Fatalf("report exit code = %d, want %d; stderr:\n%s", code, exitInvariant, repErr.String())
	}

	// The saved result must carry the counts, not recompute them.
	const wantCounts = "Cases: 4  Passed: 0  Failed: 4"
	if !strings.Contains(repOut.String(), wantCounts) {
		t.Errorf("report output did not contain %q:\n%s", wantCounts, repOut.String())
	}
	if !strings.Contains(repOut.String(), "Run run-") {
		t.Errorf("report output did not identify the run:\n%s", repOut.String())
	}
	for _, want := range []string{"Scenario: lost-response", "Violation: expected counter=1"} {
		if !strings.Contains(repOut.String(), want) {
			t.Errorf("report output did not contain %q:\n%s", want, repOut.String())
		}
	}
}

// TestOutRefusesAnExistingPath checks the refusal happens before the run, so
// a mistyped path cannot cost a completed run.
func TestOutRefusesAnExistingPath(t *testing.T) {
	out := filepath.Join(t.TempDir(), "run.json")
	if err := os.WriteFile(out, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"run", "--scenario", scenarioPath("no-fault-baseline.json"), "--out", out,
	}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "--overwrite") {
		t.Errorf("stderr did not explain how to proceed:\n%s", stderr.String())
	}
	// Refused before executing: the untouched file proves nothing ran over it.
	if data, err := os.ReadFile(out); err != nil || string(data) != "{}" {
		t.Errorf("the existing file was modified: %q, %v", data, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{
		"run", "--scenario", scenarioPath("no-fault-baseline.json"), "--out", out, "--overwrite",
	}, &stdout, &stderr); code != exitPass {
		t.Fatalf("exit code with --overwrite = %d, want %d; stderr:\n%s", code, exitPass, stderr.String())
	}
}

// TestResultFileCarriesNoRequestPayloads is the end-to-end half of GO-AC09:
// whatever a real run observes, the file it leaves behind holds no request
// body and no credential-bearing header.
//
// The assertion is on key names rather than on a list of values, because a
// value list can only catch leaks someone thought of in advance: this fails
// the moment any field whose name looks like a header, a body or a credential
// enters the schema.
//
// The idempotency key is present on purpose and is not a leak. It is the
// harness's own identifier for a logical operation, which the PRD requires
// for traceability, and it carries no caller data.
func TestResultFileCarriesNoRequestPayloads(t *testing.T) {
	out := filepath.Join(t.TempDir(), "run.json")

	var stdout, stderr bytes.Buffer
	run([]string{
		"run", "--scenario", scenarioPath("lost-response.json"), "--runs", "2", "--out", out,
	}, &stdout, &stderr)

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}

	risky := regexp.MustCompile(`(?i)"[a-z_]*(header|body|cookie|auth|token|secret|password|credential)[a-z_]*"\s*:`)
	if found := risky.FindAllString(string(data), -1); found != nil {
		t.Errorf("result file contains field(s) that suggest request data: %v", found)
	}
	// The fixture's request body field, which must never be serialised.
	if strings.Contains(string(data), `"delta"`) {
		t.Error("result file recorded the request body")
	}
}

// TestOverwriteWithoutOutIsRejected keeps the flag combinations honest: a flag
// that silently does nothing is worse than one that is refused.
func TestOverwriteWithoutOutIsRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"run", "--scenario", scenarioPath("no-fault-baseline.json"), "--overwrite",
	}, &stdout, &stderr); code != exitUsage {
		t.Errorf("exit code = %d, want %d; stderr:\n%s", code, exitUsage, stderr.String())
	}
}

// TestOutRefusesAnUnwritablePathBeforeRunning covers the other way a result
// path fails late: the file name is free but the directory is not writable.
func TestOutRefusesAnUnwritablePathBeforeRunning(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"run", "--scenario", scenarioPath("no-fault-baseline.json"),
		"--out", filepath.Join(dir, "run.json"),
	}, &stdout, &stderr); code != exitUsage {
		t.Errorf("exit code = %d, want %d; stderr:\n%s", code, exitUsage, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("a run executed before the path was refused:\n%s", stdout.String())
	}
}

// TestRunWritesTracesAndReplayReproducesThem covers GO-T14 for the replay
// command: the trace an operator is told to replay must actually replay, and
// the replayed verdict must carry the same exit code the run reported.
func TestRunWritesTracesAndReplayReproducesThem(t *testing.T) {
	cases := []struct {
		scenario string
		wantCode int
	}{
		{"lost-response.json", exitInvariant},
		{"lost-response-safe.json", exitPass},
	}

	for _, tc := range cases {
		t.Run(tc.scenario, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "traces")

			var runOut, runErr bytes.Buffer
			code := run([]string{
				"run", "--scenario", scenarioPath(tc.scenario), "--runs", "2", "--traces", dir,
			}, &runOut, &runErr)
			if code != tc.wantCode {
				t.Fatalf("run exit code = %d, want %d; stderr:\n%s", code, tc.wantCode, runErr.String())
			}
			if !strings.Contains(runOut.String(), "Traces: "+dir) {
				t.Errorf("run did not report where it wrote traces:\n%s", runOut.String())
			}

			tracePath := filepath.Join(dir, "case-0002.json")
			if _, err := os.Stat(tracePath); err != nil {
				t.Fatalf("expected one trace per case: %v", err)
			}

			var repOut, repErr bytes.Buffer
			if code := run([]string{"replay", "--trace", tracePath}, &repOut, &repErr); code != tc.wantCode {
				t.Fatalf("replay exit code = %d, want %d; stderr:\n%s", code, tc.wantCode, repErr.String())
			}
			for _, want := range []string{"Replaying case-0002", "Replay reproduced"} {
				if !strings.Contains(repOut.String(), want) {
					t.Errorf("replay output did not contain %q:\n%s", want, repOut.String())
				}
			}
		})
	}
}

// TestRunPointsAtTheTraceOfAFailingCase checks the PRD's reproduce line, which
// is the only way an operator learns that replay exists.
func TestRunPointsAtTheTraceOfAFailingCase(t *testing.T) {
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	run([]string{
		"run", "--scenario", scenarioPath("lost-response.json"), "--runs", "2", "--traces", dir,
	}, &stdout, &stderr)

	want := "Reproduce: faulttest replay --trace " + filepath.Join(dir, "case-0001.json")
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("output did not contain %q:\n%s", want, stdout.String())
	}

	// A clean run has nothing to reproduce, so the line would be noise.
	var passOut, passErr bytes.Buffer
	run([]string{
		"run", "--scenario", scenarioPath("no-fault-baseline.json"), "--traces", t.TempDir(),
	}, &passOut, &passErr)
	if strings.Contains(passOut.String(), "Reproduce:") {
		t.Errorf("a passing run suggested reproducing a case:\n%s", passOut.String())
	}
}

// TestReplayRejectsUnusableTraces covers the PRD's exit-code table, which
// classifies a corrupted trace as infrastructure failure rather than as a
// finding about the application.
func TestReplayRejectsUnusableTraces(t *testing.T) {
	dir := t.TempDir()

	var runOut, runErr bytes.Buffer
	run([]string{
		"run", "--scenario", scenarioPath("lost-response.json"), "--traces", dir,
	}, &runOut, &runErr)
	good, err := os.ReadFile(filepath.Join(dir, "case-0001.json"))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	write := func(t *testing.T, data []byte) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "trace.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write trace: %v", err)
		}
		return path
	}

	t.Run("missing file", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"replay", "--trace",
			filepath.Join(t.TempDir(), "absent.json")}, &stdout, &stderr); code != exitHarness {
			t.Errorf("exit code = %d, want %d", code, exitHarness)
		}
	})
	t.Run("not a trace", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"replay", "--trace",
			write(t, []byte("garbage"))}, &stdout, &stderr); code != exitHarness {
			t.Errorf("exit code = %d, want %d", code, exitHarness)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"replay", "--trace",
			write(t, good[:len(good)/2])}, &stdout, &stderr); code != exitHarness {
			t.Errorf("exit code = %d, want %d", code, exitHarness)
		}
	})
	t.Run("unsupported version", func(t *testing.T) {
		bumped := bytes.Replace(good, []byte(`"schema_version": 1`), []byte(`"schema_version": 2`), 1)
		if bytes.Equal(bumped, good) {
			t.Fatal("could not change the trace's schema version")
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"replay", "--trace", write(t, bumped)}, &stdout, &stderr); code != exitHarness {
			t.Errorf("exit code = %d, want %d", code, exitHarness)
		}
		if !strings.Contains(stderr.String(), "schema_version") {
			t.Errorf("stderr did not name the problem:\n%s", stderr.String())
		}
	})
}

// TestReplayReportsAMismatchAsInfrastructure pins the exit-code boundary: a
// replay that does not reproduce its record is this tool failing to reproduce
// itself, not the application violating an invariant.
func TestReplayReportsAMismatchAsInfrastructure(t *testing.T) {
	dir := t.TempDir()

	var runOut, runErr bytes.Buffer
	if code := run([]string{
		"run", "--scenario", scenarioPath("lost-response.json"), "--traces", dir,
	}, &runOut, &runErr); code != exitInvariant {
		t.Fatalf("run exit code = %d, want %d", code, exitInvariant)
	}

	data, err := os.ReadFile(filepath.Join(dir, "case-0001.json"))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	cases := []struct {
		name string
		edit func(m map[string]any)
		want string
	}{
		{
			// The recorded verdict is wrong, everything else reproduces.
			"recorded verdict edited",
			func(m map[string]any) { m["expected"].(map[string]any)["verdict"] = "pass" },
			"verdict: replayed fail; recorded pass",
		},
		{
			// The trace claims nothing was injected, so replay must inject
			// nothing and the case must come out clean. If replay fell back to
			// the scenario's own rules it would reproduce the failure instead
			// and this mismatch would read differently.
			"recorded decisions removed",
			func(m map[string]any) { m["fault_decisions"] = []any{} },
			"verdict: replayed pass; recorded fail",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("decode trace: %v", err)
			}
			tc.edit(m)
			doctored, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("encode trace: %v", err)
			}
			path := filepath.Join(t.TempDir(), "case-0001.json")
			if err := os.WriteFile(path, doctored, 0o600); err != nil {
				t.Fatalf("write trace: %v", err)
			}

			var stdout, stderr bytes.Buffer
			if code := run([]string{"replay", "--trace", path}, &stdout, &stderr); code != exitHarness {
				t.Fatalf("exit code = %d, want %d; stdout:\n%s", code, exitHarness, stdout.String())
			}
			for _, want := range []string{"did not reproduce", tc.want} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("output did not contain %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

// TestReplayUsageErrors keeps argument mistakes at exit 2, away from both
// findings and infrastructure failures.
func TestReplayUsageErrors(t *testing.T) {
	cases := [][]string{
		{"replay"},
		{"replay", "--trace", ""},
		{"replay", "--trace", "x.json", "extra"},
		{"replay", "--trace", "x.json", "--timeout", "0s"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
		})
	}
}

// TestTraceFilesCarryNoRequestPayloads is the written-artefact half of the
// redaction rule. The embedded scenario's synthetic delta is permitted
// configuration; captured request data is not.
func TestTraceFilesCarryNoRequestPayloads(t *testing.T) {
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	run([]string{
		"run", "--scenario", scenarioPath("lost-response.json"), "--traces", dir,
	}, &stdout, &stderr)

	data, err := os.ReadFile(filepath.Join(dir, "case-0001.json"))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	risky := regexp.MustCompile(`(?i)"[a-z_]*(header|body|cookie|auth|token|secret|password|credential)[a-z_]*"\s*:`)
	if found := risky.FindAllString(string(data), -1); found != nil {
		t.Errorf("trace file contains field(s) that suggest request data: %v", found)
	}
	if strings.Contains(string(data), `"idempotency_key"`) {
		t.Error("trace file recorded a request header value")
	}
}

// TestTracesRefuseAnExistingFileBeforeRunning keeps the refusal ahead of the
// work, so a rerun into a used directory costs nothing.
func TestTracesRefuseAnExistingFileBeforeRunning(t *testing.T) {
	dir := t.TempDir()
	// case-0004 is outside the three cases this run plans, so the refusal
	// cannot come from an accidental overlap -- and an off-by-one in the
	// planned case IDs cannot pass this by chance.
	if err := os.WriteFile(filepath.Join(dir, "case-0004.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed trace: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"run", "--scenario", scenarioPath("lost-response.json"), "--runs", "3", "--traces", dir,
	}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if strings.Contains(stdout.String(), "Cases:") {
		t.Errorf("the run executed before refusing the trace directory:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--overwrite") {
		t.Errorf("stderr did not say how to proceed:\n%s", stderr.String())
	}
}

// TestCaseIDTraceIsWrittenUnderThatID pins the --case-id path through the
// planned-case-ID list, which decides what the pre-run check looks for.
func TestCaseIDTraceIsWrittenUnderThatID(t *testing.T) {
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"run", "--scenario", scenarioPath("lost-response.json"),
		"--case-id", "my-case", "--traces", dir,
	}, &stdout, &stderr); code != exitInvariant {
		t.Fatalf("exit code = %d, want %d; stderr:\n%s", code, exitInvariant, stderr.String())
	}

	path := filepath.Join(dir, "my-case.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("trace for --case-id was not written as my-case.json: %v", err)
	}
	// And the pre-run check must look for that name, not case-0001.
	var again bytes.Buffer
	if code := run([]string{
		"run", "--scenario", scenarioPath("lost-response.json"),
		"--case-id", "my-case", "--traces", dir,
	}, &again, &again); code != exitUsage {
		t.Errorf("a rerun into the same directory exited %d, want %d", code, exitUsage)
	}

	var repOut, repErr bytes.Buffer
	if code := run([]string{"replay", "--trace", path}, &repOut, &repErr); code != exitInvariant {
		t.Fatalf("replay exit code = %d, want %d; stderr:\n%s", code, exitInvariant, repErr.String())
	}
	if !strings.Contains(repOut.String(), "Replaying my-case") {
		t.Errorf("replay did not run the recorded case:\n%s", repOut.String())
	}
}

// TestTracesThatCannotBeWrittenDoNotReportACleanRun covers the rule that a
// run whose traces never arrived is unreproducible, so it must not exit 0.
func TestTracesThatCannotBeWrittenDoNotReportACleanRun(t *testing.T) {
	dir := t.TempDir()
	// A directory where the trace file belongs: the pre-run probe succeeds,
	// and --overwrite skips the existence refusal, so the failure lands at
	// write time -- which is the path under test.
	if err := os.Mkdir(filepath.Join(dir, "case-0001.json"), 0o755); err != nil {
		t.Fatalf("seed directory: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"run", "--scenario", scenarioPath("no-fault-baseline.json"),
		"--traces", dir, "--overwrite",
	}, &stdout, &stderr)
	if code != exitHarness {
		t.Fatalf("exit code = %d, want %d: the scenario passes, but its traces were never "+
			"written; stderr:\n%s", code, exitHarness, stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not write traces") {
		t.Errorf("stderr did not report the failure:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "Traces: ") {
		t.Errorf("output claimed traces were written:\n%s", stdout.String())
	}
}

// TestEmptyOutputPathsAreRejected keeps --out="$UNSET" from reading as "no
// output requested" in a CI script.
func TestEmptyOutputPathsAreRejected(t *testing.T) {
	for _, flag := range []string{"--out", "--traces"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{
				"run", "--scenario", scenarioPath("no-fault-baseline.json"), flag + "=",
			}, &stdout, &stderr)
			if code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if strings.Contains(stdout.String(), "Cases:") {
				t.Errorf("the run executed before refusing the empty path:\n%s", stdout.String())
			}
		})
	}
}

// TestOutInsideTracesIsRejected: the result file would land on a trace file,
// destroying the one the run's own Reproduce line points at.
func TestOutInsideTracesIsRejected(t *testing.T) {
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"run", "--scenario", scenarioPath("lost-response.json"), "--runs", "2",
		"--traces", dir, "--out", filepath.Join(dir, "case-0002.json"), "--overwrite",
	}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if strings.Contains(stdout.String(), "Cases:") {
		t.Errorf("the run executed before refusing the collision:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), dir) {
		t.Errorf("stderr did not name the collision:\n%s", stderr.String())
	}
}

// TestReplayUsesTheDeadlineFromTheTrace checks the CLI wiring of the recorded
// deadline. Without it the replay runs under the default 5s, the delayed case
// comes out pass instead of inconclusive, and the output says the record did
// not reproduce.
func TestReplayUsesTheDeadlineFromTheTrace(t *testing.T) {
	dir := t.TempDir()

	var runOut, runErr bytes.Buffer
	if code := run([]string{
		"run", "--scenario", scenarioPath("delayed-response.json"),
		"--timeout", "100ms", "--traces", dir,
	}, &runOut, &runErr); code != exitHarness {
		t.Fatalf("run exit code = %d, want %d (inconclusive); stdout:\n%s", code, exitHarness, runOut.String())
	}

	var repOut, repErr bytes.Buffer
	code := run([]string{"replay", "--trace", filepath.Join(dir, "case-0001.json")}, &repOut, &repErr)
	if !strings.Contains(repOut.String(), "Replay reproduced") {
		t.Errorf("replay did not reproduce the recorded case (exit %d):\n%s", code, repOut.String())
	}
	if !strings.Contains(repOut.String(), "deadline 100ms") {
		t.Errorf("replay did not report the recorded deadline:\n%s", repOut.String())
	}
	if code != exitHarness {
		t.Errorf("exit code = %d, want %d: an inconclusive case stays indeterminate", code, exitHarness)
	}
}

// TestInterruptPhases covers what successive interrupts mean. Replacing the
// whole dispatcher with a single cancellation used to leave the suite green,
// so this is the only thing holding that contract.
func TestInterruptPhases(t *testing.T) {
	signals := make(chan os.Signal, 4)
	done := make(chan struct{})
	defer close(done)

	events := make(chan string, 4)
	go interruptPhases(signals,
		done,
		func() { events <- "run" },
		func() { events <- "output" },
		func() { events <- "die" },
	)

	for _, want := range []string{"run", "output", "die"} {
		signals <- os.Interrupt
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("interrupt produced %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no response to the interrupt that should have produced %q", want)
		}
	}

	// Having given up, the dispatcher is finished: it must not keep absorbing
	// signals, because the process has to be able to die.
	signals <- os.Interrupt
	select {
	case got := <-events:
		t.Errorf("the dispatcher acted on a further interrupt (%q) after giving up", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestInterruptPhasesStopsWithTheRun checks the dispatcher releases when the
// command returns, rather than leaving a goroutine blocked on a channel
// nobody will write to again.
func TestInterruptPhasesStopsWithTheRun(t *testing.T) {
	signals := make(chan os.Signal)
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		interruptPhases(signals, done, func() {}, func() {}, func() {})
		close(finished)
	}()

	close(done)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Error("the dispatcher did not return when the command finished")
	}
}

// TestNoReproduceLineWithoutAViolation: an inconclusive case has no outcome to
// reproduce, so nominating one would send the operator to a replay that
// reports a mismatch by construction. After an interrupt every unfinished
// case is inconclusive, which is when this matters most.
func TestNoReproduceLineWithoutAViolation(t *testing.T) {
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"run", "--scenario", scenarioPath("delayed-response.json"),
		"--timeout", "100ms", "--traces", dir,
	}, &stdout, &stderr)
	if code != exitHarness {
		t.Fatalf("exit code = %d, want %d (inconclusive); stdout:\n%s", code, exitHarness, stdout.String())
	}
	if !strings.Contains(stdout.String(), "Inconclusive: 1") {
		t.Fatalf("expected an inconclusive case:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "Reproduce:") {
		t.Errorf("a run with no violation nominated a case to reproduce:\n%s", stdout.String())
	}
	// The trace is still written: it is evidence, even though it is not a
	// reproduction.
	if _, err := os.Stat(filepath.Join(dir, "case-0001.json")); err != nil {
		t.Errorf("the inconclusive case's trace was not written: %v", err)
	}
}
