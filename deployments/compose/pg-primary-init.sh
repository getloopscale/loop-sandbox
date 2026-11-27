#!/bin/bash
# Primary init (M11): create the replication role + allow the replica to stream. Runs
# once via docker-entrypoint-initdb.d on the primary.
set -e
psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" <<-SQL
    CREATE ROLE repl WITH REPLICATION LOGIN PASSWORD 'replpass';
SQL
# POSTGRES_HOST_AUTH_METHOD=trust covers `host all all all` (the pgbouncer connection) but
# only adds REPLICATION trust for localhost — so the replica's streaming connection needs
# an explicit rule for the private compose network.
echo "host replication repl all trust" >> "$PGDATA/pg_hba.conf"
