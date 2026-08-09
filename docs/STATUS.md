# Current state

Milestone: M1 | Active task: GO-01 (from [`PRD.md`](PRD.md))
Branch: feat/GO-01-lost-response-core
Last known good commit: 1caea2c
Status: in progress

Done: the application under test. A loopback HTTP/1.1 fixture with two modes —
`unsafe-counter` applies every successful operation, `safe-counter` applies at
most one per idempotency key — plus `POST /__test/reset` and
`GET /__test/state` so an oracle can observe what was actually applied. State
is isolated per case ID. The state projection lists the key of every applied
operation, so a duplicate is visible as a repeated entry and not only as a
counter value. A goroutine-leak check is asserted by every server test.

Evidence: `./scripts/verify-full.sh` PASS on 2026-11-10. `go test -race` clean,
including a 32-case concurrent isolation test.

Not done: the proxy, fault injection, scenario parsing, the oracle's verdict
logic, and the CLI's `run` subcommand.

Known issues / failed approaches: the first leak-check helper used
`defer leaktest.Check(t)()`, which runs before `t.Cleanup` shuts servers down
and therefore reported every test as leaking. It now registers its own cleanup
so ordering cannot be got wrong at the call site.

Next action: implement GO-02 and GO-03 — the forwarding proxy and its fault
stages — against this fixture.

Human decision needed: none.
