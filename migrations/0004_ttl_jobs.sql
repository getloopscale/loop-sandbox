-- 0004_ttl_jobs.sql — M4: mandatory TTL + push job queue.

ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;

-- The reconciler scans non-destroyed sandboxes by expiry; index it (shardable by cell).
CREATE INDEX IF NOT EXISTS idx_sandboxes_state_expiry ON sandboxes (state, expires_at);

-- Push job queue (TRD §9.3): claimed via FOR UPDATE SKIP LOCKED; producers pg_notify
-- so consumers wake without polling. Consumers are idempotent (at-least-once).
CREATE TABLE IF NOT EXISTS jobs (
    id          TEXT        NOT NULL PRIMARY KEY,
    cell_id     TEXT        NOT NULL DEFAULT 'cell-0',
    kind        TEXT        NOT NULL,
    payload     JSONB       NOT NULL DEFAULT '{}',
    status      TEXT        NOT NULL DEFAULT 'pending',
    attempts    INT         NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL,
    claimed_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_jobs_pending ON jobs (status, created_at) WHERE status = 'pending';
