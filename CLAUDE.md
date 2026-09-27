# Agent instructions

## Orientation
Read `docs/PRD.md` (product contract), `docs/STATUS.md` (current state), and
`docs/MILESTONES.md` (independently testable slices) before changing code.
Inspect the existing packages and tests before planning work.
Durable interface decisions live in `docs/decisions/`; task contracts in `docs/features/`.

## Scope discipline
Keep every change inside the active task's acceptance criteria and non-goals.
The PRD's `LATER` sections are out of scope unless explicitly approved.
No speculative abstractions: add an interface only when a second real
implementation or test double exists.

## Verification
- `./scripts/verify.sh` — gofmt, go vet, go build, go test. Run on every edit batch.
- `./scripts/verify-full.sh` — adds `go test -race` and the integration suite.
  Run before every commit. Development is main-first, so this is the only gate.
Never weaken, skip, or delete a test to obtain a pass. Never change acceptance
criteria to match the implementation; stop and escalate the contradiction instead.
Maximum two autonomous repair cycles per distinct persistent failure, then report evidence.
A flaky test is a defect, not noise: find the cause. Concurrency defects in this
repository have surfaced only under full-suite load, so a green single-package
run is not evidence. Repeat the race suite before trusting a concurrency fix.

## Independent review
Substantial changes get an independent, fresh-context review before they are
committed: concurrency and cancellation, HTTP connection semantics, wire
formats, public interfaces, or any change touching more than three packages.
Give the reviewer the task contract, the diff, and the test evidence, not the
reasoning that produced them. Record in `docs/STATUS.md` that the review ran
and what it changed. A review never substitutes for `verify-full.sh`.

## Safety rules (non-negotiable)
This tool deliberately breaks HTTP request semantics and can duplicate side effects.
- Fixtures and the proxy bind `127.0.0.1` on ephemeral ports only.
- Refuse non-loopback upstream targets in code, not in documentation.
- Bound every resource: body size (64 KiB), per-case deadline, concurrency, sockets.
- Never infer committed backend state from an HTTP status code; the state oracle decides.
- Classify unknown outcomes as `inconclusive` / `harness_error`, never as a product bug.
- Never record request bodies or sensitive headers in traces or logs.
- No public network access in tests or CI.

## Go conventions
Standard library only; a new dependency needs human approval.
`gofmt` clean, `go vet` clean, no data races. Propagate `context.Context`.
Close and drain response bodies. Shut down every listener and goroutine you start;
tests assert no goroutine leaks.

## Git
Development is main-first: commit directly to `main`, with no feature branch and
no pull request for routine work. `./scripts/verify-full.sh` must pass first.
Do not force-push, rewrite published commits, or push without being asked.
Commit only verified milestones, with descriptive subjects under 10 words
and no conventional-commit prefixes or tool names.
Commit dates: the owner placed the M1-M2 history across August and September
2026; the decision and what it means are recorded in `docs/STATUS.md`. An agent
never chooses, sets, or rewrites commit dates on its own. Ask.

## Human approval required
Architecture or wire-format changes, new dependencies, scope expansion,
credential use, remote or destructive operations, merging.

## Handoff
Before finishing, update `docs/STATUS.md` with what changed, the exact verification
commands and their results, what is not done, the next action, and branch/commit IDs.
