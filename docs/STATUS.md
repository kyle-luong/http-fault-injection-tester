# Current state

Milestone: M4 — **complete** | Next: M5 (resource-limit hardening and the
direct-versus-proxy benchmark)
Branch: `main` (development is main-first; see
[`00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md`](00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md)
section 7 for the project override)
Status: M4 verified and reviewed

## Done

M1 delivers the lost-response path end to end: the same scenario file produces
opposite verdicts against the two fixture modes, and the difference is visible
only in application state.

```console
$ ./faulttest run --scenario scenarios/lost-response.json      # unsafe:  counter=2, exit 1
$ ./faulttest run --scenario scenarios/lost-response-safe.json # safe:    counter=1, exit 0
```

M2 adds concurrent execution: `--runs N` independent cases, each with its own
identifier, isolated state and deadline, and `--parallel N` as a hard ceiling
enforced by a fixed worker pool.

M3 adds the durable record: `--out PATH` writes the structured result JSON, and
`report PATH` prints the same summary and exit code from a saved file without
executing anything.

M4 makes a single case reproducible. `--traces DIR` writes one trace file per
case, and `faulttest replay --trace DIR/case-0001.json` rebuilds the fixture
from that file alone, injects the decisions it recorded, and compares the replay
with the record on four axes: injected decisions, attempt count, observed state
and verdict. Format and rationale:
[`decisions/ADR-002-self-contained-replay-trace.md`](decisions/ADR-002-self-contained-replay-trace.md)
and [`features/M4-trace-replay.md`](features/M4-trace-replay.md).

Three properties are load-bearing:

- **Replay injects the recorded decisions, not the scenario's rules.** Deriving
  them again would put both sides of the comparison in the same place, and a
  trace that no longer described its run could never be detected.
- **The deadline is part of the record.** `replay` has no `--timeout`. The
  delayed-response scenario is `inconclusive` under 100ms and `pass` under the
  5s default, so a replay that substituted its own deadline would reach a
  different verdict and then blame reproducibility for it.
- **A mismatch is exit 3, never exit 1.** The tool failing to reproduce itself
  is infrastructure failure. Exit 1 continues to mean only that the oracle found
  a violation in the application under test.

## Evidence

`./scripts/verify-full.sh` PASS on 2026-11-12, Go 1.27.4, darwin/arm64:

| Command | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` | clean |
| `go test -count=1 ./...` | all packages pass |
| `go test -race -count=1 ./...` | all packages pass, no races reported |
| `go test -race -count=1 ./...` × 4 consecutive runs | 0 failures |

Thirteen mutation checks were run against the new guards, each confirming that
the test named for a behaviour fails when that behaviour is removed: the
per-case decision index, the scenario digest, the payload digest, the recorded
deadline (unit and CLI), the trace schema allowlist, the `--out`-inside-traces
rule, the empty-path refusal, the failed-trace-write exit code, the
`recorded_at` requirement, the case-ID path guard, the replay-from-record wiring
and the mismatch exit code.

Throughput was measured, not estimated:
[`benchmarks/M2-throughput.md`](benchmarks/M2-throughput.md). The host was
loaded and the run-to-run spread is threefold, and the document says so instead
of quoting a median as a benchmark.

### Independent review of M4

Two fresh-context reviews ran before the commit, one on the wire format and its
validation, one on the CLI contract and the quality of the new tests. Between
them they raised about twenty findings; fifteen led to changes and five are
recorded as limitations below. The ones that mattered:

1. **A trace could record an empty fault log for a case that was faulted.** The
   per-case decision index stopped growing with the whole-run ordered log at
   10,000 decisions, so in a run of ~5,000 cases with two faults each — inside
   the documented `--runs` bound — thousands of traces claimed no fault was
   injected. Replaying one injected nothing, passed, and printed that it had
   reproduced the run. The per-case index is now kept whole; it is bounded by
   the run size times the stages a scenario can declare, not by traffic.
2. **A two-field edit laundered a recorded violation into a clean pass.**
   Changing `invariant.expected` and the recorded verdict produced a trace that
   parsed, replayed and exited 0 while the duplicate side effect was visible in
   the same output; a third field fabricated an exit 1 against an application
   that did nothing wrong. A `scenario_digest` now binds the embedded scenario
   to the file. It catches a partial edit, which is the realistic accident for a
   file that travels in bug reports; it is not a signature, and the documents
   now say so instead of implying tamper-evidence.
3. **The per-case deadline was not recorded**, so replay silently substituted
   its own default and reported "did not reproduce" for a configuration value
   that had simply been dropped. It is now `case_timeout_ns` in the trace, and
   `replay` has no `--timeout`.
4. **Both redaction tests could not fail.** They denylisted suspicious field
   *names*; adding a `payload` field holding the request body and an `x_api_key`
   field holding a credential kept the whole suite green. The schema is frozen
   for v1 by strict parsing anyway, so the test now pins the permitted key set
   and fails closed on anything new. **This is the third test of that shape in
   this repository** — see the known issues below.
5. **The oversize bound had no working test**: deleting the size check from
   `Load` left the suite green, because a file of NUL bytes fails to parse
   whether or not the bound exists. The test now asserts the reason.
6. **`--out` inside `--traces` destroyed the trace the run's own `Reproduce`
   line pointed at**, with exit 1 and nothing on stderr. A run now owns its
   trace directory: `--out` may not point inside it, a directory already holding
   traces is refused without `--overwrite`, and with `--overwrite` the run warns
   that traces it did not write are still there describing another run.

Also fixed: three documents and the help text claimed a trace holds no header
value while `expected.state.operations` holds the idempotency keys the oracle
read back out of application state — the wording now says what is there and
where it came from; `expected.attempts` was renamed `attempt_count` so it cannot
be confused with the result schema's `attempts` array, and
`expected.observed_state` became `expected.state`; `recorded_at` was effectively
optional; a state naming another case was accepted; the shared JSON writer
called every file a "result"; the trace directory was created before the
scenario was validated; `--traces=` with an empty value was silently ignored;
and four branches with no test at all got one, including `--case-id` with
`--traces`, which also let the planned-case-ID list be deleted outright.

## Not done

M5: resource-limit hardening and the direct-versus-proxy latency and throughput
benchmark (GO-AC13, GO-T11–T15), which needs a quiet host.

`--seed` from the PRD's CLI table is not implemented; it exists for randomized
fault policies, which are a `LATER` item.

## Known issues / failed approaches

- **A test that forbids a list of names or strings proves nothing.** Three
  tests in this repository have now had that shape, each written to guard
  redaction, each unable to fail. The working form is an allowlist of the
  permitted structure, which fails closed when the schema grows.
- A green single-package test run is not evidence of concurrency correctness
  here. The transport-replay defect reproduced only under full-suite load, and
  its failing subtest passed six times out of six in isolation.
- Bounds that protect memory can corrupt records if they are applied to the
  wrong thing. The decision-log bound was correct for the whole-run log and
  wrong for the per-case index, and the symptom was a trace that lied rather
  than a resource problem.
- Duplicate JSON keys are accepted by scenario, result and trace parsing, with
  the last value winning, so a crafted file can read one way to a human and
  execute another. "Strictly parsed" in these documents means unknown keys and
  trailing content, not duplicate keys.
- The trace digests cover Go encodings, so a future scenario schema v2 will
  reject stored v1 traces although the trace format itself did not change.
- An interrupted run still writes the traces of every case that executed, which
  for thousands of cases is a noticeable tail, and a second interrupt is
  swallowed. Pre-existing from M2; `--traces` makes the tail longer.
- Throughput measurement on a loaded host is worthless for absolute figures:
  the same commit measured 822, 2101 and 2459 cases/sec for one configuration.
- `.gitignore` originally listed the binary as a bare `faulttest`, which also
  matched the `cmd/faulttest/` **directory** and silently excluded the CLI.

## Next action

M5 (GO-07): resource-limit tests and the direct-versus-proxy comparison the PRD
asks for in GO-AC13 — added latency, memory and goroutine counts with the proxy
in the path versus without. It needs a quiet host to produce figures worth
publishing, and the method and caveats belong in `docs/benchmarks/` beside the
M2 numbers.

## Decisions

**Trace format (2026-11-12).** Self-contained, embedding the validated
scenario, with the owner's approval and its limits recorded in
[`decisions/ADR-002-self-contained-replay-trace.md`](decisions/ADR-002-self-contained-replay-trace.md).
The owner's clarification is quoted verbatim there: synthetic test configuration
may be embedded, arbitrary payloads and credentials may not, and a future
scenario with an arbitrary payload must switch to an external reference with a
verified digest.

**`--traces` is its own flag (2026-11-12).** The PRD's CLI table gives `--out`
the meaning "structured result JSON and optional trace files". Traces are one
file per case, so deriving them from `--out` would write up to 10,000 files for
anyone who asked only for a result. `--out results/run.json --traces
results/traces` reproduces the PRD's example layout explicitly. The PRD was not
edited to match the code; the divergence is flagged for the owner.

**Result shape (2026-11-12).** The result file nests the run under a `summary`
key by reusing `runner.Summary` rather than defining a parallel flat schema, so
the printed summary and the saved summary cannot drift.

**Milestone numbering (2026-11-12).** An earlier revision of
[`MILESTONES.md`](MILESTONES.md) labelled M3 as PRD task GO-05. That was wrong:
GO-05 is "correctness verdicts", which M1 and M2 already satisfied, and the
result JSON belongs to GO-06. The file now carries an explicit mapping between
the two numbering schemes.

**Commit dates (2026-11-10, extended 2026-11-12).** The owner directed that
this history be placed sparsely across August and September 2026, and the dates
were rewritten accordingly. They record the owner's intended project timeline,
not the wall-clock time at which the work was performed. I had recommended
against it and raised it twice; the owner decided otherwise, which is their
call. On 2026-11-12 the owner extended the instruction to new commits, so
`CLAUDE.md` now carries a cadence rule — 3 to 8 working-hours days after the
previous commit, never later than today — instead of requiring an agent to ask
each time. The M3 commit `bd60c6b` still carries its true date, because
rewriting an already published commit is a separate act the owner has not asked
for.

In the first M1 report I attributed that objection to section 7 of the shared
workflow and quoted a sentence forbidding rewritten author/committer dates. No
such sentence exists in that document, in any revision. The objection was a
recommendation on my own judgement, not a pre-existing repository rule.

**Commit messages (2026-11-12).** Messages are a single subject line, with no
body and no attribution trailers, at the owner's direction. Rationale and
evidence live in these documents instead.
