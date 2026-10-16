# M5: hardening, interruption, and the proxy's cost

PRD criteria: GO-AC08, GO-AC11, GO-AC12, GO-AC13, GO-T11, GO-T12, GO-T13, GO-T15

## Behaviour

Three things, none of which changes the product's interface:

1. **Boundary coverage.** Each of GO-T13's five boundaries now has a test that
   fails if the boundary stops being handled; the mapping is in
   [`../MILESTONES.md`](../MILESTONES.md).
2. **Settlement covers every request the proxy accepted.** The in-flight window
   brackets the whole handler rather than the upstream round trip, and the
   upstream call runs on its own deadline instead of the client's context.
3. **Interrupts mean successive things.** The first stops the run, the second
   stops writing output, the third restores the default disposition and
   re-raises so the process dies.
4. **The proxy's cost is measured**, not asserted:
   [`../benchmarks/M5-proxy-overhead.md`](../benchmarks/M5-proxy-overhead.md).

## Why settlement changed

The oracle may read a case's state only once nothing can still change it. That
boundary used to be "no upstream call for this case is in flight", which was
narrower than the question in two ways:

- A request stalled in an injected delay **before** the upstream call had
  nothing in flight, so a case could be scored while its handler was still
  going to apply the operation. `TestStateIsNotReadWhileTheProxyStillHoldsARequest`
  fails without the wider window.
- An upstream call inherited the **client's** context, so a client that gave up
  mid-round-trip cancelled the call: the handler returned, the count dropped,
  and settlement said yes while the fixture was still applying the operation.
  `TestQuiescenceCoversAnUpstreamCallTheClientAbandoned` fails without the
  independent deadline.

On the unsafe fixture the write that lands late is the duplicate this tool
exists to find, so both holes produced the worst kind of wrong answer: a clean
verdict over a real defect.

## Open defect: a request the proxy has not yet accepted

**Settlement still cannot see a request the client has sent that this server
has not yet accepted.** No accounting inside the proxy can: the request is in
the kernel's queue, and the proxy has never heard of it. A case whose attempt is
abandoned faster than the server reads it can therefore be scored before that
attempt's write lands.

Measured with `lost-response.json` at `timeout_ms: 1` — a legal scenario, since
the schema accepts 1 — 300 cases at parallelism 32, counting cases whose final
state differs from the state they were scored on:

| Condition | Cases carrying a verdict about state that later changed, per 300 |
|---|---|
| Quiet host | 0, 0, 0, 0, 0, 0 |
| 10 CPU hogs running | 0, 1, 1, 2, 0, 2 |
| 10 CPU hogs, before the upstream-deadline fix | 3, 6, 3 |

So the fix above cuts the rate by roughly three times under load and does not
eliminate it. `TestEveryCaseIsScoredOnItsFinalState` reproduces it and is
skipped unless `FAULTTEST_SETTLEMENT_DIAGNOSTIC=1` is set — not to make the
suite green, but because a commit gate that fails for a known, reported reason
stops being read. The test states this in its own comment.

**The fix that would close it, which the owner has to approve**, because it
changes the settlement contract the PRD describes in section 3: the client
knows how many attempts it sent, and the proxy knows how many it finished
handling. Settlement should wait for those to agree, and report inconclusive
when they do not. That turns "a request nobody can see" into "a case the tool
declines to judge", which is the correct answer. It needs: a per-case count of
completed attempts in the proxy, the attempt count threaded into the settlement
call, and a decision about the right budget for a request that never arrives.

Until then the honest description is the one now in the code: settlement is
authoritative for every request the proxy accepted, and silent about one it has
not.

## Invariants and forbidden behavior

- A verdict describes the state the case ended with. Where that cannot be
  guaranteed, the case is inconclusive, never a pass or a fail.
- An abandoned upstream call still completes, bounded by the upstream deadline.
  The request was already sent; whether it committed is the only question that
  matters, and only the upstream can answer it.
- Settlement timing out is inconclusive, never a finding.
- An interrupted run still writes the evidence it collected; a second interrupt
  stops that too; a third kills the process.
- Benchmarks come from ordinary builds, never `-race`, and no threshold derived
  from them runs in CI.

## Failure cases and non-goals

Handled: empty and undecodable responses, an application 500, a reset mid-flight,
oversized bodies, an oracle that never answers, an oracle that is gone, a
trace directory holding another run's files, a leftover temporary file from a
hard kill, an interrupt in any phase.

Non-goals: a whole-run timeout flag (each case is bounded, which is what the PRD
asks for); fsync durability for output files (atomic against process death, not
machine death); and performance work of any kind — nothing in this repository
has been optimised, and the benchmark exists to say what the proxy costs, not to
improve it.
