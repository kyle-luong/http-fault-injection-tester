# M2 throughput measurement

How many complete test cases `faulttest` executes per second, at three
parallelism levels. Reproduce with `./scripts/throughput.sh`.

## What this does and does not measure

It measures **the harness driving its own in-process fixture on one machine**:
reset, the attempt sequence through the proxy, settlement, the state read and
the verdict, for every case. Both the tool and the application under test share
the same CPUs, so this is a self-contained figure for the runner, not a
statement about any real service.

It is **not** the PRD's GO-AC13 benchmark. That one compares the proxy against
a direct connection and reports added latency percentiles, memory and goroutine
counts, and it needs a quiet host. It remains M5.

## Result

```
host:    Darwin arm64, 10 logical CPUs
go:      go1.27.4
method:  1000 cases per trial, 7 trials, median reported
load:    27.77 23.88 13.57 (1/5/15 min) before the run

scenario               parallelism        min     median        max
no-fault-baseline      1               1421.7     2458.6     3359.4
no-fault-baseline      8               2317.4     3948.3     4428.7
no-fault-baseline      32              4216.2     4788.6     4930.4
lost-response          1                466.8      686.9     1065.0
lost-response          8               1483.7     1912.6     2020.7
lost-response          32              2117.4     2298.8     2372.4

load:    21.91 22.80 13.48 (1/5/15 min) after the run
```

Units are cases per second. `lost-response` is slower than the baseline
because each of its cases makes two attempts and destroys a connection.

## Read these numbers with the caveat attached

**The host was heavily loaded.** A load average above 20 on a 10-CPU machine
means these figures are indicative lower bounds, not a benchmark. Three runs of
the same script on the same commit produced medians of 822, 2101 and 2459
cases/sec for the no-fault baseline at parallelism 1 — a threefold spread
explained entirely by what else the machine was doing, which is why
`throughput.sh` now records the load average beside the numbers.

Publishing a single figure from this data would be false precision. Anyone
wanting a citable number should re-run the script on an otherwise idle host.

## What does survive the noise

The ordering held in every run: throughput rises with parallelism, and the
spread between trials narrows as parallelism increases. That is the claim M2
needs — the bounded worker pool does convert concurrency into throughput —
and it does not depend on the absolute values.

The gain is clearly sublinear: 32 workers do not give 32 times the throughput
of one. That is expected here, because the fixture is competing for the same
CPUs as the harness driving it, and it is a further reason not to treat any of
these numbers as a property of the proxy.

No figure anywhere in this repository was estimated, scaled or rounded from
anything other than a recorded run of `./scripts/throughput.sh`.
