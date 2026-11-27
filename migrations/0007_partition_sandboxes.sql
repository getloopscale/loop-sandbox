-- 0007_partition_sandboxes.sql — M11 B2: partition the core sandboxes table by cell_id.
--
-- Extends the jobs partitioning (0006) to the primary entity. sandboxes has NO inbound
-- foreign keys (executions.sandbox_id is a plain column, not an FK), so this partitions
-- cleanly. The partition key (cell_id) joins the primary key → (cell_id, tenant_id, id).
--
-- IDEMPOTENT (same contract as 0006): the runner re-applies every file on each boot, so
-- the conversion is guarded to run ONLY while sandboxes is still an ordinary table
-- (relkind 'r'); once partitioned ('p') it is a no-op.
--
-- ATOMICITY REQUIREMENT: this whole conversion MUST run as ONE statement (the single DO
-- block) so a crash mid-conversion rolls back entirely — the runner Execs each file in
-- its own transaction (cmd/api/main.go applyMigrations). Do NOT split this block or run
-- it statement-by-statement: a partial state (sandboxes already 'p' but rows still in
-- sandboxes_unpartitioned) would make the relkind='r' guard no-op and strand the data.
--
-- PARTITION-KEY INVARIANT: the PK is (cell_id, tenant_id, id). A partitioned table cannot
-- have a unique constraint that omits the partition key, so global (tenant_id, id)
-- uniqueness is NOT enforced by the schema across partitions — it is maintained by
-- convention: sandbox ids are globally unique AND a sandbox's cell_id is IMMUTABLE after
-- creation. Rebalancing a tenant to a new cell (internal/cells) does NOT move existing
-- sandboxes; they live out their TTL in their original cell. A future rebalance runbook
-- that physically moves rows MUST do so as a cross-partition migration, never by
-- re-saving the same id under a new cell (that would create a second row).

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'sandboxes' AND relkind = 'r') THEN
        ALTER TABLE sandboxes RENAME TO sandboxes_unpartitioned;
        -- Free the index names so the partitioned indexes can reuse them.
        DROP INDEX IF EXISTS idx_sandboxes_tenant_state;
        DROP INDEX IF EXISTS idx_sandboxes_cell;
        DROP INDEX IF EXISTS idx_sandboxes_state_expiry;

        CREATE TABLE sandboxes (
            id          TEXT        NOT NULL,
            tenant_id   TEXT        NOT NULL,
            cell_id     TEXT        NOT NULL DEFAULT 'cell-0',
            image       TEXT        NOT NULL DEFAULT '',
            state       TEXT        NOT NULL,
            created_at  TIMESTAMPTZ NOT NULL,
            updated_at  TIMESTAMPTZ NOT NULL,
            expires_at  TIMESTAMPTZ,
            PRIMARY KEY (cell_id, tenant_id, id)
        ) PARTITION BY LIST (cell_id);

        -- Default partition catches cell-0 and any cell without an explicit partition.
        --   CREATE TABLE sandboxes_cell_a PARTITION OF sandboxes FOR VALUES IN ('cell-a');
        CREATE TABLE sandboxes_default PARTITION OF sandboxes DEFAULT;

        -- Recreate the indexes (propagate to every partition). idx_sandboxes_cell is
        -- low-value on a LIST(cell_id) table (zero per-partition selectivity — pruning
        -- already routes cell_id predicates), but 0001 recreates it unconditionally on
        -- every boot under the re-run-all-files runner, so we recreate it here too for
        -- cross-boot consistency. Dropping it for real awaits the golang-migrate adoption
        -- (versioned, run-once) noted in migrations/embed.go.
        CREATE INDEX idx_sandboxes_tenant_state ON sandboxes (tenant_id, state);
        CREATE INDEX idx_sandboxes_cell ON sandboxes (cell_id);
        CREATE INDEX idx_sandboxes_state_expiry ON sandboxes (state, expires_at);

        -- Migrate existing rows (routed by cell_id; existing rows default to 'cell-0').
        INSERT INTO sandboxes (id, tenant_id, cell_id, image, state, created_at, updated_at, expires_at)
            SELECT id, tenant_id, cell_id, image, state, created_at, updated_at, expires_at
            FROM sandboxes_unpartitioned;

        DROP TABLE sandboxes_unpartitioned;
    END IF;
END $$;
