# Current state

Milestone: M1 | Active task: GO-02, GO-03 (from [`PRD.md`](PRD.md))
Branch: feat/GO-01-lost-response-core
Last known good commit: f851189
Status: in progress

Done: the fault proxy. A loopback-only HTTP/1.1 forwarding proxy with all four
fault actions the PRD specifies — `reject_connection` and `delay_ms` at
`before_forward`, `close_client_connection` and `delay_ms` at
`after_upstream_response`. The lost-response mechanism works: the proxy reads a
complete upstream response, then hijacks the client connection and closes it
with `SetLinger(0)` so the client sees a reset rather than an ambiguous EOF.

Fault selection is keyed by (case ID, attempt, stage) and never by timing. The
proxy also tracks in-flight upstream calls per case and exposes
`WaitForQuiescence`, which is the settlement boundary an oracle needs: once a
complete upstream response has been read, the backend has committed or refused.

Loopback is enforced twice — when parsing the upstream URL (no DNS lookup, so
validation cannot touch a network) and again in the dialer's `Control` hook.

Evidence: `./scripts/verify-full.sh` PASS on 2026-11-10. Tests assert that a
`before_forward` rejection never reaches the upstream and that a
`close_client_connection` fault reaches it exactly once.

Not done: scenario parsing, the retry client, the oracle's verdict logic, and
the CLI's `run` subcommand. Nothing yet ties a fault to an observed duplicate.

Known issues / failed approaches: none outstanding.

Next action: implement the retry client, the state oracle, and strict scenario
validation, so a scenario file can describe the whole interaction.

Human decision needed: none.
