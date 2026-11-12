# Current state

Milestone: M3 — **complete** | Next: M4 (per-case traces and `replay`)
Branch: `main` (development is main-first; see
[`00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md`](00_SHARED_AGENTIC_ENGINEERING_WORKFLOW.md)
section 7 for the project override)
Status: M3 done and pushed

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
enforced by a fixed worker pool. Isolation, reproducibility across parallelism,
and cancellation that never gives a verdict to a cut-short case are each
asserted rather than assumed.

M3 adds the durable record. `--out PATH` writes the structured result JSON —
verdict, attempts, injected fault decisions and observed state for every case,
plus a schema version and run ID — and `report PATH` prints the same summary
and returns the same exit code from a saved file without executing anything.
An existing or unwritable path is refused before the run starts, not after.

Two properties of the result file are load-bearing:

- **It is a published interface.** Parsing is strict, the key names are pinned
  by test, and `proxy.Stats` gained JSON tags so the whole schema is snake_case
  before v1 ships rather than after.
- **A result cannot claim more than it establishes.** Parsing rejects a summary
  whose numbers disagree, because `report` turns a saved result into an exit
  code and a file full of zeros would otherwise read as a pass.

## Evidence

`./scripts/verify-full.sh` PASS on 2026-11-12, Go 1.27.4, darwin/arm64:

| Command | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` | clean |
| `go test -count=1 ./...` | all packages pass |
| `go test -race -count=1 ./...` | all packages pass, no races reported |
| `go test -race -count=1 ./...` × 3 consecutive runs | 0 failures |

Throughput was measured, not estimated:
[`benchmarks/M2-throughput.md`](benchmarks/M2-throughput.md). The host was
loaded and the run-to-run spread is threefold, and the document says so instead
of quoting a median as a benchmark.

### Independent review of M3

A fresh-context review raised nine findings. The four that mattered were
reproduced against a built binary before being fixed:

1. `Parse` checked only the schema version and run ID, so
   `{"schema_version":1,"run_id":"x"}` printed a clean run and **exited 0**. A
   result now has to account for its own cases or it is rejected.
2. `Write` encoded straight into the destination, so a failure part way through
   left a truncated file that then blocked the retry, and two `--overwrite`
   writers could interleave. It now encodes to a temporary file in the same
   directory and moves it into place, using a hard link when overwrite is off
   so the refusal is atomic.
3. The pre-run guard checked existence but not writability, so a run with an
   unwritable `--out` directory executed in full and then failed. It now probes
   the directory.
4. `proxy.Stats` had no JSON tags, emitting `"Requests"` amid an otherwise
   snake_case schema. Left alone it would have frozen into v1, and strict
   parsing would make a later rename reject every result already written.

Also fixed: the end-to-end redaction test could not fail, since none of its
forbidden strings could ever appear; it now asserts on field *names* and so
fails if anything header-, body- or credential-shaped enters the schema. The
help text claimed the file carries no request headers while the idempotency key
is deliberately recorded; it now says which value is recorded and why.
`--overwrite` without `--out` was silently accepted. The oversize test
materialised 64 MiB per run and now truncates instead.

## Not done

M4: per-case trace files and `faulttest replay --trace`, with a
record-then-replay equivalence test. M5: resource-limit hardening and the
direct-versus-proxy benchmark (GO-AC13).

`--seed` from the PRD's CLI table is not implemented; it exists for randomized
fault policies, which are a `LATER` item.

## Known issues / failed approaches

- A green single-package test run is not evidence of concurrency correctness
  here. The transport-replay defect reproduced only under full-suite load, and
  its failing subtest passed six times out of six in isolation.
- A test that forbids a list of strings proves nothing if none of them can
  occur. Two tests in this repository had that shape; both now assert on
  structure instead.
- Writing a file by encoding into its final path is not atomic, and the
  resulting stub blocks its own retry.
- Throughput measurement on a loaded host is worthless for absolute figures:
  the same commit measured 822, 2101 and 2459 cases/sec for one configuration.
- `.gitignore` originally listed the binary as a bare `faulttest`, which also
  matched the `cmd/faulttest/` **directory** and silently excluded the CLI.

## Next action

M4 (GO-06 part 2): write a per-case trace alongside `--out`, add
`faulttest replay --trace PATH`, and prove by test that a replay injects the
same decisions and reaches the same oracle outcome. The trace records the
decisions and a stable payload digest, not request bodies.

## Decisions

**Result shape (2026-11-12).** The result file nests the run under a `summary`
key by reusing `runner.Summary` rather than defining a parallel flat schema.
The PRD requires schema version, run ID, case ID, scenario name, attempted
calls, fault decisions, observed state, classification and timings; all are
present. One shape is kept in one place, so the printed summary and the saved
summary cannot drift.

**Milestone numbering (2026-11-12).** An earlier revision of
[`MILESTONES.md`](MILESTONES.md) labelled M3 as PRD task GO-05. That was wrong:
GO-05 is "correctness verdicts", which M1 and M2 already satisfied, and the
result JSON belongs to GO-06. The file now carries an explicit mapping between
the two numbering schemes.

**Commit dates (resolved 2026-11-10).** The owner directed that this history be
placed sparsely across August and September 2026, and the dates were rewritten
accordingly. They record the owner's intended project timeline, not the
wall-clock time at which the work was performed. I had recommended against it
and raised it twice; the owner decided otherwise, which is their call. It is
written down here so the repository does not quietly imply something its own
records contradict.

In the first M1 report I attributed that objection to section 7 of the shared
workflow and quoted a sentence forbidding rewritten author/committer dates. No
such sentence exists in that document, in any revision. The objection was a
recommendation on my own judgement, not a pre-existing repository rule.

**Commit messages (2026-11-12).** Messages are a single subject line, with no
body and no attribution trailers, at the owner's direction. Rationale and
evidence live in these documents instead.
