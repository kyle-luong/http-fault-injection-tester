# Milestones

Each milestone is independently testable: it ends with a named verification
command whose result can be inspected without reference to later work. PRD task
IDs (`GO-0x`) and acceptance IDs (`GO-ACxx`, `GO-Txx`) come from
[`PRD.md`](PRD.md) section 6 and section 4.

| Milestone | PRD tasks | Deliverable | Verification | State |
|---|---|---|---|---|
| **M1 — Lost response, end to end** | GO-00, GO-01, GO-02, GO-03 | Repository scaffold; stateful fixture with a state oracle; loopback HTTP/1.1 fault proxy; all four MVP fault actions; strict scenario schema; single-case execution behind `faulttest run` | `./scripts/verify-full.sh` | **done** |
| **M2 — Concurrent runner** | GO-04 | Bounded worker pool, `--runs` and `--parallel`, per-case IDs and deadlines, aggregate summary, cancellation of a whole run | `./scripts/verify-full.sh` plus 100 cases at parallelism 1 and 16 | todo |
| **M3 — Result reporting** | GO-05 | `--out` structured JSON results, `report` subcommand, redaction rules asserted by test | `./scripts/verify-full.sh` plus a schema-stability test | todo |
| **M4 — Trace and replay** | GO-06 | Per-case trace files, `faulttest replay --trace`, stable payload digests | `./scripts/verify-full.sh` plus a record-then-replay equivalence test | todo |
| **M5 — Hardening and benchmarks** | GO-07 | Resource-limit tests, direct-versus-proxy latency and throughput measurements, README demo walkthrough | `./scripts/verify-full.sh` plus `docs/benchmarks/` with measured numbers | todo |

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
| GO-AC07 | partial | `TestConcurrentCasesAreIsolated` proves isolation at 1/8/32; the worker pool is M2 |
| GO-AC08 | partial | `TestCaseDeadlineIsRespected` plus `leaktest` in every server test; mid-run cancellation is M2 |
| GO-AC09 | partial | Strict scenario validation (`TestParseRejectsInvalidScenarios`) and `TestOutputCarriesNoRequestPayloads`; result JSON schema is M3 |
| GO-AC10 | met | `TestHarnessFailureIsNotAnInvariantFailure`, `TestRunExitCodes`, `TestUsageErrorsExitTwo` |
| GO-AC11 | partial | `TestStartRejectsUnsafeUpstreams`, oversized-body and bad-header tests; concurrency bounds are M2 |
| GO-AC12 | met | `go vet`, `go test`, `go test -race` all clean; no known flaky tests |
| GO-AC13 | not started | M5 |
| GO-AC14 | met | README demo commands, exercised by `cmd/faulttest` tests |

`partial` means the part of the criterion that this milestone's scope covers is
tested; the rest depends on work that is explicitly later.
