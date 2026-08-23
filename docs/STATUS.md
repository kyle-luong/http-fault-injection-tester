# Current state

Milestone: M1 | Active task: GO-03, GO-04 schema (from [`PRD.md`](PRD.md))
Branch: feat/GO-01-lost-response-core
Last known good commit: 5526279
Status: in progress

Done: the three pieces that sit either side of the proxy.

- `internal/client`: the retry policy. Each attempt sends a byte-identical body
  and, when the policy reuses the key, the same idempotency key — the condition
  under which a correct server may deduplicate and an incorrect one duplicates.
  A transport error is recorded as data, not returned as a harness failure,
  because ambiguity is the behaviour under test.
- `internal/verify`: the state oracle and the verdict vocabulary. `fail` is
  reserved for the application under test; `inconclusive` and `harness_error`
  exist so the tool never reports a bug it did not establish.
- `internal/scenario`: strict v1 parsing. Unknown keys, unknown enum values,
  impossible rules and ambiguous schedules are all refused before anything
  starts, because a scenario that silently does nothing is worse than one that
  refuses to run.

Five example scenarios are checked in under `scenarios/` and a test parses
every one of them, so the shipped examples cannot rot.

Evidence: `./scripts/verify-full.sh` PASS on 2026-11-10, including 27 scenario
rejection cases.

Not done: nothing wires these together yet. The CLI still only serves `version`
and `help`, and no test has yet observed a duplicate caused by a lost response.

Known issues / failed approaches: none outstanding.

Next action: add the single-case runner and the CLI `run` subcommand, then prove
end to end that the unsafe fixture duplicates and the safe one does not.

Human decision needed: none.
