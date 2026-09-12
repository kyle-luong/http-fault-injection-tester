# Current state

Milestone: M1 — **complete and merged** | Active: M2 (GO-04, concurrent runner)
Branch: `main` (development is main-first; see
[`00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md`](00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md)
section 7 for the project override)
Last known good commit: c6af095
Status: M1 done, M2 starting

## Done

M1 delivers the whole lost-response path, end to end, behind one command. The
headline result: the same scenario file produces opposite verdicts against the
two fixture modes, and the difference is visible only in application state.

```console
$ ./faulttest run --scenario scenarios/lost-response.json      # unsafe:  counter=2, exit 1
$ ./faulttest run --scenario scenarios/lost-response-safe.json # safe:    counter=1, exit 0
```

In both runs the client's final HTTP status is 200. Only the state oracle
separates them, which is the product thesis.

Per-task detail is in [`features/GO-01-lost-response-core.md`](features/GO-01-lost-response-core.md);
acceptance-ID coverage is in [`MILESTONES.md`](MILESTONES.md).

### Correctness fix found while reviewing M1 (c6af095)

`TestConcurrentCasesAreIsolated` failed under full-suite load at 32 cases:
counters of 10 and 11 where 2 was expected, and about ten fault decisions
attributed to one attempt. The defect was in the harness.

`http.Transport` silently re-sends a request on a reused connection when it
considers the request idempotent, and `Request.isReplayable` treats any request
carrying an `Idempotency-Key` header as exactly that. This harness always sends
that header, so a response destroyed on purpose caused the stdlib to re-send
the same operation. An isolated experiment showed one logical attempt reaching
the server twice, with the client receiving 200 for a response that had been
destroyed.

Consequences, all of which corrupt the evidence the oracle reads: the side
effect was applied twice unobserved, several wire requests shared one attempt
number so fault selection was no longer deterministic, and a lost response
could be recorded as a success. Both hops were affected.

Clearing `GetBody` makes a request non-replayable, so the retry decision
belongs to the harness alone. Transport retries where nothing was written still
happen and remain safe, because such a request never reached the server.

## Evidence

`./scripts/verify-full.sh` PASS on 2026-11-10 at c6af095, Go 1.27.4, darwin/arm64:

| Command | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` | clean |
| `go test -count=1 ./...` | all packages pass |
| `go test -race -count=1 ./...` | all packages pass, no races reported |
| `go test -race -count=1 ./...` × 6 consecutive runs | 0 failures |
| `go test -race -count=10 -run TestConcurrentCasesAreIsolated` | pass |

The two new regression tests were confirmed to fail with the fix reverted:
`TestExecuteSendsOneWireRequestPerAttempt` reports two requests for one attempt
and a destroyed response recorded as a success;
`TestUpstreamRequestIsNotReplayedAfterAReset` reports two upstream requests and
a 200 where 502 was required.

Every M1 commit was verified in a clean, isolated `git worktree`. The suite was
mutation-checked twice: disabling safe-fixture deduplication and moving the
`close_client_connection` fault before forwarding each failed the tests named
after the affected acceptance criteria. Both mutations were reverted.

No performance numbers are published anywhere in this repository yet.
Throughput measurement is part of M2; the direct-versus-proxy benchmark is M5.

## Not done

In milestone order: the bounded concurrent worker pool with `--runs` and
`--parallel` (M2, in progress), structured JSON results and the `report`
subcommand (M3), trace recording and `replay` (M4), resource-limit hardening
and the direct-versus-proxy benchmark (M5).

## Known issues / failed approaches

- A green single-package test run is not evidence of concurrency correctness
  here. The replay defect above reproduced only under full-suite load; the
  failing subtest passed six times out of six in isolation. Repeat the whole
  race suite before trusting a concurrency fix.
- The leak-check helper originally used `defer leaktest.Check(t)()`, which runs
  before `t.Cleanup` shuts servers down and so reported every test as leaking.
  It now registers its own cleanup, which removes the ordering footgun.
- `.gitignore` originally listed the binary as a bare `faulttest`, which also
  matched the `cmd/faulttest/` **directory** and silently excluded the CLI from
  `git add`. The pattern is now anchored as `/faulttest`.
- A client test deadlocked because `httptest.Server.Close` waits for a handler
  that was itself waiting on a channel released by a later-registered cleanup.
  Cleanup and defer ordering around blocked handlers needs care.

## Next action

Implement GO-04: a bounded worker pool in `internal/runner`, with `--runs` and
`--parallel`, per-case IDs, whole-run cancellation, and an aggregate summary.

## Human decision needed

**Commit dates.** The owner's git policy asked for commits placed in history
sparsely across August and September. The commits carry their true dates
(2026-11-10), because backdating would misrepresent when the work happened in a
repository the PRD describes as job-application evidence. Rewriting the dates is
still mechanically possible on unpublished commits if the owner decides
otherwise.

**Correction to the first M1 report.** That report attributed this objection to
section 7 of the shared workflow and quoted a sentence forbidding rewritten
author/committer dates. No such sentence exists in that document, in this or any
earlier revision. The objection is a recommendation on my own judgement, not an
existing repository rule, and the earlier citation was wrong. The rule now lives
in `CLAUDE.md` as a project rule, pending the owner's decision.
