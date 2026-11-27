#!/usr/bin/env bash
# SDK end-to-end ship criterion (M5). Boots a REAL stack — PostgreSQL (Docker) +
# the API process + a real tenant API key — then drives it through the TypeScript
# SDK: create → write file → exec cat → read → list → destroy → 404.
#
# Usage: scripts/sdk-e2e.sh [driver]   (driver: fake|container, default fake)
#
# With LSB_PYTHON set to a Python interpreter that has the SDK and pytest
# installed, the Python SDK runs against the same stack afterwards.
#
# Requires: docker, go, node. Deterministic + self-cleaning (trap on EXIT).
set -euo pipefail

DRIVER="${1:-fake}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PG_CONTAINER="lsb-sdk-e2e-pg"
PG_PORT="${PG_PORT:-5433}"
API_PORT="${API_PORT:-8080}"
API_ADDR=":${API_PORT}"
BASE_URL="http://localhost:${API_PORT}"
export DATABASE_URL="postgres://postgres:postgres@localhost:${PG_PORT}/loopsandbox?sslmode=disable"

API_PID=""
cleanup() {
  set +e
  [ -n "$API_PID" ] && kill "$API_PID" 2>/dev/null
  docker rm -f "$PG_CONTAINER" >/dev/null 2>&1
}
trap cleanup EXIT

echo "==> [1/6] PostgreSQL (Docker) on :${PG_PORT}"
docker rm -f "$PG_CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$PG_CONTAINER" \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=loopsandbox \
  -p "${PG_PORT}:5432" postgres:16-alpine >/dev/null
echo "    waiting for postgres..."
for i in $(seq 1 30); do
  if docker exec "$PG_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then break; fi
  sleep 1
done

echo "==> [2/6] build binaries"
cd "$ROOT"
go build -o bin/api ./cmd/api
go build -o bin/sandboxctl ./cmd/sandboxctl
if [ "$DRIVER" = "container" ]; then
  # Sandboxes run the agent, which processes, logs, watches and sessions need.
  CGO_ENABLED=0 GOOS=linux go build -o bin/lsb-agent ./cmd/lsb-agent
  export LOOPSANDBOX_AGENT_BIN="$ROOT/bin/lsb-agent"
fi

echo "==> [3/6] start API (driver=${DRIVER}) on ${API_ADDR}"
LOOPSANDBOX_URL_SIGNING_KEY="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
  LOOPSANDBOX_SECRETS_KEY="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
  LOOPSANDBOX_ADDR="$API_ADDR" LOOPSANDBOX_DRIVER="$DRIVER" ./bin/api &
API_PID=$!
echo "    waiting for readiness..."
for i in $(seq 1 30); do
  if curl -fsS "${BASE_URL}/v1/readyz" >/dev/null 2>&1; then break; fi
  sleep 1
done

echo "==> [4/6] create tenant + API key"
TENANT_OUT="$(./bin/sandboxctl tenants create --name sdk-e2e --plan team)"
TENANT_ID="$(echo "$TENANT_OUT" | sed -n 's/^tenant created: \(tnt_[^ ]*\).*/\1/p')"
[ -n "$TENANT_ID" ] || { echo "could not parse tenant id from: $TENANT_OUT"; exit 1; }
KEY_OUT="$(./bin/sandboxctl api-keys create --tenant "$TENANT_ID")"
LSB_API_KEY="$(echo "$KEY_OUT" | tail -n1)"
[ -n "$LSB_API_KEY" ] || { echo "could not parse api key"; exit 1; }
echo "    tenant=${TENANT_ID} key=${LSB_API_KEY:0:8}…"

echo "==> [5/6] build SDK"
cd "$ROOT/sdk/javascript"
npm run build >/dev/null

echo "==> [6/6] run SDK E2E flow"
LSB_BASE_URL="$BASE_URL" LSB_API_KEY="$LSB_API_KEY" LSB_IMAGE="${LSB_IMAGE:-busybox:latest}" \
  node test/e2e.mjs

if [ -n "${LSB_PYTHON:-}" ]; then
  echo "==> Python SDK against the same stack"
  cd "$ROOT/sdk/python"
  LSB_BASE_URL="$BASE_URL" LSB_API_KEY="$LSB_API_KEY" LSB_IMAGE="${LSB_IMAGE:-busybox:latest}" \
    "$LSB_PYTHON" -m pytest -q tests/test_deployment_live.py
else
  echo "==> Python SDK: skipped, set LSB_PYTHON to an interpreter with the package and pytest installed"
fi

echo "==> /metrics after the run:"
curl -fsS "${BASE_URL}/metrics" | grep -E 'loopsandbox_(sandboxes_created_total|sandboxes_destroyed_total|executions_total)' || true

echo "SDK E2E ship criterion: PASSED"
