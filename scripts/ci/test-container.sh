#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
  if [ "${CI:-false}" = "true" ] || [ "${GITHUB_ACTIONS:-false}" = "true" ]; then
    echo "Error: Docker is required in CI environment but not found." >&2
    exit 1
  fi
  echo "Docker not available; skipping container smoke test in local environment."
  exit 0
fi

image_tag="codex-proxy:test"
container_name="codex-proxy-smoke-$$-$(date +%s)"

echo "Building container image..."
docker build -t "$image_tag" .

echo "Verifying container user is 10001:10001..."
user=$(docker image inspect -f '{{.Config.User}}' "$image_tag")
if [ "$user" != "10001:10001" ]; then
  echo "Expected container user 10001:10001, got $user" >&2
  exit 1
fi

echo "Starting container under hardened sandbox..."
trap 'docker logs "$container_name" 2>/dev/null || true; docker rm -f -v "$container_name" 2>/dev/null || true' EXIT

docker run -d \
  --name "$container_name" \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --mount type=volume,dst=/home/app/.codex \
  --mount type=volume,dst=/home/app/.local/share/codex-proxy \
  -p 127.0.0.1:8080:8080 \
  -e CODEX_PROXY_API_KEY=docker-smoke-inference \
  -e CODEX_PROXY_MANAGEMENT_KEY=docker-smoke-management \
  "$image_tag"

echo "Waiting for health endpoint..."
curl --fail --retry 20 --retry-delay 1 --retry-connrefused \
  http://127.0.0.1:8080/health > health.json

python3 - <<'PY'
import json
with open('health.json') as stream:
    health = json.load(stream)
assert health['status'] == 'ok', f"unexpected status: {health}"
assert health['credential_configured'] is False, f"credential_configured should be False"
PY
rm -f health.json

echo "Verifying authentication enforcement..."
unauth=$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/status/account)
if [ "$unauth" != "401" ]; then
  echo "Expected status 401 for unauthenticated request, got $unauth" >&2
  exit 1
fi

mgmt=$(curl -sS -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer docker-smoke-management' http://127.0.0.1:8080/status/account)
if [ "$mgmt" != "503" ]; then
  echo "Expected status 503 for valid management key with no upstream credentials, got $mgmt" >&2
  exit 1
fi

echo "Stopping container..."
docker stop --time 30 "$container_name"
exit_code=$(docker inspect -f '{{.State.ExitCode}}' "$container_name")
if [ "$exit_code" != "0" ]; then
  echo "Container exited with non-zero code $exit_code" >&2
  exit 1
fi

echo "Container smoke test passed."
