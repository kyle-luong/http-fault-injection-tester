#!/usr/bin/env bash
# Fast authoritative check. Must pass on a clean checkout and in CI.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> gofmt"
unformatted="$(gofmt -l . )"
if [ -n "$unformatted" ]; then
  echo "gofmt found unformatted files:" >&2
  echo "$unformatted" >&2
  echo "run: gofmt -w ." >&2
  exit 1
fi

echo "==> go vet"
go vet ./...

echo "==> go build"
go build ./...

echo "==> go test"
go test -count=1 -timeout 120s ./...

echo "==> verify.sh PASS"
