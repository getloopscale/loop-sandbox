#!/usr/bin/env bash
# Validate the PostgreSQL HA topology (M11 B2): streaming replication + pgbouncer pooling,
# against REAL containers. Brings the topology up, asserts behavior, prints evidence, and
# always tears down. Exit 0 = all assertions passed.
set -euo pipefail
cd "$(dirname "$0")/../deployments/compose"
COMPOSE="docker compose -f pg-ha.yml -p gspgha"

cleanup() { $COMPOSE down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== bringing up primary + replica + pgbouncer =="
$COMPOSE up -d >/dev/null

echo "== waiting for replica to finish base-backup + stream =="
for i in $(seq 1 60); do
  if $COMPOSE exec -T pg-replica pg_isready -U postgres -d loopsandbox >/dev/null 2>&1; then break; fi
  sleep 2
done

pass=0; fail=0
check() { if eval "$2"; then echo "PASS: $1"; pass=$((pass+1)); else echo "FAIL: $1"; fail=$((fail+1)); fi; }

# 1. A streaming standby is connected to the primary.
echo "== pg_stat_replication (primary) =="
$COMPOSE exec -T pg-primary psql -U postgres -d loopsandbox -c \
  "SELECT client_addr, state, sync_state FROM pg_stat_replication;"
check "primary has a streaming standby" \
  '[ "$($COMPOSE exec -T pg-primary psql -tAU postgres -d loopsandbox -c "SELECT count(*) FROM pg_stat_replication WHERE state = \$\$streaming\$\$")" -ge 1 ]'

# 2. The replica is read-only (in recovery).
check "replica is in recovery (read-only)" \
  '[ "$($COMPOSE exec -T pg-replica psql -tAU postgres -d loopsandbox -c "SELECT pg_is_in_recovery()")" = "t" ]'

# 3. Write on the primary → replicates to the replica.
$COMPOSE exec -T pg-primary psql -U postgres -d loopsandbox -c \
  "CREATE TABLE IF NOT EXISTS repltest(id int primary key); INSERT INTO repltest VALUES (42) ON CONFLICT DO NOTHING;" >/dev/null
sleep 2
check "row written on primary is readable on the replica" \
  '[ "$($COMPOSE exec -T pg-replica psql -tAU postgres -d loopsandbox -c "SELECT id FROM repltest WHERE id=42")" = "42" ]'

# 4. The replica rejects writes (proves it is a true standby).
check "replica rejects writes (standby)" \
  '! $COMPOSE exec -T pg-replica psql -U postgres -d loopsandbox -c "INSERT INTO repltest VALUES (99)" >/dev/null 2>&1'

# 5. pgbouncer pools connections (write path goes through it → primary).
echo "== pgbouncer SHOW POOLS =="
PGPASSWORD=postgres psql -h 127.0.0.1 -p 6432 -U postgres -d loopsandbox -c \
  "CREATE TABLE IF NOT EXISTS viabouncer(id int);" >/dev/null 2>&1 || true
check "pgbouncer accepts a pooled connection to the primary" \
  'PGPASSWORD=postgres psql -h 127.0.0.1 -p 6432 -U postgres -d loopsandbox -tAc "SELECT 1" 2>/dev/null | grep -q 1'
PGPASSWORD=postgres psql -h 127.0.0.1 -p 6432 -U pgbouncer -d pgbouncer -c "SHOW POOLS;" 2>/dev/null | head -5 || true

echo "== result: $pass passed, $fail failed =="
[ "$fail" -eq 0 ]
