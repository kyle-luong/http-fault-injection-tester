# faulttest

Automatically test how an HTTP service behaves when a request **succeeds on the
server but fails in transit** — the ambiguous completion that makes retries
unsafe.

The tool runs a stateful application on loopback, forwards traffic through a
proxy that destroys the client connection *after* the server has finished its
work, lets the client retry, and then decides the verdict by reading the
application's actual state. The client's HTTP status is not evidence: a client
that got a transport error has learned nothing about whether its operation
committed.

```
scenario JSON → validator → runner → test client → fault proxy → test fixture
                                          │             │             │
                                          └─────────────┴── oracle ←──┘
                                                              ↓
                                                      verdict + exit code
```

## Status

All five milestones are complete: the lost-response path works end to end,
cases run concurrently under a bounded worker pool, results are saved as
structured JSON, any recorded case can be replayed from its trace, and the
proxy's cost is measured rather than asserted. Every PRD acceptance criterion
(GO-AC01 to GO-AC14) is evidenced by a test or a documented benchmark. See
[`docs/MILESTONES.md`](docs/MILESTONES.md) for what each later milestone adds,
and [`docs/STATUS.md`](docs/STATUS.md) for current state.

## Requirements

Go 1.27.4 or newer (pinned in `go.mod`). No third-party dependencies.

## Build and verify

```console
$ go build ./cmd/faulttest
$ ./scripts/verify.sh        # gofmt, go vet, go build, go test
$ ./scripts/verify-full.sh   # the above plus go test -race
```

## Demo: the same fault, two implementations

A counter service increments on `POST /operation`. The proxy forwards attempt 1,
reads the complete successful response, and then resets the client's connection
without delivering it. The client retries with the same idempotency key.

The **unsafe** fixture applies the operation again, so the single logical
operation happens twice:

```console
$ ./faulttest run --scenario scenarios/lost-response.json
Scenario: lost-response | Fixture: unsafe-counter
Cases: 1  Passed: 0  Failed: 1  Inconclusive/infrastructure: 0
Case case-0001: verdict=fail attempts=2 duration=2ms
  attempt 1: no response (Post "http://127.0.0.1:59325/operation": read tcp ...: connection reset by peer)
  attempt 2: status 200
  injected: attempt=1 stage=after_upstream_response action=close_client_connection
Violation: expected counter=1; observed counter=2 after 2 applied operation(s) [case-0001-key case-0001-key]
$ echo $?
1
```

The **safe** fixture deduplicates by idempotency key, so the identical fault
produces exactly one effect:

```console
$ ./faulttest run --scenario scenarios/lost-response-safe.json
Scenario: lost-response-safe | Fixture: safe-counter
Cases: 1  Passed: 1  Failed: 0  Inconclusive/infrastructure: 0
Case case-0001: verdict=pass attempts=2 duration=2ms
  attempt 1: no response (Post "http://127.0.0.1:59331/operation": read tcp ...: connection reset by peer)
  attempt 2: status 200
  injected: attempt=1 stage=after_upstream_response action=close_client_connection
Result: counter=1 as expected after 1 applied operation(s)
$ echo $?
0
```

Note that in both runs the client's *last* status is `200`. Only the state
oracle distinguishes them.

Other shipped scenarios: `no-fault-baseline.json` (proves no false positive),
`reject-before-forward.json` (a failure the backend never saw), and
`delayed-response.json` (a response stalled past the client's timeout).

## Running many cases

Each case gets its own identifier, its own isolated fixture state and its own
deadline, so cases can run concurrently without contaminating one another.

```console
$ ./faulttest run --scenario scenarios/lost-response.json --runs 500 --parallel 16
Scenario: lost-response | Fixture: unsafe-counter
Cases: 500  Passed: 0  Failed: 500  Inconclusive: 0  Infrastructure: 0
Parallelism: 16  Duration: 210ms  Throughput: 2375.3 cases/sec
```

That throughput line is one real run on a busy laptop; it moves by a factor
of three with host load, so treat it as an illustration of the output rather
than a figure to quote.

`--parallel` is a hard ceiling on cases in flight, not a target: it bounds
goroutines, sockets and load on the fixture together. Passing cases are
counted, not printed; only cases that need explaining produce output, capped so
a large run cannot bury its own summary.

Interrupting a run stops new cases from starting. Cases already in flight end
as inconclusive rather than as findings, because a case that was cut short
proves nothing about the application — but a violation found before the
interrupt is still reported, and still sets exit code 1.

Measured throughput, and the reasons to distrust its absolute values, are in
[`docs/benchmarks/M2-throughput.md`](docs/benchmarks/M2-throughput.md).

## Saving and re-reading results

`--out PATH` writes the run's structured result JSON: the verdict, attempts,
injected fault decisions and observed state for every case, with a schema
version and run ID. `report PATH` prints the same summary and returns the same
exit code from a saved result, executing nothing.

```console
$ ./faulttest run --scenario scenarios/lost-response.json --runs 4 --out results/run.json
$ ./faulttest report results/run.json
Run run-65d921356d6d58da started 2026-11-12T22:12:50Z
Scenario: lost-response | Fixture: unsafe-counter
Cases: 4  Passed: 0  Failed: 4  Inconclusive: 0  Infrastructure: 0
```

The file holds no request bodies and no request headers. The one request-side
value it records is the harness's own idempotency key, which identifies the
logical operation and carries no caller data. An existing path is refused
unless `--overwrite` is given, and an unwritable one is refused before the run
starts rather than after it.

## Recording and replaying one case

`--traces DIR` writes one trace file per case; it is a separate flag from
`--out` because traces are one file per case, and a 10,000-case run should not
produce 10,000 files for someone who asked only for a result. A trace is
self-contained, so replay needs nothing else — no scenario file, no result file:

```console
$ ./faulttest run --scenario scenarios/lost-response.json --runs 4 \
      --out results/run.json --traces results/traces
Scenario: lost-response | Fixture: unsafe-counter
Cases: 4  Passed: 0  Failed: 4  Inconclusive: 0  Infrastructure: 0
...
Traces: results/traces
Reproduce: faulttest replay --trace results/traces/case-0001.json
Results: results/run.json

$ ./faulttest replay --trace results/traces/case-0001.json
Replaying case-0001 from run run-e397c0d17ea64342 recorded 2026-11-12T23:47:08Z
Scenario: lost-response | Fixture: unsafe-counter
Cases: 1  Passed: 0  Failed: 1  Inconclusive: 0  Infrastructure: 0
Case case-0001: verdict=fail attempts=2 duration=2ms
  attempt 1: no response (read: connection reset by peer)
  attempt 2: status 200
  injected: attempt=1 stage=after_upstream_response action=close_client_connection
Violation: expected counter=1; observed counter=2 after 2 applied operation(s) [case-0001-key case-0001-key]
Replay reproduced the recorded decisions, state and verdict (fail)
```

Replay injects the decisions the trace **recorded**, not the faults the scenario
would match. Rebuilding the schedule from the scenario would mean both sides of
the comparison came from the same place, so a trace that no longer describes its
run could never be detected. After the case runs, the replay compares four
things against the record: the injected decisions, the number of attempts, the
observed fixture state, and the verdict.

A trace carries the scenario's own configuration — fixture mode, retry policy,
invariant, the synthetic numeric delta — the deadline the recorded run used, and,
for the request, only its method, path and a `sha256` digest of the payload. It
carries no request body and no request header. The digest is taken over the bytes
the client builds, by the same function that builds them, so it cannot come to
describe something else. The one request-side value present is the list of
operation identifiers the oracle read back out of application state, which are
the harness's own idempotency keys and carry no caller data.

Digests over the embedded scenario and the request identity mean a partly edited
trace is refused instead of replayed: without them, changing `invariant.expected`
and the recorded verdict would make a real violation replay as a clean pass.

They are **consistency checks, not security**: they provide no authentication and
no tamper resistance, and a trace rewritten wholesale with its digests recomputed
is indistinguishable from a genuine one. A trace is input, trusted exactly as far
as a scenario file is. Replay establishes that the embedded configuration reaches
the recorded outcome, not that the record is genuine.

What replay reproduces is the **injected fault decisions and the oracle's
verdict** under the built-in fixture. It does not reproduce goroutine
scheduling, port allocation or wall-clock timing, so a scenario whose verdict
depends on an injected delay racing a client timeout can legitimately replay
differently; the mismatch report names the axis that disagreed. A replay that
does not reproduce its record exits 3, because that is this tool failing to
reproduce itself rather than a finding about the application under test.

[`docs/features/M4-trace-replay.md`](docs/features/M4-trace-replay.md) has the
schema and the full validation rules.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | every case invariant passed |
| 1 | an invariant failed — the application under test is wrong |
| 2 | invalid input or configuration |
| 3 | test infrastructure failure, or an outcome that could not be determined |

A corrupted trace, and a replay that does not reproduce its record, are both 3.

The 1/3 split is load-bearing. A harness that could not reach the oracle has not
found an application bug, and the tool never reports one in that situation.

## Fault stages

| Stage | Action | What it simulates |
|---|---|---|
| `before_forward` | `reject_connection` | The backend never received the request |
| `before_forward` | `delay_ms` | Request-path delay, possibly past the client deadline |
| `after_upstream_response` | `close_client_connection` | The backend completed; the response is lost in transit |
| `after_upstream_response` | `delay_ms` | A slow response after the work was already done |

Faults are selected by `(case ID, attempt number, stage)` — never by timing — so
a schedule means the same thing on every run. That is not a claim that Go
goroutine scheduling, port allocation, or wall-clock timing are reproducible.

## Scenario format

```json
{
  "schema_version": 1,
  "name": "lost-response",
  "fixture": "unsafe-counter",
  "client": {
    "method": "POST",
    "path": "/operation",
    "max_attempts": 2,
    "retry_on_transport_error": true,
    "reuse_idempotency_key": true
  },
  "faults": [
    {"attempt": 1, "stage": "after_upstream_response", "action": "close_client_connection"}
  ],
  "invariant": {"type": "counter_equals", "expected": 1}
}
```

Parsing is strict: unknown keys, unknown enum values, impossible rules (an
attempt beyond `max_attempts`, an action that cannot run at the given stage) and
ambiguous schedules are all rejected before anything starts. A scenario that
silently does nothing would be worse than one that refuses to run.

## Safety boundaries

This tool deliberately breaks HTTP request semantics and **can cause duplicate
side effects**. The MVP therefore:

- binds fixtures and the proxy to `127.0.0.1` on ephemeral ports;
- refuses any upstream that is not a loopback IP or `localhost`, both when
  parsing the URL and again in the dialer's `Control` hook, so no socket to a
  non-loopback address can be opened even if the first check were bypassed;
- bounds request and response bodies (64 KiB), per-case deadlines (60s cap),
  attempt counts, scenario file size, and the fault-decision log;
- never writes request bodies or credential-bearing headers to output.

There is no flag to target a remote service. Doing that safely needs an explicit
authorization design, which is out of MVP scope.

## Limitations

Cleartext HTTP/1.1 only — hard connection control depends on it, and HTTPS
interception, HTTP/2 and HTTP/3 are out of scope. One logical operation per
case. One invariant type (`counter_equals`). Only the built-in fixture is
supported as the application under test. Replay covers one case at a time, and
`--seed` from the PRD's CLI table is unimplemented because the fault policies it
would seed are randomized ones, which are explicitly later work.

Performance figures live in [`docs/benchmarks/`](docs/benchmarks/), with the raw
per-trial output beside them. The proxy's measured cost
([`M5-proxy-overhead.md`](docs/benchmarks/M5-proxy-overhead.md)) is that it
**roughly halves throughput** — the direct-to-proxy ratio held between 1.89 and
2.07 at every parallelism level across sessions whose host load differed by
2.4× — and that allocation per operation roughly doubles. Absolute latency
figures moved by half between those sessions, so the document publishes the
ratio and not a microsecond number. No figure appears anywhere here that was not
measured, and nothing in this repository was optimised for one.

**A known defect to be aware of:** a case can be scored before an attempt's
write lands, if the client abandons that attempt faster than the proxy accepts
it — reachable with a per-attempt `timeout_ms` shorter than a loopback round
trip. Measured at roughly one case in three hundred under CPU load, zero on a
quiet host. Full measurement and the proposed fix:
[`docs/features/M5-hardening.md`](docs/features/M5-hardening.md).

## Layout

```
cmd/faulttest/        CLI, subcommands, exit codes
internal/scenario/    strict v1 JSON parsing and validation
internal/fixture/     the application under test, and its wire contract
internal/proxy/       loopback HTTP/1.1 forwarding with fault stages
internal/faults/      deterministic fault selection and decision log
internal/client/      retry policy and attempt sequencing
internal/verify/      state oracle and verdict classification
internal/runner/      scenario → running infrastructure → case execution
internal/report/      structured result file, written and read back
internal/trace/       per-case replay record and the replay comparison
internal/integration/ end-to-end tests, named after PRD acceptance IDs
scenarios/            checked-in example scenarios, validated by tests
docs/                 PRD, status, milestones, decisions, task contracts
```
