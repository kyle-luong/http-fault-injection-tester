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
| **M4 — Trace and replay** | GO-06 (part 2) | `--traces DIR` per-case trace files, `faulttest replay --trace`, stable payload digests, replay-versus-record comparison | `./scripts/verify-full.sh` plus `TestTraceReplayReproducesTheRecordedCase` | **done** |
| **M5 — Hardening and benchmarks** | GO-07 | Boundary tests for the GO-T13 cases, settlement covering every request the proxy accepted, interrupt phases, direct-versus-proxy benchmark; contract in [`features/M5-hardening.md`](features/M5-hardening.md) | `./scripts/verify-full.sh` plus [`benchmarks/M5-proxy-overhead.md`](benchmarks/M5-proxy-overhead.md) | **done**, with one open defect in [`STATUS.md`](STATUS.md) |

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
| GO-AC05 | met | `TestDelayedResponseBeyondClientTimeout`, `TestStateIsNotReadWhileTheProxyStillHoldsARequest` |
| GO-AC06 | met | `TestDecideIsDeterministic` for stable decisions; `TestTraceReplayReproducesTheRecordedCase` for record-then-replay equivalence across four scenarios; `TestReplayDetectsATraceThatNoLongerDescribesItsRun` for the mismatch path; `TestReplayUsesTheRecordedDeadline` for the one configuration value a scenario does not hold; `TestPerCaseDecisionsSurviveTheLogBound` for the truncation that used to make a faulted case record an empty decision list |
| GO-AC07 | met | `TestRunIsolatesEveryCase` at 1/8/32, `TestHundredCasesAtTwoParallelisms` at 100 cases, `TestRunHonoursTheConcurrencyBound` |
| GO-AC08 | met | `TestCaseDeadlineIsRespected`, `TestRunCancellationStopsDispatchWithoutMisclassifying`, `TestRunCaseCancelledDuringAnAttemptHasNoVerdict`, `leaktest` in every server test |
| GO-AC09 | met | `TestResultSchemaIsStable`, `TestParseRejectsUnusableResults`, `TestResultCarriesNoRequestPayloads`, `TestResultFileCarriesNoRequestPayloads`, `TestParseRejectsInvalidScenarios`, `TestParseRejectsUnusableTraces`, `TestTraceSchemaIsExactlyThis`, `TestTraceFilesCarryNoRequestPayloads` |
| GO-AC10 | met | `TestHarnessFailureIsNotAnInvariantFailure`, `TestRunExitCodes`, `TestUsageErrorsExitTwo` |
| GO-AC11 | met | `TestStartRejectsUnsafeUpstreams`, oversized-body and bad-header tests, `TestRunSpecBounds`, `TestLoadRejectsAnOversizedTrace`, and the `--runs`/`--parallel` subcases of `TestUsageErrorsExitTwo` |
| GO-AC12 | met | `go vet`, `go test`, `go test -race` all clean; no known flaky tests |
| GO-AC13 | met | [`benchmarks/M5-proxy-overhead.md`](benchmarks/M5-proxy-overhead.md): `BenchmarkOperation` direct and through the proxy at parallelism 1, 8 and 32, five trials each, with latency percentiles, allocation and goroutine counts and the host's load recorded |
| GO-AC14 | met | README demo commands, exercised by `cmd/faulttest` tests |

**Every GO-AC01–14 criterion is now evidenced**, so no row is `partial` any
more; the word survives in the table's history only where a milestone note
explains what was outstanding at the time.

One qualification on GO-AC05 and GO-AC10, which both rest on the oracle being
read after settlement: settlement is authoritative for every request the proxy
accepted, and blind to one the client sent that the server has not accepted
yet. A case can therefore carry a verdict about state that later changed,
measured at roughly one in three hundred under CPU load and zero on a quiet
host. The scenarios these criteria are evidenced with do not trigger it, the
measurement and the proposed fix are in
[`features/M5-hardening.md`](features/M5-hardening.md), and the fix is the
owner's decision because it changes the settlement contract.

The test IDs the PRD names for trace and replay are evidenced as follows:
GO-T07 by `TestDecideIsDeterministic` and `TestRulesAreTheRecordedDecisions`,
GO-T08 by `TestTraceReplayReproducesTheRecordedCase`, and GO-T14 by
`TestRunWritesTracesAndReplayReproducesThem`, `TestReplayRejectsUnusableTraces`,
`TestReplayReportsAMismatchAsInfrastructure`, `TestReplayUsageErrors`,
`TestCaseIDTraceIsWrittenUnderThatID` and `TestReplayUsesTheDeadlineFromTheTrace`.

The remaining detailed test IDs: GO-T11 by `TestRunExitCodes`,
`TestExitCodePrecedence` and `TestUsageErrorsExitTwo`; GO-T12 by the `-race`
suite; GO-T15 by `BenchmarkOperation`. GO-T13 names five boundaries, each with its
own test:

| Boundary | Test |
|---|---|
| Empty response | `TestExecuteTreatsAnUndecodableResponseAsNoResponse` (empty body, non-JSON, and a 200 that is not the fixture's contract) |
| Server 500 | `TestUpstreamErrorStatusIsForwardedUnchanged` |
| Reset failure | `TestCloseAfterUpstreamResponseLosesTheResponse`, `TestExecuteSendsOneWireRequestPerAttempt`, `TestUpstreamRequestIsNotReplayedAfterAReset` |
| Oversized body | `TestOversizedBodyIsRejected` (fixture), `TestOversizedRequestBodyIsRejected` and `TestReadBoundedRejectsOversizedInput` (proxy) |
| Slow or absent oracle | `TestFetchStateGivesUpOnASlowOracle` for a state endpoint that never answers, `TestHarnessFailureIsNotAnInvariantFailure` for one that is gone |

`TestStateIsNotReadWhileTheProxyStillHoldsARequest` is listed under GO-AC05
rather than here: it stalls the proxy, not the oracle, and what it establishes
is the settlement boundary.

`--traces DIR` is a separate flag from `--out PATH`, which the owner approved on
2026-11-12; the PRD's CLI table now documents both, since traces are one file
per case and deriving them from `--out` would write up to 10,000 files for
anyone who asked only for a result.

## Throughput

M2 measures how fast the harness itself executes cases, because a concurrent
runner that is not faster is not worth its complexity. The measurement, its
method and its caveats are in [`benchmarks/M2-throughput.md`](benchmarks/M2-throughput.md).

It is deliberately not the PRD's GO-AC13 benchmark: that one compares the
proxy against a direct connection and reports added latency, memory and
goroutine counts, and it remains M5.
