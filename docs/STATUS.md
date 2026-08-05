# Current state

Milestone: M1 | Active task: GO-00 (from [`PRD.md`](PRD.md))
Branch: main
Last known good commit: NONE
Status: in progress

Done: repository scaffold — Go module pinned to 1.27.4, `.gitignore`,
`CLAUDE.md`, README stub, PRD and shared workflow under `docs/`,
`scripts/verify.sh` and `scripts/verify-full.sh`, GitHub Actions running both,
and a `faulttest` CLI skeleton that serves `version` and `help` with the exit
codes the PRD specifies.

Evidence: `./scripts/verify-full.sh` PASS on 2026-11-10 (gofmt clean, `go vet`
clean, `go build`, `go test`, `go test -race`).

Not done: everything in the product. No fixture, no proxy, no fault injection,
no scenario parsing, no oracle.

Known issues / failed approaches: none yet.

Next action: implement GO-01, the stateful fixture and its state oracle, as the
first piece of the lost-response path.

Human decision needed: none.
