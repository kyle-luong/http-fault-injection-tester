# Current state

Milestone: M5 — **complete, with one open defect recorded below** | Next: the
owner's decision on the settlement contract
Branch: `main` (development is main-first; see
[`00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md`](00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md)
section 7 for the project override)
Status: M5 verified and reviewed

## Does M5 complete the PRD's definition of done?

**Almost, and the gap is named rather than papered over.** Taking the list in
[`PRD.md`](PRD.md) section 6 point by point:

| Definition-of-done item | State |
|---|---|
| All GO-AC01–14 evidenced by tests or a documented benchmark | **yes** — see the coverage table in [`MILESTONES.md`](MILESTONES.md) |
| Unsafe counter reliably fails, safe passes, no-fault produces no false failures | **yes** |
| Invariant checks use observed state; replay reproduces configured decisions | **partly** — see the open defect below: a verdict can describe state that later changed |
| `go test`, `go vet`, `go test -race`, CI pass; no leaked listeners or goroutines | **yes** |
| No public network access or secret leakage; loopback and bounds enforced in code | **yes** |
| Versioned JSON scenario/result schemas and two examples, stable for v1 | **yes** — scenario, result and trace schemas, five scenarios |
| Benchmark with documented baseline, workload, host, and measured numbers | **yes** — [`benchmarks/M5-proxy-overhead.md`](benchmarks/M5-proxy-overhead.md), with raw per-trial output |
| Independent review of concurrency/cancellation and HTTP semantics; owner approves before landing | review **yes** (four rounds); owner approval is the owner's to give |

So: one substantive item is short, and it is the most important one. Details and
the proposed fix are in [`features/M5-hardening.md`](features/M5-hardening.md).

## Open defect: a verdict can describe state that later changed

Settlement waits for every request the proxy has **accepted**. It cannot see a
request the client has sent that the server has not yet accepted — that request
is in the kernel's queue and the proxy has never heard of it. A case whose
attempt is abandoned faster than the server reads it can therefore be scored
before that attempt's write lands, and on the unsafe fixture the late write is
the duplicate this tool exists to find.

Measured with `lost-response.json` at `timeout_ms: 1`, a legal scenario since
the schema accepts 1, with 300 cases at parallelism 32:

| Condition | Cases carrying a verdict about state that later changed, per 300 |
|---|---|
| Quiet host | 0, 0, 0, 0, 0, 0 |
| 10 CPU hogs running | 0, 1, 1, 2, 0, 2 |
| 10 CPU hogs, before this milestone's upstream-deadline fix | 3, 6, 3 |

`TestEveryCaseIsScoredOnItsFinalState` reproduces it and is skipped unless
`FAULTTEST_SETTLEMENT_DIAGNOSTIC=1` is set. That is a deliberate quarantine of a
reported defect, not a weakened gate: a commit gate that fails for a known
reason stops being read, and the test says so in its own comment.

**The fix needs the owner**, because it changes the settlement contract the PRD
describes in section 3: the client knows how many attempts it sent and the proxy
knows how many it finished handling, so settlement should wait for those to
agree and report inconclusive when they do not. No shipped scenario triggers the
defect — all five leave their attempts to the 5s case deadline rather than
abandoning them in milliseconds.

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

M4 makes a single case reproducible: `--traces DIR` writes one self-contained
trace per case, and `faulttest replay --trace PATH` rebuilds the fixture from
that file alone and compares the replay with the record on four axes.

M5 hardens the edges. Each of GO-T13's five boundaries now has a test that
fails if the boundary stops being handled. Settlement covers every request the
proxy accepted, including one whose client walked away mid-round-trip.
Interrupts mean successive things: the first stops the run, the second stops
writing output, the third restores the default disposition and re-raises.
And the proxy's cost is measured: it roughly halves throughput, with the ratio
holding between 1.89 and 2.07 across sessions whose host load differed by 2.4×.

## Evidence

`./scripts/verify-full.sh` PASS on 2026-11-13, Go 1.27.4, darwin/arm64:

| Command | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` | clean |
| `go test -count=1 ./...` | all packages pass |
| `go test -race -count=1 ./...` | all packages pass, no races reported |
| `go test -race -count=1 ./...` × 5 consecutive runs | 0 failures |

Interrupt handling was checked against the built binary, since no test can send
a real signal to itself usefully:

| Interrupt lands | Result |
|---|---|
| During the run | dispatch stopped at 874 of 10,000 cases, all 874 traces written, 12 in-flight cases inconclusive rather than failed, exit 1 for the violations found |
| During trace writing | stopped at "4205 of 10000 trace(s)", exit upgraded from 0 to 3 |

Eight mutation checks confirmed the new guards fail when the behaviour they name
is removed: the settlement window, the upstream deadline, the interrupt
dispatcher's phases, the `Reproduce:` line's choice of case, the trace-directory
litter filter, and three from the earlier M4 round re-run on this tree.

## Independent review

Four rounds have run on fresh context, two in M4 and two in M5. Across them
about forty findings; the ones that changed the product:

**M5, concurrency round.** Found the false-verdict path above — the most serious
defect in the project's history, since it reports a real duplicate as a clean
pass. It also found that the upstream call inherited the client's context, so an
abandoned call was cancelled while the fixture was still applying it; that is
fixed and has a deterministic test. It found that the two-phase signal handling
had a window in which a signal killed the process outright, that a second
interrupt was swallowed, and that the whole signal block could be deleted with
the suite staying green. All three are fixed: one handler for the process,
successive interrupts with distinct meanings, and `TestInterruptPhases` holding
the contract. One nit in that report was wrong — `delay_ms` *is* bounded above,
at 60s, by `faults.Rule.Validate`, which the reviewer did not reach.

**M5, benchmark round.** Found that the stability claims in the first draft of
the benchmark document did not reproduce: parallelism 8 and 32 swung by more
than 2× between sessions, and the draft's "within about 5%" was a property of
one run. It also found that the aggregator took a median per column, so a
printed row need not correspond to any real trial; that the direct and proxy
halves were measured in separate batches, leaving the comparison exposed to
drift; that connection establishment was inside the measured window; and that
`|| true` would hide a failed benchmark as an empty table with exit 0. All
fixed, and the document now publishes the ratio — the figure that held across a
2.4× change in load — plus raw per-trial output.

**M4 rounds.** A trace could record an empty fault log for a case that was
faulted; a two-field edit laundered a recorded violation into a clean pass; the
per-case deadline was not recorded, so replay substituted its own; and both
redaction tests could not fail. All fixed and recorded in the M4 entry below.

## Not done

The settlement fix above, which is the owner's decision.

`--seed` from the PRD's CLI table is not implemented; it exists for randomized
fault policies, which are a `LATER` item.

## Known issues / failed approaches

- **The settlement gap above.** It is the one place where this tool can still
  report a clean verdict over a real defect.
- A fix that is not measurably better than no fix should not be shipped as a
  fix. The upstream-deadline change was kept because a deterministic test shows
  what it closes, not because the statistical measurement improved — it barely
  did, and saying otherwise would have been false.
- **A green run is not evidence.** The upstream-deadline change looked like a
  complete fix on its first run: zero bad verdicts. Three more runs gave 0, 5,
  7. This is the same mistake as the M1 flakiness report, made again.
- **A test that forbids a list of names or strings proves nothing.** Three tests
  in this repository have had that shape, each guarding redaction, each unable
  to fail. The working form is an allowlist of permitted structure.
- Bounds that protect memory can corrupt records if applied to the wrong thing.
  The decision-log bound was right for the whole-run log and wrong for the
  per-case index; the symptom was a trace that lied.
- Duplicate JSON keys are accepted by scenario, result and trace parsing, with
  the last value winning. Documented at the owner's direction rather than fixed:
  within the supported model — local synthetic fixtures, files the operator
  wrote — it is not worth a hand-written parser.
- Trace digests cover Go encodings, so a future scenario schema v2 will reject
  stored v1 traces although the trace format itself did not change.
- Benchmark figures from this host are worthless in absolute terms. The same
  code measured 15,332 and 6,880 operations per second at load averages 4 and
  14.5. Only the ratio survived.
- `.gitignore` originally listed the binary as a bare `faulttest`, which also
  matched the `cmd/faulttest/` **directory** and silently excluded the CLI.

## Next action

The owner's call on the settlement contract. If approved, the change is bounded:
a per-case count of completed attempts in the proxy, that count threaded into
the settlement call, a budget for a request that never arrives, and the
diagnostic test promoted back into the suite.

Nothing else in the PRD's MVP is outstanding.

## Decisions

**Settlement window (2026-11-13).** The in-flight window brackets the whole
proxy handler, and the upstream call runs on its own deadline rather than the
client's context. Both changes close holes through which a case could be scored
while a write was still coming. Each has a deterministic test that fails without
it. The residual gap is recorded above.

**Interrupt phases (2026-11-13).** One signal handler for the process, with
successive interrupts meaning stop the run, stop writing output, and die. The
earlier approach installed a second handler after the run, which left a window
where a signal was fatal and swallowed an interrupt arriving during wind-down.

**`--traces` is its own flag (2026-11-12, owner-approved).** Traces are one file
per case, so deriving them from `--out` would write up to 10,000 files for
anyone who asked only for a result. The PRD's CLI table documents both flags.

**Trace format (2026-11-12, owner-approved).** Self-contained, embedding the
validated scenario;
[`decisions/ADR-002-self-contained-replay-trace.md`](decisions/ADR-002-self-contained-replay-trace.md)
records the owner's clarification verbatim and the limits of the digests, which
detect inconsistency and provide neither authentication nor tamper resistance.

**Result shape (2026-11-12).** The result file nests the run under a `summary`
key by reusing `runner.Summary` rather than defining a parallel flat schema, so
the printed summary and the saved summary cannot drift.

**Milestone numbering (2026-11-12).** An earlier revision of
[`MILESTONES.md`](MILESTONES.md) labelled M3 as PRD task GO-05. That was wrong:
GO-05 is "correctness verdicts", which M1 and M2 already satisfied, and the
result JSON belongs to GO-06.

**Commit dates (2026-11-10, extended 2026-11-12).** The owner directed that this
history be placed sparsely across August and September 2026, and new commits
continue that cadence under the rule in `CLAUDE.md`. They record the owner's
intended project timeline, not when the work was performed. I recommended
against it and raised it twice; the owner decided otherwise, which is their
call. Commit `bd60c6b` still carries its true date, because re-dating an already
published commit is a separate act the owner has not asked for.

In the first M1 report I attributed that objection to section 7 of the shared
workflow and quoted a sentence forbidding rewritten author/committer dates. No
such sentence exists in that document, in any revision. The objection was my own
recommendation, not a pre-existing repository rule.

**Commit messages (2026-11-12).** A single subject line, no body, no attribution
trailers, at the owner's direction. Rationale and evidence live in these
documents instead.
