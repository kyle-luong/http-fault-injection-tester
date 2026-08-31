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

Milestone M1 of five is complete: the lost-response path works end to end and is
covered by tests. See [`docs/MILESTONES.md`](docs/MILESTONES.md) for what each
later milestone adds, and [`docs/STATUS.md`](docs/STATUS.md) for current state.

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

## Exit codes

| Code | Meaning |
|---|---|
| 0 | every case invariant passed |
| 1 | an invariant failed — the application under test is wrong |
| 2 | invalid input or configuration |
| 3 | test infrastructure failure, or an outcome that could not be determined |

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
supported as the application under test. No performance numbers are published
yet; measurement is M5, and no figure will appear here that was not measured.

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
internal/integration/ end-to-end tests, named after PRD acceptance IDs
scenarios/            checked-in example scenarios, validated by tests
docs/                 PRD, status, milestones, decisions, task contracts
```
