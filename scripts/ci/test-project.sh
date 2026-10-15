#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

echo "Validating Docker container tag rules..."
python3 scripts/test-docker-tags.py

echo "Validating Compose interpolation and security configuration..."
python3 scripts/test-compose.py

if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  echo "Validating Compose with docker compose config..."
  echo "CODEX_PROXY_API_KEY=ci-dummy-key" > deploy/.env
  docker compose -f deploy/compose.yaml --env-file deploy/.env config >/dev/null
  rm -f deploy/.env
fi

if [ "${CODEX_PROXY_BIFROST_TEST:-0}" = "1" ]; then
  echo "Testing Bifrost compatibility..."
  go test -race -run '^TestBifrostCompatibility$' -v -timeout 10m ./internal/proxy
fi

echo "Project checks passed."
