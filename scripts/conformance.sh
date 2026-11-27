#!/usr/bin/env bash
# Run the conformance suite against a real stack: PostgreSQL (Docker) + the API
# process with the chosen runtime driver + two tenants with one API key each.
#
# Usage: scripts/conformance.sh [driver]   (driver: container|firecracker|fake, default container)
#
# The firecracker driver needs /dev/kvm and the assets from scripts/fc-assets.sh.
# To run the suite against a deployment that is already up, skip this script and
# set LSB_BASE_URL, LSB_API_KEY and LSB_API_KEY_OTHER:
#
#   go test -tags=conformance -count=1 -v ./test/conformance/
#
# Requires: docker, go, curl. Self-cleaning (trap on EXIT).
set -euo pipefail

DRIVER="${1:-container}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PG_CONTAINER="lsb-conformance-pg-$$"
PG_PORT="${PG_PORT:-55432}"
API_PORT="${API_PORT:-18080}"
BASE_URL="http://127.0.0.1:${API_PORT}"
export DATABASE_URL="postgres://postgres:postgres@127.0.0.1:${PG_PORT}/loopsandbox?sslmode=disable"

if [ "$DRIVER" = "firecracker" ]; then
  FCDIR="${LOOPSANDBOX_FC_DIR:-$HOME/.cache/loopsandbox/fc}"
  [ -r /dev/kvm ] || { echo "conformance: /dev/kvm is not readable; the firecracker driver cannot run" >&2; exit 2; }
  [ -x "$FCDIR/firecracker" ] || { echo "conformance: Firecracker assets missing in $FCDIR; run scripts/fc-assets.sh" >&2; exit 2; }
  export LOOPSANDBOX_FC_BIN="${LOOPSANDBOX_FC_BIN:-$FCDIR/firecracker}"
  export LOOPSANDBOX_FC_KERNEL="${LOOPSANDBOX_FC_KERNEL:-$FCDIR/vmlinux.bin}"
  export LOOPSANDBOX_FC_ROOTFS="${LOOPSANDBOX_FC_ROOTFS:-$FCDIR/rootfs-lsb.ext4}"
fi

API_PID=""
API_LOG="$(mktemp)"
cleanup() {
  status=$?
  set +e
  [ -n "$API_PID" ] && kill "$API_PID" 2>/dev/null && wait "$API_PID" 2>/dev/null
  docker rm -f "$PG_CONTAINER" >/dev/null 2>&1
  if [ "$status" -ne 0 ]; then
    echo "==> API log (last 30 lines):" >&2
    tail -n 30 "$API_LOG" >&2
  fi
  rm -f "$API_LOG"
}
trap cleanup EXIT

echo "==> [1/5] PostgreSQL (Docker) on 127.0.0.1:${PG_PORT}"
docker run -d --name "$PG_CONTAINER" \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=loopsandbox \
  -p "127.0.0.1:${PG_PORT}:5432" postgres:16-alpine >/dev/null
for _ in $(seq 1 60); do
  docker exec "$PG_CONTAINER" pg_isready -U postgres >/dev/null 2>&1 && break
  sleep 1
done

echo "==> [2/5] build binaries"
cd "$ROOT"
BIN="$(mktemp -d)"
go build -o "$BIN/api" ./cmd/api
go build -o "$BIN/sandboxctl" ./cmd/sandboxctl
if [ "$DRIVER" = "container" ]; then
  # Sandboxes run the agent, which is what code sessions need.
  CGO_ENABLED=0 GOOS=linux go build -o "$BIN/lsb-agent" ./cmd/lsb-agent
  export LOOPSANDBOX_AGENT_BIN="$BIN/lsb-agent"
  # Sandboxes get a network, so the "network" capability is exercised. Set
  # LOOPSANDBOX_CONTAINER_NETWORK=off to run the suite on a host without internet.
  if [ "${LOOPSANDBOX_CONTAINER_NETWORK:-on}" = "off" ]; then
    unset LOOPSANDBOX_CONTAINER_NETWORK
  else
    export LOOPSANDBOX_CONTAINER_NETWORK="${LOOPSANDBOX_CONTAINER_NETWORK:-on}"
  fi
fi

echo "==> [3/5] start API (driver=${DRIVER}) on ${BASE_URL}"
# The API runs the worker loops itself, so the TTL case needs no second process, and a
# short interval keeps that case fast.
LOOPSANDBOX_URL_SIGNING_KEY="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
  LOOPSANDBOX_SECRETS_KEY="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
  LOOPSANDBOX_ADDR="127.0.0.1:${API_PORT}" LOOPSANDBOX_DRIVER="$DRIVER" \
  LOOPSANDBOX_EMBEDDED_WORKER=true LOOPSANDBOX_RECONCILE_INTERVAL=1s \
  "$BIN/api" >"$API_LOG" 2>&1 &
API_PID=$!
ready=""
for _ in $(seq 1 60); do
  if curl -fsS "${BASE_URL}/v1/readyz" >/dev/null 2>&1; then ready=1; break; fi
  kill -0 "$API_PID" 2>/dev/null || break
  sleep 1
done
[ -n "$ready" ] || { echo "conformance: the API did not become ready" >&2; exit 1; }

echo "==> [4/5] create two tenants, one API key each"
new_key() {
  local tenant
  # The team plan: the suite creates more sandboxes than the starter plan allows.
  tenant="$("$BIN/sandboxctl" tenants create --name "$1" --plan team | sed -n 's/^tenant created: \(tnt_[^ ]*\).*/\1/p')"
  [ -n "$tenant" ] || { echo "conformance: could not create tenant $1" >&2; exit 1; }
  "$BIN/sandboxctl" api-keys create --tenant "$tenant" --scopes sandbox:execute,api_keys:manage,secrets:manage | tail -n1
}
LSB_API_KEY="$(new_key conformance-a)"
LSB_API_KEY_OTHER="$(new_key conformance-b)"

echo "==> [5/5] run the conformance suite"
SUMMARY="$(mktemp)"
suite_status=0
LSB_BASE_URL="$BASE_URL" LSB_API_KEY="$LSB_API_KEY" LSB_API_KEY_OTHER="$LSB_API_KEY_OTHER" \
  LSB_SUMMARY_FILE="$SUMMARY" \
  go test -tags=conformance -count=1 -timeout=30m ./test/conformance/ "${@:2}" || suite_status=$?
cat "$SUMMARY"
rm -f "$SUMMARY"
exit "$suite_status"
