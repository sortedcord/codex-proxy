#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
if [ -n "$(gofmt -l cmd internal integration/bifrost)" ]; then
  echo 'Go files need formatting' >&2
  exit 1
fi
go vet ./...
go test -race -cover ./...
go build -trimpath -ldflags='-s -w' -o bin/codex-proxy ./cmd/codex-proxy
