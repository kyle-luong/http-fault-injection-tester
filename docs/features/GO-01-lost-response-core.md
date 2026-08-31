# M1: reproduce a lost response and detect the duplicate it causes

PRD criteria: GO-AC01, GO-AC02, GO-AC03, GO-AC04, GO-AC05, GO-AC10, GO-AC12,
GO-AC14 (plus partial GO-AC06/07/08/09/11 — see [`../MILESTONES.md`](../MILESTONES.md))

## Behavior

An operator runs one command against a checked-in scenario file. The tool starts
a stateful fixture and a fault-injecting proxy on loopback, drives one logical
operation through the proxy with the scenario's retry policy, injects the
scheduled faults, waits for upstream work to settle, reads the fixture's state,
and prints a verdict derived from that state.

The headline observable: the same scenario yields `fail` against the unsafe
fixture (counter 2) and `pass` against the safe fixture (counter 1).

## Preconditions and postconditions

- **Pre:** a valid v1 scenario file; no listener on a fixed port is required
  (everything binds `127.0.0.1:0`).
- **Post:** every listener, pooled connection, and goroutine started by the run
  is released; the process exit code classifies the outcome.

## Invariants and forbidden behavior

- A verdict is never derived from the client's HTTP status alone.
- A fault at `after_upstream_response` fires only once a complete upstream
  response has been read, so the upstream's work has definitely happened.
- A fault at `before_forward` never contacts the upstream.
- No socket is opened to a non-loopback address, enforced both when parsing the
  upstream URL and in the dialer's `Control` hook.
- An outcome that cannot be determined is `inconclusive` or `harness_error`,
  never `fail`.
- Request bodies and credential-bearing headers never appear in output.

## Failure cases and non-goals

Handled: invalid scenario (exit 2), unreachable fixture (exit 3), per-case
deadline exceeded (inconclusive, exit 3), oversized request body (413),
missing harness headers (400), unreachable upstream (502), operation without a
prior reset (409).

Non-goals for this milestone: `--runs`/`--parallel` (M2), `--out` and `report`
(M3), `replay` (M4), benchmarks (M5), HTTPS/HTTP2, non-loopback targets.

## Acceptance evidence

| PRD ID | Test |
|---|---|
| GO-AC01 | `internal/integration` → `TestNoFaultBaseline` |
| GO-AC02 | `internal/integration` → `TestLostResponseDuplicatesOnUnsafeFixture` |
| GO-AC03 | `internal/integration` → `TestLostResponseAppliesOnceOnSafeFixture` |
| GO-AC04 | `internal/integration` → `TestRejectBeforeForwardLeavesBackendUntouched` |
| GO-AC05 | `internal/integration` → `TestDelayedResponseBeyondClientTimeout` |
| GO-AC10 | `internal/integration` → `TestHarnessFailureIsNotAnInvariantFailure`; `cmd/faulttest` → `TestRunExitCodes` |
| GO-T01 | `internal/scenario` → `TestParseRejectsInvalidScenarios` (27 rejection cases) |
| GO-T02 | `internal/integration` → `TestProxiedAndDirectAgreeOnFinalState` |
| GO-T07 | `internal/faults` → `TestDecideIsDeterministic` |
| GO-T12 | `./scripts/verify-full.sh` → `go test -race ./...` |

Negative coverage is deliberate: the suite asserts that no-fault runs produce no
false failure, that a `before_forward` drop produces no side effect, and that a
harness failure is not reported as an application defect.

## Specification validation

The PRD was checkable but left six details undefined. Each was resolved as
below; none changes a documented guarantee.

1. **Stable logical operation ID has no field.** Resolved: the idempotency key
   is the operation identity for v1. See [ADR-001](../decisions/ADR-001-harness-control-headers.md).
2. **The proxy cannot know the case or attempt number.** Resolved: harness
   control headers, ADR-001.
3. **Behavior for an operation on a case that was never reset is undefined.**
   Resolved: `409`, and for an unknown case on the state endpoint, `404`. Both
   surface a harness mistake as a harness mistake (GO-AC10).
4. **A missing `Idempotency-Key` is undefined.** Resolved: `400` in both fixture
   modes, so the contract does not differ between them.
5. **"Response-processing boundary" has no mechanism.** Resolved: the proxy
   tracks in-flight upstream calls per case and exposes `WaitForQuiescence`.
   Once a complete upstream response has been read, the backend has committed or
   refused, so a state read afterwards cannot race it. A settle timeout yields
   `inconclusive`.
6. **Verdict for a case where no attempt ever succeeded is undefined.**
   Resolved: if no attempt got a response and the counter is below the expected
   value, the result is `inconclusive` — the injected faults prevented the work,
   which is not evidence about the application's retry semantics.

One contradiction remains open and is **not** a code issue: see "Human decision
needed" in [`../STATUS.md`](../STATUS.md).

## Implementation notes

Package layout follows the PRD's section 3 with two additions: `internal/client`
(the retrying test client, which the PRD describes but does not place) and
`internal/leaktest` (a test-only goroutine leak check). `internal/runner`
currently executes one case; its shared state is already concurrency-safe and
race-tested, so M2 adds a worker pool over it rather than restructuring it.
