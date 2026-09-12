# Product Requirements Document 02 — HTTP Fault Injection & Testing Framework

**Language/platform:** Go (current stable supported release; pin exact Go version in `go.mod` and CI)  
**Type:** Local-first, automated HTTP reliability testing tool  
**Status:** Implementation-ready specification; no implementation assumed  
**Estimated solo MVP:** 2–4 focused weeks with agent assistance, plus hardening  
**Priority:** Broad SWE relevance, networking, concurrency, state correctness, scalable reproducible testing  
**Shared agent workflow:** [`00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md`](00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md)

## 1. Product brief

**One-sentence pitch:** Automatically test how an HTTP service behaves when requests succeed on the server but fail in transit, exposing unsafe retries, duplicate side effects, and other ambiguous-completion bugs.

This is **not** another generic reverse proxy, HTTP load generator, or chaos dashboard. The user provides (or uses an included) stateful test application and a declarative failure scenario. The framework launches isolated concurrent test cases, injects controlled faults, **checks final application state**, and saves enough information to replay the faulty interaction.

**Primary users:** Backend developers testing create/update endpoints; distributed-systems and infrastructure engineers; CI developers; anyone learning why retry/idempotency behavior is hard. Run only against **local development/test services under the user's control** during MVP. No production service fuzzing.

**Demonstration:** A test service increments a counter for `POST /operation`. The proxy forwards the first POST, obtains the successful server response, then closes the client connection without forwarding it. The client retries the POST. An unsafe server increments twice; a server enforcing idempotency keys increments once. The tool reports the difference and reproduces the failure on demand.

### Success criteria

- A fresh checkout runs a built-in end-to-end demo with a single documented CLI command.
- HTTP/1.1 proxy supports explicitly scheduled request/response faults at observable stages.
- The test runner can execute independent cases concurrently with bounded worker count and proper cancellation.
- A state oracle, not the client's HTTP status alone, determines whether an operation was duplicated or invariants failed.
- A saved trace specifies the sequence of **injected decisions** and request attempts; replay reconstructs controlled scenarios. Explicitly do not claim to deterministically reproduce arbitrary Go goroutine scheduling or external network timing.
- The reference unsafe test service fails the retry/idempotency test, while the safe one passes, with no false positive in an ordinary no-fault run.
- `go test`, `go vet`, `go test -race`, and local HTTP integration checks pass without leaked goroutines or hung servers.
- Results report meaningful performance numbers (cases/sec, suite duration, added proxy latency under no-fault conditions) using documented workload and hardware, not arbitrary 99% assertions.

### Scope and non-goals

**MVP includes:** local HTTP/1.1 proxy; integrated test client; built-in stateful unsafe and safe fixtures; declarative scenario JSON; deterministic fault selection by case/attempt/stage; bounded concurrent case execution; state reset/isolation; state-oracle checks; trace and JSON result export; replay of one case; integration/race tests; simple no-fault benchmark.

**MVP explicitly excludes:** HTTPS MITM, HTTP/2 and HTTP/3 protocol support, TLS certificate manipulation, production targeting, Docker/Kubernetes, distributed runner fleet, arbitrary API schema discovery, a universal oracle for any service, rate-limit bypass, load attacks, UI, complex proxy scripting, automated network packet manipulation, or externally hosted model APIs. Built-in fixture endpoints are for testing only.

## 2. Personas, workflow, and CLI

Binary name: **`faulttest`**. Run/summary/replay without extra services to deploy.

```console
$ faulttest run --scenario scenarios/lost-response.json --runs 500 --parallel 16 --out results/run.json
Scenario: lost-response | Fixture: unsafe-counter
Cases: 500  Passed: 0  Failed: 500  Infrastructure errors: 0
Violation: expected counter=1; observed counter=2 after retry
Reproduce: faulttest replay --trace results/traces/case-0001.json

$ faulttest run --scenario scenarios/lost-response-safe.json --runs 500 --parallel 16
Cases: 500  Passed: 500  Failed: 0  Infrastructure errors: 0

$ faulttest replay --trace results/traces/case-0001.json
```

Example *illustrative*, not measured output. Test exit codes: `0` if all case invariants pass, `1` if at least one invariant fails, `2` for invalid input/config, `3` for test infrastructure failure (fixture failed, could not set up socket, corrupted trace). The tool must **distinguish an application-under-test failure from a test harness failure**.

| CLI flag | Meaning | Default / bounds |
|---|---|---|
| `run --scenario PATH` | JSON scenario contract | Required |
| `--runs N` | Independent test cases, each with its own ID and isolated state | Default 1; max 10,000 initially |
| `--parallel N` | Maximum simultaneous cases, not unbounded goroutines | Default 1; max 64 for built-in fixture initially |
| `--timeout DURATION` | Overall per-case deadline | Default 5s; explicit upper bound 60s |
| `--out PATH` | Structured result JSON and optional trace files | Optional; refuse overwrite unless `--overwrite` |
| `--seed UINT64` | Derive stable case-specific seeds when randomized fault policies are added | Default explicit stable seed; not a claim of OS scheduling determinism |
| `replay --trace PATH` | Re-run recorded test using same injected fault decisions and request data | Required trace |
| `report PATH` | Summarize saved result without re-execution | Optional convenience, after `run` exists |

### Initial fixture and oracle contract

The built-in fixture is a tiny local HTTP/1.1 application with two modes (`unsafe-counter`, `safe-counter`) and three endpoints:

```
POST /__test/reset        {"case_id":"case-0001"} -> 204
POST /operation           {"case_id":"case-0001","delta":1}
                         header: Idempotency-Key: case-0001-key
GET  /__test/state?case_id=case-0001 -> {"counter": 1, "operations": ["..."]}
```

- The **unsafe** fixture increments on every successful POST, regardless of idempotency key.
- The **safe** fixture deduplicates by `(case_id, idempotency_key)` and returns the original result on repeat without incrementing again.
- State is isolated by case ID. `reset` is only reachable on a loopback-bound test fixture; no sensitive data.
- Each operation has a stable client-side logical operation ID; retries must reuse the same idempotency key and body.
- State oracle is invoked *after the last client attempt and after the known response-processing boundary*; tests wait for in-flight upstream work to settle within a timeout. A read before the backend finishes is inconclusive, not a bug.
- To support a developer's external **local** test app later, MVP may allow overriding the base URL **only when it implements this explicit fixture/oracle contract**. A plug-in oracle for arbitrary APIs is LATER.

### Example scenario JSON v1

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
    {
      "attempt": 1,
      "stage": "after_upstream_response",
      "action": "close_client_connection"
    }
  ],
  "invariant": {
    "type": "counter_equals",
    "expected": 1
  }
}
```

All enum/string values must be validated before running; unknown keys should be rejected for the v1 schema to catch mistakes. JSON results include `schema_version`, run ID, case ID, scenario name, attempted calls, fault-decision trace, observed final state, pass/fail/inconclusive classification, error details, and timings. Omit secret-bearing payloads/headers from logs by default; fixture uses non-sensitive data.

## 3. Architecture and component interfaces

```
scenario JSON → validator → runner (bounded worker pool)
                                 │ case ID + scoped context
                     ┌───────────┼────────────┐
                     ↓           ↓            ↓
                test client → fault proxy → test fixture
                     │           │            │
                     └───────────┴── oracle ←──┘
                                 ↓
                         verdict + trace
                                 ↓
                         JSON + CLI summary
```

**Packages (small, idiomatic):**

```
cmd/faulttest/main.go           subcommands / flags / exit codes
internal/scenario/schema.go     strict JSON parsing, validation
internal/runner/runner.go       cases, worker pool, context cancellation
internal/proxy/proxy.go         HTTP/1.1 forwarding and fault stages
internal/faults/scheduler.go    rule matcher / injection decision log
internal/fixture/server.go      unsafe/safe fixture and state oracle
internal/verify/oracle.go       expected state / invariants
internal/report/report.go       structured result and replay trace
internal/integration/           end-to-end tests
scenarios/                      checked-in safe example scenarios
scripts/verify.sh, verify-full.sh
```

Avoid interface proliferation. Introduce an interface only where two real implementations or test doubles are needed (e.g., state oracle for built-in versus local external fixture). Keep request/attempt metadata typed, not `map[string]interface{}` throughout the codebase.

### Fault stage semantics (must be precise)

| Stage | Action | What it actually simulates |
|---|---|---|
| `before_forward` | `reject_connection` | The backend did not receive the request; useful control case |
| `before_forward` | `delay_ms` | Simulated request network delay; may trigger client deadline before forwarding |
| `after_upstream_response` | `close_client_connection` | Upstream produced a complete response; client gets transport error / no response |
| `after_upstream_response` | `delay_ms` | Slow response after backend processing, potentially exceeding client timeout |

**MVP only these actions.** HTTP/1.1 is mandatory for hard client connection termination; use `http.Hijacker` or equivalent tested connection control on the local proxy, and disable HTTP/2 for the test path. Do not pretend an HTTP handler can close an arbitrary connection using `ResponseWriter` alone. Require non-streaming bounded bodies (for instance, max 64 KiB) to simplify replay; reject oversized input explicitly. The proxy is not responsible for retrying requests—the **test client** controls retries according to scenario policy.

**Network failure classification:** after a complete 2xx upstream response is read, the proxy knows the upstream responded successfully but **cannot infer that durable business state committed**. The oracle confirms final state. If the upstream timed out while processing or the oracle is unavailable, classify as `inconclusive`/`harness_error` rather than claiming duplication.

**Deterministic fault schedule:** match rules by `(case_id, logical operation, attempt number, stage)` rather than the wall-clock race order. A replay trace records each decision and expected input (e.g., method, path, stable payload digest). The same trace must produce the same chosen injected faults in a controlled local fixture. Server thread scheduling, port allocation, wall clock timing, and arbitrary external responses are **not** fully deterministic.

### Concurrency rules

- Runner uses a fixed-size worker pool with work queue and cancellation via `context.Context`.
- Each case owns its fixture state namespace, client request IDs, metrics, and trace; no shared mutable counters without a lock or atomic.
- Servers bind `127.0.0.1` with ephemeral ports, and shutdown closes listeners, idle connections, worker queues, and pending contexts. Integration tests must detect leaks/timeouts.
- Bound `http.Transport` connections, response body size, per-case timers, and global concurrency. `defer response.Body.Close()` where applicable; always drain/close responses appropriately for connection reuse.
- Proxy no-fault mode must preserve status codes, safe request headers, and bodies for local fixture traffic, excluding hop-by-hop headers correctly.
- Ensure custom fault behavior cannot accidentally leak traffic to a non-loopback endpoint during MVP.

## 4. Test acceptance matrix

| ID | Test / invariant | Pass condition |
|---|---|---|
| GO-AC01 | Fixture no-fault baseline | Both fixture modes process one request once; no spurious duplicate verdict |
| GO-AC02 | Lost response, unsafe fixture | First attempt processed, response withheld, retry duplicates effect; oracle reports 2 not 1 |
| GO-AC03 | Lost response, safe fixture | Identical scenario yields exactly one logical operation by idempotency key |
| GO-AC04 | Failure before forwarding | Backend not called on first attempt; client retry yields single effect |
| GO-AC05 | Delayed response with timeout | Test client times out as configured; final verdict determined by oracle after settlement |
| GO-AC06 | Fault scheduling | Same trace injects same logical stage/attempt decisions in controlled fixture |
| GO-AC07 | Concurrency isolation | 1, 8, 32 parallel cases have independent state and correct per-case outcomes |
| GO-AC08 | Cancellation/timeout | Hung fixture or slow response respects deadline, cleans up without leaked goroutines |
| GO-AC09 | Output/schema | Strict schema/version validation; stable report fields; redaction of sensitive headers |
| GO-AC10 | Verdict separation | Target invariant failure ≠ fixture setup/transport infrastructure error; distinct exit status |
| GO-AC11 | Bounds/security | Reject invalid targets, oversized bodies, unsupported HTTP protocol, malformed scenario, too-high concurrency |
| GO-AC12 | Engineering quality | `go test ./...`, `go test -race ./...`, `go vet ./...` pass; no known flaky tests |
| GO-AC13 | Performance evidence | No-fault proxy latency/throughput compared against direct calls, with same local workload and environment |
| GO-AC14 | Demo | README walks through unsafe failure and safe behavior using one command each |

### Detailed automated test matrix

| Test ID | Type | Scenario |
|---|---|---|
| GO-T01 | Unit | Reject unknown schema fields, invalid fault stages, impossible attempt rules |
| GO-T02 | Integration | No fault: direct and proxied requests agree on final state |
| GO-T03 | Integration | Close client connection only after complete upstream response; oracle reveals unsafe duplicate |
| GO-T04 | Integration | Same failure against idempotent fixture produces no duplicate |
| GO-T05 | Integration | Drop before upstream: no upstream effect on failed attempt |
| GO-T06 | Integration | Delay after response > client timeout; correct settlement classification |
| GO-T07 | Unit | Schedule by attempt/stage yields stable decision trace |
| GO-T08 | Integration | Trace replay same injected fault decision and expected oracle outcome |
| GO-T09 | Integration | 100 independent cases at parallelism 1 and 16; no shared-state contamination |
| GO-T10 | Integration | Cancel mid-run, all subprocesses/listeners close promptly |
| GO-T11 | Unit | Invariant failure, harness error, timeout, malformed config have different result codes |
| GO-T12 | Race | Run concurrent fixture and proxy under `go test -race`; no reported race on exercised paths |
| GO-T13 | Boundaries | Empty response, server 500, reset failure, oversized body, slow/absent oracle |
| GO-T14 | CLI | Valid `run` and `replay` commands, meaningful output; no secrets in logs |
| GO-T15 | Benchmark | Compare no-fault proxy versus direct local fixture with identical payload/parallelism; publish raw results |

**Benchmarking:** execute representative local fixture workloads at parallelism 1, 8, and 32. Record wall time, cases/sec, completed operation rate, mean/p50/p95 added proxy latency, memory/goroutine counts, and test environment; p99 only with adequate samples and caveats. Repeat trials and show distribution rather than one cherry-picked number. No benchmark thresholds in CI; perf measurements from ordinary non-race builds only.

**Numerical targets are planning hints, not resume claims:** 500–2,000 case executions can demonstrate orchestration, but a verified unsafe-retry failure and a safe-idempotency pass are more important than thousands of repetitive runs. Never fabricate detected real-world defects; distinguish intentionally seeded fixture bugs from discoveries in third-party services.

## 5. Security boundaries

This tool can intentionally damage request semantics. Default `run` must start locally controlled fixture servers and only bind/connect to loopback interfaces. It does not target public networks in MVP. No credentials, payment APIs, external services, or live production endpoints are required. Don't store private headers or request bodies in traces. Set explicit total test timeouts. Limit concurrent sockets, response-body sizes, and output logs to avoid disk/resource exhaustion. If external local service support is added, require a flag specifying the test oracle and a warning that it can produce duplicate side effects.

**No automatic authorization to test third-party APIs.** A future `--allow-remote` must require user approval and documented safety restrictions; it is outside MVP. The CLI must refuse non-loopback URLs by default rather than merely warn.

## 6. Milestones and issue-ready implementation sequence

| Phase/task | Depends on | Deliverable | Acceptance |
|---|---|---|---|
| **M0 GO-00 — repository** | None | Go module, formatter, vet/test script, CI, PRD/STATUS/CLAUDE, sample CLI | Clean `./scripts/verify.sh` |
| **M1 GO-01 — fixture and oracle** | GO-00 | Safe/unsafe counter, reset/state endpoints, isolated case IDs | GO-AC01, GO-T02 |
| **M1 GO-02 — client and proxy** | GO-01 | HTTP/1.1 forwarding, transport config, response preservation | No-fault baseline, GO-AC11 |
| **M1 GO-03 — fault stages** | GO-02 | Before-forward rejection/delay; after-response drop/delay | GO-AC02–05, GO-T03–06 |
| **M2 GO-04 — scenario runner** | GO-03 | Strict JSON schema, per-case deadlines, bounded worker pool | GO-AC07/10/11, GO-T01/09/10 |
| **M2 GO-05 — correctness verdicts** | GO-04 | Oracle validation, distinct outcomes, safe/unsafe demos | GO-AC03/10/14 |
| **M2 GO-06 — deterministic trace/replay** | GO-05 | Record injected decisions, replay one case, redact data | GO-AC06/09, GO-T07/08/14 |
| **M3 GO-07 — hardening/benchmarks** | GO-06 | Race suite, timeouts/limits, direct-vs-proxy comparison, README | GO-AC08/12/13, GO-T11–15 |

**Task decomposition:** if GO-03 becomes difficult, split by `before_forward` and `after_upstream_response`; do not implement new fault types until both pass integration tests. `GO-06` does **not** need to emulate an entire network packet trace. Do not build generic plugin APIs or complex DSLs under M2.

### Definition of done

- All GO-AC01–14 are evidenced through automated tests or a documented benchmark/demo.
- Built-in unsafe counter reliably fails the intended invariant; safe counter passes; no-fault fixtures do not produce false failures.
- Invariant checks use *observed state*, not client status alone. Trace replay reproduces configured injected decisions under stable fixture conditions.
- `go test`, `go vet`, `go test -race` and CI pass; no hanging listener/goroutine leaks in controlled test suite.
- No unexpected public network access or secret leakage; loopback-only and resource bounds are enforced by code.
- Versioned JSON scenario/results and two examples are documented, validated, and stable for v1.
- Performance benchmark has documented baseline/workload/host and measured, not invented, numbers.
- Independent review checks concurrency/cancellation and HTTP connection semantics; the owner approves before the work lands on `main` (see the project override in `00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md` section 7).

## 7. LATER extensions

Additional local-fixture adapters; manual target oracles; fault probability distributions using seeded pseudo-random choices; response corruption/truncation; HTTP/2 support; explicit TLS test-certificate handling; integration with other test services; GitHub Actions test report artifact; per-fault coverage analysis; protocol-neutral TCP fault bridge. Adopt only after MVP proves useful. A generic chaos-engineering platform is expressly not the goal.

## 8. First Claude Code task and implementation prompt

> Read `docs/PRD.md` and `docs/STATUS.md`, inspect the repository, and implement GO-00 only: minimal Go project, CLI placeholder, sample scenario fixture, `go test`/`go vet` verification script, GitHub Actions, and clear README. Do not implement an arbitrary network proxy yet. Pass `./scripts/verify.sh`, record exact commands, and produce a bounded plan for GO-01. Never test public endpoints or infer backend state from response status alone.

**Implementation references:** Go standard library `net/http`, `httputil`, `http.Transport`, `http.Hijacker`, `context`, `sync`, `testing`, `go test -race`. Consult official Go docs while implementing tricky HTTP/1.1 connection behavior. APIs and fault semantics in this PRD are intentionally more important than selecting external libraries.
