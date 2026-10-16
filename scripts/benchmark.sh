#!/usr/bin/env bash
# Measure the proxy's cost: one no-fault operation against the built-in
# fixture, directly and through the proxy, at parallelism 1, 8 and 32.
#
# This is the PRD's GO-AC13 / GO-T15 evidence for added latency. It is a
# loopback measurement of this tool against its own in-process fixture on one
# machine, not a statement about any real service. Case throughput through the
# whole runner is a separate measurement: ./scripts/throughput.sh.
#
# Each trial is a separate `go test` invocation, so the direct and proxy
# measurements of one trial are adjacent in time. Using -count instead would
# run every direct trial first and every proxy trial afterwards, which leaves
# the comparison exposed to anything that drifts on the host in between.
#
# Benchmarks must come from an ordinary build: never measure a -race build.
set -euo pipefail

cd "$(dirname "$0")/.."

# A fixed operation count per trial keeps the workload identical across paths
# and parallelism levels, so the comparison is not also a comparison of
# different amounts of work.
OPS="${OPS:-2000}"
TRIALS="${TRIALS:-5}"
RAW="${RAW:-$(mktemp)}"

cpu="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || true)"
[ -n "$cpu" ] || cpu="$(uname -p)"

echo "host:    $(uname -sm), $(getconf _NPROCESSORS_ONLN) logical CPUs, $cpu"
echo "go:      $(go version | awk '{print $3}')"
echo "commit:  $(git rev-parse --short HEAD)$(git diff --quiet || echo ' (uncommitted changes)')"
echo "method:  ${OPS} operations per trial, ${TRIALS} trials, each trial one"
echo "         invocation measuring direct then proxy, non-race build"
# Recorded because it decides whether these numbers mean anything: a host under
# load produces figures several times worse than a quiet one, and a reader
# cannot tell the difference from the table alone.
echo "load:    $(uptime | sed 's/.*load averages*: //') (1/5/15 min) before the run"
echo

: > "$RAW"
for trial in $(seq 1 "$TRIALS"); do
  out="$(go test -run '^$' -bench BenchmarkOperation -benchmem \
    -benchtime "${OPS}x" -count 1 ./internal/integration/)" || {
    echo "benchmark trial $trial failed:" >&2
    printf '%s\n' "$out" >&2
    exit 1
  }
  printf '%s\n' "$out" | grep -E '^Benchmark' | sed "s/^/trial $trial /" >> "$RAW"
done

echo "raw results, every trial (GO-T15 asks for these, not just a summary):"
cat "$RAW"
echo

python3 - "$RAW" <<'PY'
import collections, statistics, sys

# One record per trial per sub-benchmark, so a printed row is one real
# measurement rather than a mix of columns from different trials.
rows = collections.defaultdict(list)
for line in open(sys.argv[1]):
    parts = line.split()
    if len(parts) < 4 or parts[0] != "trial":
        continue
    name = parts[2].split("-")[0].removeprefix("BenchmarkOperation/")
    metrics = {}
    for value, unit in zip(parts[4::2], parts[5::2]):
        try:
            metrics[unit] = float(value)
        except ValueError:
            pass
    if "ops/sec" in metrics:
        rows[name].append(metrics)

COLUMNS = ("ops/sec", "wall-ms", "mean-us", "p50-us", "p95-us", "B/op", "allocs/op", "goroutines")

def median_trial(name):
    """The trial whose throughput is the median, printed whole."""
    trials = rows.get(name, [])
    if not trials:
        return None
    ordered = sorted(trials, key=lambda m: m["ops/sec"])
    return ordered[(len(ordered) - 1) // 2]

def spread(name, unit):
    values = [m[unit] for m in rows.get(name, []) if unit in m]
    return (min(values), max(values)) if values else None

paths = ["direct", "proxy"]
levels = sorted({n.split("parallel=")[1] for n in rows}, key=int)

header = f"{'path':8} {'parallel':>8}" + "".join(f"{c:>11}" for c in COLUMNS)
print("median trial by throughput, printed whole (one real measurement per row)")
print(header)
for p in paths:
    for lvl in levels:
        row = median_trial(f"{p}/parallel={lvl}")
        if row is None:
            continue
        cells = "".join(f"{row.get(c, float('nan')):>11.0f}" for c in COLUMNS)
        print(f"{p:8} {lvl:>8}{cells}")

print()
print("between-trial spread, min .. max over all trials")
print(f"{'path':8} {'parallel':>8} {'ops/sec':>19} {'p50-us':>17} {'p95-us':>19}")
for p in paths:
    for lvl in levels:
        name = f"{p}/parallel={lvl}"
        if name not in rows:
            continue
        cells = []
        for unit in ("ops/sec", "p50-us", "p95-us"):
            s = spread(name, unit)
            cells.append(f"{s[0]:.0f}..{s[1]:.0f}" if s else "-")
        print(f"{p:8} {lvl:>8} {cells[0]:>19} {cells[1]:>17} {cells[2]:>19}")

print()
print("proxy minus direct, from the median trials above")
print(f"{'parallel':>8} {'mean us':>9} {'p50 us':>8} {'p95 us':>8} {'ops/sec':>10} {'goroutines':>11}")
for lvl in levels:
    d, x = median_trial(f"direct/parallel={lvl}"), median_trial(f"proxy/parallel={lvl}")
    if d is None or x is None:
        continue
    def delta(unit):
        if unit not in d or unit not in x:
            return float("nan")
        return x[unit] - d[unit]
    print(f"{lvl:>8} {delta('mean-us'):>+9.0f} {delta('p50-us'):>+8.0f} {delta('p95-us'):>+8.0f}"
          f" {delta('ops/sec'):>+10.0f} {delta('goroutines'):>+11.0f}")

print()
print("throughput ratio, direct over proxy, per trial")
for lvl in levels:
    ds = [m["ops/sec"] for m in rows.get(f"direct/parallel={lvl}", [])]
    xs = [m["ops/sec"] for m in rows.get(f"proxy/parallel={lvl}", [])]
    if ds and xs:
        ratios = [d / x for d, x in zip(ds, xs)]
        print(f"  parallel={lvl:<3} {' '.join(f'{r:.2f}' for r in ratios)}"
              f"   (median {statistics.median(ratios):.2f})")
PY

echo
echo "load:    $(uptime | sed 's/.*load averages*: //') (1/5/15 min) after the run"
