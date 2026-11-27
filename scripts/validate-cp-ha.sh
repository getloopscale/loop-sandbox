#!/usr/bin/env bash
# Validate control-plane HA (M11): two stateless API replicas behind a load balancer share
# one PostgreSQL. Proves cross-replica state consistency + survival of a replica failure +
# rolling-migration safety (both replicas apply the idempotent migrations at startup).
# Requires the loopsandbox:ha image (docker build -t loopsandbox:ha .) and a
# sandboxctl binary at /tmp/sandboxctl (go build -o /tmp/sandboxctl ./cmd/sandboxctl).
set -euo pipefail
cd "$(dirname "$0")/../deployments/compose"
COMPOSE="docker compose -f control-plane-ha.yml -p gscpha"
LB="http://127.0.0.1:8090"

cleanup() { $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== bringing up pg + api-1 + api-2 + lb =="
$COMPOSE up -d >/dev/null

echo "== waiting for both API replicas to be ready via the LB =="
for i in $(seq 1 60); do
  if curl -fsS "$LB/v1/readyz" >/dev/null 2>&1; then break; fi
  sleep 2
done

pass=0; fail=0
check() { if eval "$2"; then echo "PASS: $1"; pass=$((pass+1)); else echo "FAIL: $1"; fail=$((fail+1)); fi; }

# Rolling-migration safety: both replicas booted (each ran applyMigrations idempotently) —
# if a second replica's migration re-run had crashed, it would not be healthy.
check "both replicas booted (idempotent migrations across replicas)" \
  '[ "$($COMPOSE ps --status running --services | grep -c "^api-")" -eq 2 ]'

# Seed a tenant + API key straight into the shared PG (host sandboxctl via the exposed port).
export DATABASE_URL="postgres://postgres:postgres@127.0.0.1:6433/loopsandbox?sslmode=disable"
TENANT=$(/tmp/sandboxctl tenants create --name ha 2>/dev/null | sed -n 's/^tenant created: \([^ ]*\).*/\1/p')
# The raw API key is printed on its own line ("shown once"); take the last non-empty line.
KEY=$(/tmp/sandboxctl api-keys create --tenant "$TENANT" --scopes sandbox:write,sandbox:read 2>/dev/null | grep -vE '^\s*$|shown once' | tail -1 | tr -d '[:space:]')
echo "seeded tenant=$TENANT key=${KEY:0:8}…"

# Create a sandbox through the LB (lands on one replica, persisted to PG).
SID=$(curl -fsS -X POST "$LB/v1/sandboxes" -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" -d '{"image":"busybox"}' 2>/dev/null | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
check "create a sandbox via the LB" '[ -n "$SID" ]'

# Read it back many times through the LB — round-robin hits BOTH replicas; every read is 200
# because the state lives in the shared PG (not in a replica's memory).
ok=0; for i in $(seq 1 10); do
  code=$(curl -s -o /dev/null -w "%{http_code}" "$LB/v1/sandboxes/$SID" -H "Authorization: Bearer $KEY")
  [ "$code" = "200" ] && ok=$((ok+1))
done
check "sandbox readable across replicas (shared PG state)" '[ "$ok" -eq 10 ]'

# Kill one replica → the LB fails over to the survivor; reads still 200 (HA under failure).
echo "== killing api-1 =="
$COMPOSE stop api-1 >/dev/null 2>&1
sleep 2
ok=0; for i in $(seq 1 10); do
  code=$(curl -s -o /dev/null -w "%{http_code}" "$LB/v1/sandboxes/$SID" -H "Authorization: Bearer $KEY")
  [ "$code" = "200" ] && ok=$((ok+1))
done
check "sandbox still served after a replica dies (HA failover)" '[ "$ok" -ge 8 ]'

echo "== result: $pass passed, $fail failed =="
[ "$fail" -eq 0 ]
