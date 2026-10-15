#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

count="${STRESS_COUNT:-3}"

echo "Running concurrency stress tests (count=${count})..."
go test -race -shuffle=on -count="$count" ./internal/oauth ./internal/proxy ./internal/status ./internal/storage

echo "Stress tests passed."
