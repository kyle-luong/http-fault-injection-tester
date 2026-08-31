# Current state

Milestone: M1 — **complete** | Next task: GO-04 (from [`PRD.md`](PRD.md))
Branch: feat/GO-01-lost-response-core (not merged; awaiting review)
Last known good commit: see `git log` on this branch; previous checkpoint 1880bea
Status: ready for review

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

## Evidence

`./scripts/verify-full.sh` PASS on 2026-11-10, Go 1.27.4, darwin/arm64:

| Command | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` | clean |
| `go test -count=1 ./...` | all packages pass |
| `go test -race -count=1 ./...` | all packages pass, no races reported |
| `go test -count=5 ./...` | pass — no flakiness observed |
| `go test -race -count=3 ./...` | pass — no flakiness observed |

Every commit on this branch was verified in a clean, isolated `git worktree`
rather than against the working tree, so each one stands on its own.

The suite was mutation-checked twice to confirm it has teeth. Disabling
deduplication in the safe fixture failed `TestSafeModeDeduplicatesByIdempotencyKey`,
`TestLostResponseAppliesOnceOnSafeFixture` and two others. Moving the
`close_client_connection` fault to before forwarding failed
`TestLostResponseDuplicatesOnUnsafeFixture`,
`TestCloseAfterUpstreamResponseLosesTheResponse` and three others. Both
mutations were reverted.

No performance numbers are published anywhere in this repository. Measurement is
M5.

## Not done

Deliberately out of M1 scope, in milestone order: the bounded concurrent worker
pool with `--runs` and `--parallel` (M2), structured JSON results and the
`report` subcommand (M3), trace recording and `replay` (M4), resource-limit
hardening and the direct-versus-proxy benchmark (M5).

`internal/runner` executes exactly one case today. Its shared state is already
concurrency-safe and is exercised by a 32-case concurrent isolation test under
`-race`, so M2 adds a pool over it rather than restructuring it.

## Known issues / failed approaches

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
`--parallel`, per-case IDs, and whole-run cancellation.

## Human decision needed

**The commit-dating instruction conflicts with this repository's own policy.**
The request was to place these commits in history sparsely across August and
September. [`00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md`](00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md)
section 7 states: "Never rewrite true Git author/committer dates to fabricate
previous activity. If authentic work predates the repository, import it now with
a clear note of the genuine evidence and original artifact dates, preserving
actual commit metadata."

This work was done on 2026-11-10 and the commits carry that true date. Rewriting
them to August or September would misrepresent when the work happened, in a
repository the PRD describes as job-application evidence. Rewriting the dates is
still mechanically possible on this unmerged branch if you decide otherwise; it
is your call, not one to make silently.
