# Milestones

Each milestone is independently testable: it ends with a named verification
command whose result can be inspected without reference to later work. PRD task
IDs (`GO-0x`) and acceptance IDs (`GO-ACxx`, `GO-Txx`) come from
[`PRD.md`](PRD.md) section 6 and section 4.

| Milestone | PRD tasks | Deliverable | Verification | State |
|---|---|---|---|---|
| **M1 — Lost response, end to end** | GO-00, GO-01, GO-02, GO-03 | Repository scaffold; stateful fixture with a state oracle; loopback HTTP/1.1 fault proxy; all four MVP fault actions; strict scenario schema; single-case execution behind `faulttest run` | `./scripts/verify-full.sh` | **done** |
| **M2 — Concurrent runner** | GO-04 | Bounded worker pool, `--runs` and `--parallel`, per-case IDs and deadlines, aggregate summary, cancellation of a whole run, measured throughput | `./scripts/verify-full.sh` plus 100 cases at parallelism 1 and 16 | **done** |
| **M3 — Result reporting** | GO-06 (part 1) | `--out` structured JSON results, `report` subcommand, redaction rules asserted by test | `./scripts/verify-full.sh` plus a schema-stability test | **done** |
| **M4 — Trace and replay** | GO-06 (part 2) | Per-case trace files, `faulttest replay --trace`, stable payload digests | `./scripts/verify-full.sh` plus a record-then-replay equivalence test | todo |
| **M5 — Hardening and benchmarks** | GO-07 | Resource-limit tests, direct-versus-proxy latency and throughput measurements, README demo walkthrough | `./scripts/verify-full.sh` plus `docs/benchmarks/` with measured numbers | todo |

## How these milestones map onto the PRD's task graph

The PRD numbers tasks GO-00 to GO-07; these milestones group them into
independently testable slices, and the two numbering schemes are not
one-to-one. The mapping, so neither drifts:

| PRD task | Where it was delivered |
|---|---|
| GO-00 to GO-03 | M1 |
| GO-04 scenario runner | M2 |
| GO-05 correctness verdicts | M1 and M2, not a milestone of its own |
| GO-06 trace/replay | split: result JSON in M3, trace and replay in M4 |
| GO-07 hardening/benchmarks | M5 |

**GO-05 has no milestone row because it was already satisfied when M2 ended.**
Its deliverable is "oracle validation, distinct outcomes, safe/unsafe demos"
against GO-AC03, GO-AC10 and GO-AC14, and all three are evidenced in the
coverage table below. Giving it a milestone of its own would have meant
inventing work the PRD does not ask for.

An earlier revision of this file labelled M3 as GO-05. That was wrong: the
result JSON and its redaction rules belong to GO-06, whose acceptance list
names GO-AC09.

## Why M1 is one milestone and not four

PRD tasks GO-00 through GO-03 cannot each stand alone as *demonstrable* work:
a fixture with no proxy cannot lose a response, and a proxy with no state oracle
cannot tell a duplicate from a success. The smallest slice that proves the
product thesis is the whole lost-response path, so M1 carries all four tasks and
ends with the demonstration the PRD names as its headline evidence.

Everything after M1 is genuinely separable, because each later milestone adds a
layer over a working core rather than a part of it.

## Acceptance coverage after M1

| PRD ID | Status | Evidence |
|---|---|---|
| GO-AC01 | met | `TestNoFaultBaseline` (both fixture modes) |
| GO-AC02 | met | `TestLostResponseDuplicatesOnUnsafeFixture` |
| GO-AC03 | met | `TestLostResponseAppliesOnceOnSafeFixture`, `TestSameScenarioSeparatesSafeFromUnsafe` |
| GO-AC04 | met | `TestRejectBeforeForwardLeavesBackendUntouched`, `TestRejectConnectionDoesNotContactUpstream` |
| GO-AC05 | met | `TestDelayedResponseBeyondClientTimeout` |
| GO-AC06 | partial | `TestDecideIsDeterministic` proves stable decisions; trace replay is M4 |
| GO-AC07 | met | `TestRunIsolatesEveryCase` at 1/8/32, `TestHundredCasesAtTwoParallelisms` at 100 cases, `TestRunHonoursTheConcurrencyBound` |
| GO-AC08 | met | `TestCaseDeadlineIsRespected`, `TestRunCancellationStopsDispatchWithoutMisclassifying`, `TestRunCaseCancelledDuringAnAttemptHasNoVerdict`, `leaktest` in every server test |
| GO-AC09 | met | `TestResultSchemaIsStable`, `TestParseRejectsUnusableResults`, `TestResultCarriesNoRequestPayloads`, `TestResultFileCarriesNoRequestPayloads`, `TestParseRejectsInvalidScenarios` |
| GO-AC10 | met | `TestHarnessFailureIsNotAnInvariantFailure`, `TestRunExitCodes`, `TestUsageErrorsExitTwo` |
| GO-AC11 | met | `TestStartRejectsUnsafeUpstreams`, oversized-body and bad-header tests, `TestRunSpecBounds`, and the `--runs`/`--parallel` subcases of `TestUsageErrorsExitTwo` |
| GO-AC12 | met | `go vet`, `go test`, `go test -race` all clean; no known flaky tests |
| GO-AC13 | not started | M5 |
| GO-AC14 | met | README demo commands, exercised by `cmd/faulttest` tests |

`partial` means the part of the criterion that this milestone's scope covers is
tested; the rest depends on work that is explicitly later.

## Throughput

M2 measures how fast the harness itself executes cases, because a concurrent
runner that is not faster is not worth its complexity. The measurement, its
method and its caveats are in [`benchmarks/M2-throughput.md`](benchmarks/M2-throughput.md).

It is deliberately not the PRD's GO-AC13 benchmark: that one compares the
proxy against a direct connection and reports added latency, memory and
goroutine counts, and it remains M5.
