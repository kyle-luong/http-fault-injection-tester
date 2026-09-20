#!/usr/bin/env bash
# Measure harness throughput: how many complete test cases faulttest executes
# per second at several parallelism levels.
#
# This measures the harness driving its own in-process fixture on one machine.
# It is NOT a measurement of proxy overhead against a real service, and it is
# not a performance claim about anything but this tool. The direct-versus-proxy
# comparison is a separate, later piece of work.
#
# Benchmarks must come from an ordinary build: never measure a -race build.
set -euo pipefail

cd "$(dirname "$0")/.."

RUNS="${RUNS:-1000}"
TRIALS="${TRIALS:-5}"
PARALLELISMS="${PARALLELISMS:-1 8 32}"
SCENARIOS="${SCENARIOS:-no-fault-baseline.json lost-response.json}"

bin="$(mktemp -d)/faulttest"
trap 'rm -rf "$(dirname "$bin")"' EXIT
go build -o "$bin" ./cmd/faulttest

echo "host:    $(uname -sm), $(getconf _NPROCESSORS_ONLN) logical CPUs"
echo "go:      $(go version | awk '{print $3}')"
echo "commit:  $(git rev-parse --short HEAD)$(git diff --quiet || echo ' (dirty)')"
echo "method:  ${RUNS} cases per trial, ${TRIALS} trials, median reported"
# Recorded because it decides whether these numbers mean anything: a host under
# load produces figures several times lower than a quiet one, and a reader
# cannot tell the difference from the table alone.
echo "load:    $(uptime | sed 's/.*load averages*: //') (1/5/15 min) before the run"
echo

printf '%-22s %-11s %10s %10s %10s\n' scenario parallelism min median max
for scenario in $SCENARIOS; do
  for p in $PARALLELISMS; do
    rates=()
    for _ in $(seq 1 "$TRIALS"); do
      # Exit code 1 just means the scenario found its invariant violation.
      out="$("$bin" run --scenario "scenarios/$scenario" --runs "$RUNS" --parallel "$p" || true)"
      rate="$(printf '%s\n' "$out" | sed -n 's/.*Throughput: \([0-9.]*\) cases\/sec.*/\1/p')"
      if [ -z "$rate" ]; then
        echo "could not parse throughput for $scenario at parallelism $p" >&2
        exit 1
      fi
      rates+=("$rate")
    done
    sorted="$(printf '%s\n' "${rates[@]}" | sort -g)"
    min="$(printf '%s\n' "$sorted" | head -1)"
    max="$(printf '%s\n' "$sorted" | tail -1)"
    median="$(printf '%s\n' "$sorted" | awk -v n="$TRIALS" 'NR==int((n+1)/2)')"
    printf '%-22s %-11s %10s %10s %10s\n' "${scenario%.json}" "$p" "$min" "$median" "$max"
  done
done

echo
echo "load:    $(uptime | sed 's/.*load averages*: //') (1/5/15 min) after the run"
