-- 0006_partition_jobs.sql — M11 B2: partition the job queue by cell_id.
--
-- The jobs table is the highest-churn cell-aligned table (every TTL destroy writes a
-- row). Partitioning it BY LIST (cell_id) distributes writes/index maintenance across
-- one partition per cell instead of a single hot heap+index — the single PostgreSQL
-- writer bottleneck (B2). jobs has NO inbound foreign keys, so it partitions cleanly
-- (unlike sandboxes, whose FK from executions would cascade — deferred).
--
-- PostgreSQL cannot convert a plain table to partitioned in place, so we rename the old
-- table, create the partitioned one, copy rows, and drop the old. The partition key
-- (cell_id) must be part of the primary key, so the PK becomes (cell_id, id).
--
-- IDEMPOTENT: this project's migration runner re-applies every file on each boot (no
-- version tracking yet), so the whole conversion is guarded to run ONLY while `jobs` is
-- still an ordinary table (relkind 'r'). Once it is partitioned (relkind 'p'), the block
-- is a no-op — a second boot does not error.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'jobs' AND relkind = 'r') THEN
        ALTER TABLE jobs RENAME TO jobs_unpartitioned;
        -- Free the index name so the partitioned index can reuse it.
        DROP INDEX IF EXISTS idx_jobs_pending;

        CREATE TABLE jobs (
            id          TEXT        NOT NULL,
            cell_id     TEXT        NOT NULL DEFAULT 'cell-0',
            kind        TEXT        NOT NULL,
            payload     JSONB       NOT NULL DEFAULT '{}',
            status      TEXT        NOT NULL DEFAULT 'pending',
            attempts    INT         NOT NULL DEFAULT 0,
            created_at  TIMESTAMPTZ NOT NULL,
            claimed_at  TIMESTAMPTZ,
            PRIMARY KEY (cell_id, id)
        ) PARTITION BY LIST (cell_id);

        -- Default partition catches cell-0 and any cell without an explicit partition.
        -- Per-cell partitions are attached as cells are provisioned:
        --   CREATE TABLE jobs_cell_a PARTITION OF jobs FOR VALUES IN ('cell-a');
        CREATE TABLE jobs_default PARTITION OF jobs DEFAULT;

        -- Partitioned index — propagates to every partition (pending-scan for claim).
        CREATE INDEX idx_jobs_pending ON jobs (status, created_at) WHERE status = 'pending';

        -- Migrate any existing rows into the partitioned table (routed by cell_id).
        INSERT INTO jobs (id, cell_id, kind, payload, status, attempts, created_at, claimed_at)
            SELECT id, cell_id, kind, payload, status, attempts, created_at, claimed_at
            FROM jobs_unpartitioned;

        DROP TABLE jobs_unpartitioned;
    END IF;
END $$;
