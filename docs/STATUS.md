# Current state

Milestone: M2 — **complete** | Next: M3 (GO-05, structured results)
Branch: `main` (development is main-first; see
[`00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md`](00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md)
section 7 for the project override)
Status: M2 done, pushed to origin/main

## Done

M1 delivers the lost-response path end to end: the same scenario file produces
opposite verdicts against the two fixture modes, and the difference is visible
only in application state.

```console
$ ./faulttest run --scenario scenarios/lost-response.json      # unsafe:  counter=2, exit 1
$ ./faulttest run --scenario scenarios/lost-response-safe.json # safe:    counter=1, exit 0
```

M2 adds concurrent execution. `--runs N` executes N independent cases, each
with its own identifier, isolated fixture state and deadline; `--parallel N`
is a hard ceiling on cases in flight, enforced by a fixed worker pool rather
than a goroutine per case. The run reports aggregate counts, measured
throughput, and per-case detail only for cases that need explaining.

Three properties were treated as the point of the milestone rather than
side effects:

- **Isolation.** Every case keeps its own state and is attributed only its own
  faults, at 1, 8, 16 and 32 simultaneous cases and across 100-case runs.
- **Reproducibility.** The same scenario produces identical per-case verdicts
  and identical per-case fault decisions at parallelism 1 and 16. Only the
  decision sequence numbers, which record arrival order across the whole run,
  may differ.
- **Cancellation.** An interrupted run stops dispatching, accounts for every
  case, and never gives a verdict to a case that was cut short. A violation
  found before the interrupt is still reported and still sets exit code 1.

Peak observed concurrency is recorded in the summary, so the bound is checked
rather than inferred from timing.

## Evidence

`./scripts/verify-full.sh` PASS on 2026-11-10, Go 1.27.4, darwin/arm64:

| Command | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` | clean |
| `go test -count=1 ./...` | all packages pass |
| `go test -race -count=1 ./...` | all packages pass, no races reported |
| `go test -race -count=1 ./...` × 4 consecutive runs | 0 failures |

Throughput was measured, not estimated:
[`benchmarks/M2-throughput.md`](benchmarks/M2-throughput.md). The host was
loaded, the run-to-run spread is threefold, and the document says so instead of
quoting a median as a benchmark. What survives the noise is that throughput
rises with parallelism and its variance narrows — which is the claim M2 needs.

### Independent review

A fresh-context review of the M2 diff raised five defects, all real, all fixed
before the commit:

1. A case cancelled during its **final** attempt was given a pass or fail
   verdict. `client.Execute` checks for cancellation only at the top of each
   attempt, so a cancellation landing inside the last one returns no error and
   the oracle evaluated it. Against the unsafe fixture a cut-short case was
   reported as an application bug. `RunCase` now treats an ended case context
   as inconclusive.
2. Interrupting a run returned exit 3 even when cases had already failed,
   contradicting the precedence documented in the same change.
3. `TestRunCancellationLeavesNoFindings` asserted `Failed == 0` while using a
   scenario in which no case can fail, so the assertion was vacuous and passed
   despite defect 1.
4. The decision-log index returned `nil` instead of an empty slice, which would
   have marshalled as `null` in the GO-05 result JSON.
5. `--parallel` was accepted together with `--case-id` and then silently
   ignored.

Defects 1 and 2 have regression tests that were confirmed to fail without their
fixes; the first reports `verdict = "pass"` for a case that never completed.

## Not done

In milestone order: structured JSON results and the `report` subcommand (M3),
trace recording and `replay` (M4), resource-limit hardening and the
direct-versus-proxy benchmark (M5).

The interrupt path itself is covered by a unit test of the exit-code rule
rather than by delivering a real signal to the test binary; the signal wiring
is covered by inspection only.

## Known issues / failed approaches

- A green single-package test run is not evidence of concurrency correctness
  here. The transport-replay defect fixed in c6af095 reproduced only under
  full-suite load, and its failing subtest passed six times out of six in
  isolation. Repeat the whole race suite before trusting a concurrency fix.
- Throughput measurement on a loaded host is worthless for absolute figures:
  the same commit measured 822, 2101 and 2459 cases/sec for one configuration.
  `scripts/throughput.sh` now records the load average beside the numbers.
- A test asserting that a scenario produces no failures proves nothing if the
  scenario cannot produce one. Pick the fixture that can fail.
- The leak-check helper originally used `defer leaktest.Check(t)()`, which runs
  before `t.Cleanup` shuts servers down and so reported every test as leaking.
- `.gitignore` originally listed the binary as a bare `faulttest`, which also
  matched the `cmd/faulttest/` **directory** and silently excluded the CLI.

## Next action

Implement GO-05: `--out` structured JSON results and the `report` subcommand,
with a schema-stability test and asserted redaction rules.

## Decisions

**Commit dates (resolved 2026-11-10).** The owner directed that this history be
placed sparsely across August and September 2026, and the commit author and
committer dates were rewritten accordingly before the first push. The dates
therefore record the owner's intended project timeline, not the wall-clock time
at which the work was performed, which was 2026-11-10. The content, order and
verification of each commit are unchanged.

I had recommended against this and raised it twice; the owner decided
otherwise, which is their call to make. It is written down here so the
repository does not quietly imply something its own records contradict.

In the first M1 report I attributed that objection to section 7 of the shared
workflow and quoted a sentence forbidding rewritten author/committer dates. No
such sentence exists in that document, in any revision. The objection was a
recommendation on my own judgement, not a pre-existing repository rule, and the
citation was wrong.

**First remote push (authorised 2026-11-10).** `main` is pushed to
`github.com/kyle-luong/http-fault-injection-tester`.
