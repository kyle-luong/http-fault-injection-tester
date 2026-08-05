#!/usr/bin/env bash
# Slower pre-PR check: everything in verify.sh plus the race detector.
set -euo pipefail

cd "$(dirname "$0")/.."

./scripts/verify.sh

echo "==> go test -race"
go test -race -count=1 -timeout 300s ./...

echo "==> verify-full.sh PASS"
