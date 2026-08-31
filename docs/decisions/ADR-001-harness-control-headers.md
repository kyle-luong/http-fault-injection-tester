# ADR-001: The client names its own case and attempt

**Status:** accepted, M1
**Affects:** the proxy/client wire contract, and therefore trace replay (M4)

## Context

The PRD requires fault decisions to be keyed by
`(case_id, logical operation, attempt number, stage)` rather than by arrival
order, so that a schedule means the same thing on every run. The proxy is the
component that injects faults, but a transparent HTTP proxy has no way to know
which case a request belongs to or which attempt it is.

Three options were available:

1. **The proxy counts attempts per case.** Avoids new headers, but diverges from
   the client's view the moment an attempt fails before reaching the proxy. The
   client's count is the authoritative one.
2. **The proxy parses the request body for `case_id`.** Couples the proxy to the
   fixture's payload schema, which would break the first time a different local
   fixture is supported.
3. **The client declares both in headers.** The client already knows both
   values with certainty.

## Decision

The test client sets two control headers on every attempt:

```
X-Faulttest-Case-Id: case-0001
X-Faulttest-Attempt: 1
```

The proxy requires both, rejects a request missing or malforming either with
`400`, and strips both before forwarding so the application under test never
sees them. Constants live in `internal/proxy` (`HeaderCaseID`, `HeaderAttempt`).

The idempotency key doubles as the stable logical operation ID. The PRD mentions
a "stable client-side logical operation ID" without defining a field for it, and
the fixture contract already carries `Idempotency-Key`; one identifier is enough
for v1 and avoids a second field that would always hold the same value.

## Consequences

- Fault selection never depends on wall-clock or goroutine ordering, which is
  what makes M4's replay meaningful.
- The proxy is not a general-purpose reverse proxy: it refuses traffic that does
  not carry harness headers. That is deliberate — it cannot be pointed at
  unrelated traffic by accident.
- A trace recorded in M4 must record the case ID and attempt number, not raw
  request bytes, to reproduce a decision.
- If a future fixture needs more than one logical operation per case, the
  operation identity will need its own header rather than reusing the
  idempotency key. That is a v2 schema change, not a silent extension.
