# M4: per-case traces and replay

PRD criteria: GO-AC06, GO-AC09 (trace half), GO-T07, GO-T08, GO-T14
Format decision: [`../decisions/ADR-002-self-contained-replay-trace.md`](../decisions/ADR-002-self-contained-replay-trace.md)

## Behavior

`faulttest run --traces DIR` writes one trace file per executed case next to the
run's structured result. A trace is self-contained: `faulttest replay --trace
DIR/case-0001.json` reconstructs the controlled fixture, injects exactly the
fault decisions the trace recorded, and then compares the replay with the record
on four axes -- the injected decisions, the number of attempts, the observed
fixture state, and the correctness verdict.

The request inputs are not one of those axes. They are checked when the trace is
loaded, against the scenario the replay is about to run, and a disagreement
rejects the file before anything starts.

Replay takes no scenario argument, no `--timeout`, and reads no file but the
trace. The per-case deadline is part of the record, because a case replayed
under a different deadline is a different case: the delayed-response scenario is
`inconclusive` under 100ms and `pass` under the 5s default.

## What a trace records, and what it must never record

Approved by the owner on 2026-11-12, and the reason the trace may embed a
scenario at all:

> For built-in synthetic test fixtures, traces may embed the minimal validated
> scenario configuration, including fixture type, retry policy, invariants, and
> synthetic numeric parameters such as delta. This is permitted as test
> configuration, not captured HTTP traffic.
>
> Do not serialize arbitrary request bodies, credentials, authentication
> headers, or sensitive user data. For future scenarios involving arbitrary
> payloads, use an external scenario reference with a verified digest instead.

So a trace carries the scenario's declarative configuration and, for the request
itself, only `method`, `path` and a payload digest. It carries no request body
and no request header. The digest is `sha256:<hex>` over the exact bytes the
client would send for that case, computed by the one function that builds those
bytes, so the digest cannot drift from the body it describes.

One request-side value does appear, and it is worth naming precisely:
`expected.state.operations` lists the operation identifiers the fixture applied,
which are the harness's own idempotency keys as the **oracle read them back out
of application state**. They are not captured from the request, and they carry
no caller data -- but the sentence "a trace contains no header value" would be
false, so it is not written here. If a future external fixture echoed something
sensitive into its own state, that would reach a trace; the guard against it is
`TestTraceSchemaIsExactlyThis`, which pins the permitted key set, plus this
paragraph telling the next implementer where to look.

The embedded scenario is only ever the harness's own synthetic configuration: a
built-in fixture mode, a retry policy, a numeric delta, an invariant. When
scenarios gain arbitrary payloads, the trace must reference an external scenario
file by path and digest rather than embed it.

## Trace schema v1

```json
{
  "schema_version": 1,
  "run_id": "run-1f2e...",
  "case_id": "case-0001",
  "recorded_at": "2026-10-15T18:04:11Z",
  "case_timeout_ns": 5000000000,
  "scenario": { "...": "the validated v1 scenario" },
  "scenario_digest": "sha256:4d7a...",
  "request": {
    "method": "POST",
    "path": "/operation",
    "payload_digest": "sha256:9b1c..."
  },
  "fault_decisions": [
    { "seq": 1, "case_id": "case-0001", "attempt": 1,
      "stage": "after_upstream_response", "action": "close_client_connection" }
  ],
  "expected": {
    "verdict": "fail",
    "attempt_count": 2,
    "state": { "case_id": "case-0001", "counter": 2,
               "operations": ["case-0001-key", "case-0001-key"] }
  }
}
```

`attempt_count` is a count, not the result schema's `attempts` array, and is
named differently so the two schemas cannot be confused. `expected.state` is
what the oracle observed; `expected` names the replay's expectation of it.

### What the digests do, and what they do not do

`scenario_digest` covers the embedded scenario and `payload_digest` covers the
request identity, which together bind the case ID, the fixture mode, the retry
policy, the invariant and the delta to the rest of the file. Editing one of them
and leaving the digest alone is refused. Without this, a two-field edit --
`invariant.expected` and `expected.verdict` -- makes a recorded violation replay
as a clean `pass` and exit 0, and a third field makes the tool report a
fabricated exit 1 against an application that did nothing wrong.

They are **not signatures.** A trace is input, trusted as far as a scenario file
is: anyone who rewrites one wholesale, digests included, can make this tool
describe a run that never happened. What replay establishes is that the embedded
configuration, replayed, reaches the recorded outcome.

Two known gaps, both shared with scenario and result parsing: duplicate JSON
keys are accepted with the last winning, so a file can read one way to a human
and execute another; and the digest covers the Go encoding of the scenario, so
the day the scenario schema reaches v2, stored v1 traces are rejected although
the trace format itself did not change.

Parsing is strict in the same way scenario and result parsing are: unknown keys,
trailing content, an unsupported `schema_version`, a missing `run_id` or
`case_id`, a malformed digest, a decision belonging to another case, a decision
for an attempt the retry policy can never reach, an unknown verdict, and an
invalid embedded scenario are all rejected before anything starts. A trace that
cannot be interpreted is infrastructure failure (exit 3), which is where the
PRD's exit-code table puts a corrupted trace.

## Preconditions and postconditions

- **Pre:** a readable trace under `MaxFileBytes`; nothing else. No scenario file,
  no network, no fixed port.
- **Post:** the replayed case's verdict and the four-axis comparison are printed;
  every listener and goroutine the replay started is released.

## Invariants and forbidden behavior

- Replay injects the **recorded decisions**, not the scenario's matching rules.
  Rebuilding the schedule from the rules would re-derive what to inject and so
  could not detect a trace that no longer describes its scenario.
- A trace with no recorded decisions replays with no faults injected, which is
  distinct from "take the faults from the scenario".
- Replay never derives its verdict from the recorded one: it runs the oracle
  again and then compares.
- A comparison mismatch is a harness-reproducibility failure (exit 3), never an
  application invariant failure (exit 1). Exit 1 still means, and only means,
  that the oracle found a violation in the replayed run.
- A trace is written for every executed case, including inconclusive ones; the
  trace records the verdict as observed and claims nothing more. An inconclusive
  case is still compared on replay, and reproduces when it comes out
  inconclusive again. Only a harness error skips the comparison, because that is
  the replay itself failing.
- A case's recorded decisions are complete even when a run injects more faults
  than the whole-run ordered log holds. An empty decision list in a trace means
  no fault was injected, never that the record was truncated -- otherwise a
  trace of a faulted case would replay clean and report that it reproduced.
- A run owns its trace directory. A directory already holding trace files is
  refused unless `--overwrite` is given, and `--out` may not point inside it.

## Exit codes

| Situation | Code |
|---|---|
| Replay equivalent and the oracle passed | 0 |
| Replay equivalent and the oracle found a violation | 1 |
| Missing or malformed `--trace` argument | 2 |
| Corrupted, unreadable or unsupported trace | 3 |
| Replay could not reach the oracle (inconclusive or harness error) | 3 |
| Replay completed but disagreed with the record | 3 |

## Failure cases and non-goals

Handled: corrupted or truncated trace, unknown field, unsupported schema
version, decision set that does not reproduce, recorded verdict that does not
reproduce, recorded state that does not reproduce, digest that does not match
the embedded scenario.

**Not claimed.** Replay reproduces the *injected fault decisions* and the
*oracle outcome* under the built-in fixture. It does not reproduce goroutine
scheduling, port allocation, connection reuse, or wall-clock timing, and the PRD
says so explicitly. A scenario whose verdict depends on an injected delay racing
a client timeout can therefore legitimately replay to a different verdict; that
is a timing-dependent scenario, not a defect in replay, and the mismatch report
says which axis disagreed so an operator can tell the two apart.

Bounds: one trace file per executed case, so at most `--runs` files, itself
capped at 10,000; each file is a scenario (≤256 KiB) plus at most 20 decisions,
and is refused on read past 1 MiB.

Non-goals: replaying a whole run in one command, replaying against an external
fixture, packet-level capture, and editing a trace to explore variations.

Known rough edges, recorded rather than fixed: trace writing happens after the
run, so an interrupted run still writes the traces of the cases that executed,
which for thousands of cases is a noticeable tail; and a write that fails part
way leaves the traces written so far, named in the error rather than cleaned up.
