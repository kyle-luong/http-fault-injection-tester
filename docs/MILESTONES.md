# Milestones

Each milestone is independently testable: it ends with a named verification
command whose result can be inspected without reference to later work. PRD task
IDs (`GO-0x`) and acceptance IDs (`GO-ACxx`, `GO-Txx`) come from
[`PRD.md`](PRD.md) section 6 and section 4.

| Milestone | PRD tasks | Deliverable | Verification | State |
|---|---|---|---|---|
| **M1 — Lost response, end to end** | GO-00, GO-01, GO-02, GO-03 | Repository scaffold; stateful fixture with a state oracle; loopback HTTP/1.1 fault proxy; all four MVP fault actions; strict scenario schema; single-case execution behind `faulttest run` | `./scripts/verify-full.sh` | in progress |
| **M2 — Concurrent runner** | GO-04 | Bounded worker pool, `--runs` and `--parallel`, per-case IDs and deadlines, aggregate summary, cancellation of a whole run | `./scripts/verify-full.sh` plus 100 cases at parallelism 1 and 16 | todo |
| **M3 — Result reporting** | GO-05 | `--out` structured JSON results, `report` subcommand, redaction rules asserted by test | `./scripts/verify-full.sh` plus a schema-stability test | todo |
| **M4 — Trace and replay** | GO-06 | Per-case trace files, `faulttest replay --trace`, stable payload digests | `./scripts/verify-full.sh` plus a record-then-replay equivalence test | todo |
| **M5 — Hardening and benchmarks** | GO-07 | Resource-limit tests, direct-versus-proxy latency and throughput measurements, README demo walkthrough | `./scripts/verify-full.sh` plus `docs/benchmarks/` with measured numbers | todo |

## Why M1 is one milestone and not four

PRD tasks GO-00 through GO-03 cannot each stand alone as *demonstrable* work:
a fixture with no proxy cannot lose a response, and a proxy with no state oracle
cannot tell a duplicate from a success. The smallest slice that proves the
product thesis is the whole lost-response path, so M1 carries all four tasks and
ends with the demonstration the PRD names as its headline evidence.

Everything after M1 is genuinely separable, because each later milestone adds a
layer over a working core rather than a part of it.
