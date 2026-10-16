# M5: what the proxy costs

The PRD's GO-AC13 and GO-T15 evidence: one no-fault operation against the
built-in fixture, measured directly and through the fault proxy, with the same
payload and the same parallelism on both paths. Reproduce with
`./scripts/benchmark.sh`; case throughput through the whole runner comes from
`./scripts/throughput.sh`.

Raw output for every trial of every session is in
[`raw/`](raw/), because a median is not a measurement and GO-T15 asks for the
results themselves.

## What this measures

Both paths send an identical request — same body, same headers including the
harness control headers, same client — so the only difference is one extra
loopback hop and the proxy's own work: read the body, consult the fault
schedule twice, forward, copy headers, write the response. The per-case reset is
control traffic, goes straight to the fixture, and is outside both the timer and
the allocation counters. One warm operation per worker runs before the timer
starts, so connection establishment is not inside the window.

Each trial is one `go test` invocation measuring direct then proxy, so the two
halves of a comparison are adjacent in time. Running all direct trials first and
all proxy trials afterwards — what `-count` does — would leave the comparison
exposed to anything that drifts on the host in between.

The tool, the proxy and the application under test share one machine's CPUs.
This is a loopback figure for this tool, not a statement about any real service.

Deliberately absent: p99. A trial has 2,000 samples, so a p99 would rest on its
twenty worst, and the PRD asks for that figure only with adequate samples and
caveats. p95 is reported instead.

## This host was never quiet, and it matters

Two full sessions, same code, same method, hours apart:

| | session B | session C |
|---|---|---|
| load average (1 min) | 6.24 | 14.51 |
| direct, parallelism 1 | 9,655 ops/sec | 6,880 ops/sec |
| proxy, parallelism 1 | 5,276 ops/sec | 3,634 ops/sec |
| added p50 latency, parallelism 1 | +86µs | +126µs |
| **throughput ratio, direct ÷ proxy** | **1.98** | **1.89** |

Absolute throughput moved by 40% and added latency by 47%. The **ratio moved by
5%**. An earlier session on the same code at load average 4.11, using a method
with two known biases (no warmup, and `-count` batching), gave 2.18 — the same
neighbourhood.

So the figure this benchmark actually establishes is the ratio: **the proxy
roughly halves throughput, and the median ratio sat between 1.89 and 2.07 at
every parallelism level in both sessions.** Per-trial ratios ranged 1.08 to
2.31, the low outliers being trials where the direct path was unusually slow.

Absolute microsecond figures are properties of the host at that minute. None of
them is "this tool's latency cost", and no single one of them is quoted
elsewhere in this repository as though it were.

## Session C, verbatim

```
host:    Darwin arm64, 10 logical CPUs, Apple M4
go:      go1.27.4
commit:  c04ebaf plus the uncommitted M5 changes this document lands with
method:  2000 operations per trial, 5 trials, each trial one
         invocation measuring direct then proxy, non-race build
load:    14.51 15.04 11.87 (1/5/15 min) before the run

median trial by throughput, printed whole (one real measurement per row)
path     parallel    ops/sec    wall-ms    mean-us     p50-us     p95-us       B/op  allocs/op goroutines
direct          1       6880        290        145        133        210       8444        106          8
direct          8      21378         93        369        346        553       8710        106         32
direct         32      20179         99       1552       1441       2340       9281        107        107
proxy           1       3634        550        275        259        360      18872        215         14
proxy           8      10390        192        760        713       1135      19553        216         59
proxy          32       9993        200       3162       2883       4746      19461        217        209

between-trial spread, min .. max over all trials
path     parallel             ops/sec            p50-us              p95-us
direct          1          3435..7206          124..202            194..657
direct          8        19353..23221          318..353            487..769
direct         32        17871..20982        1357..1462          2173..3487
proxy           1          3190..3692          250..278            360..482
proxy           8         9440..10809          682..737          1055..1487
proxy          32         9716..10822        2623..2949          3983..5076

proxy minus direct, from the median trials above
parallel   mean us   p50 us   p95 us    ops/sec  goroutines
       1      +130     +126     +150      -3246          +6
       8      +391     +367     +582     -10988         +27
      32     +1610    +1442    +2406     -10186        +102

throughput ratio, direct over proxy, per trial
  parallel=1   1.95 1.93 1.08 1.89 1.89   (median 1.89)
  parallel=8   2.15 2.07 2.05 2.10 2.06   (median 2.07)
  parallel=32  1.94 1.98 1.84 2.02 2.05   (median 1.98)

load:    13.95 14.89 11.87 (1/5/15 min) after the run
```

During this session Discord alone was using over 200% CPU, alongside four
orphaned spin loops left behind by an earlier test. That is what load average
14.51 on ten CPUs means, and it is why the ratio is the published figure.

## Case throughput

The PRD also asks for cases per second at parallelism 1, 8 and 32. That is a
different measurement — a case is reset, attempt sequence, settlement, state
read and verdict, all through the proxy — and `./scripts/throughput.sh` produces
it. Measured in the same session, 500 cases per trial, 5 trials, median:

```
scenario               parallelism        min     median        max
no-fault-baseline      1               1013.0     1664.5     1774.8
no-fault-baseline      8               4176.8     4702.5     4783.7
no-fault-baseline      32              3273.9     3728.3     4231.0
lost-response          1                746.8      794.2      803.6
lost-response          8               2075.7     2104.9     2190.5
lost-response          32              1969.6     2164.3     2209.1
```

`lost-response` is slower because each of its cases makes two attempts and
destroys a connection. There is no direct-versus-proxy comparison here: a case
is defined by going through the proxy, so there is no "direct" case to measure.

## What the numbers say

**Latency follows from throughput, not the other way round.** At a fixed number
of in-flight requests, mean latency is concurrency ÷ throughput. The published
table obeys it: direct at parallelism 32, 20,179/sec × 1,552µs = 31.3 ≈ 32 in
flight; proxy, 9,993/sec × 3,162µs = 31.6 ≈ 32. So the +1,442µs at p50 is the
same ~2× throughput deficit applied to 32 queued requests, not a per-request
cost that grows. Adding "the proxy costs 1.4ms" to a latency budget would be
wrong; the deficit is multiplicative and the microseconds depend on how many
requests are in flight.

**Allocation roughly doubles**: 8,444 B and 106 allocations per operation
direct, 18,872 B and 215 through the proxy — 2.2× bytes, 2.0× allocations. A
second request and response on the same machine is exactly what that buys.

**Goroutine growth is bounded and proportional**: +6 at parallelism 1, +27 at 8,
+102 at 32 — one goroutine per connection on each side of the hop, plus the
accept loop. The count is a single sample per trial and moves by a few between
trials, so read it as an order of magnitude, not a constant.

**Parallelism 8 beats parallelism 32** on both paths in this session, as it did
in session B. Ten CPUs hosting client, proxy and fixture are past their useful
concurrency somewhere below 32; that is a property of the arrangement, not of the
proxy, and it affects both paths.

## Read these numbers with the caveat attached

No performance improvement is claimed anywhere in this repository. Nothing was
optimised for these numbers, no threshold derived from them runs in CI by PRD
instruction, and the one figure offered as a result — the ratio — is the one
that held across a 2.4× change in host load.
