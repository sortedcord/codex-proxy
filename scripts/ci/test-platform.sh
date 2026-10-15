#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

os="$(uname -s 2>/dev/null || echo unknown)"
case "$os" in
  MINGW*|MSYS*|CYGWIN*|Windows_NT*)
    go test -v ./internal/storage
    go build -trimpath -o codex-proxy.exe ./cmd/codex-proxy
    python3 scripts/test-packaged-binary.py ./codex-proxy.exe
    ;;
  *)
    ./scripts/build.sh
    python3 scripts/test-packaged-binary.py ./bin/codex-proxy
    ;;
esac
