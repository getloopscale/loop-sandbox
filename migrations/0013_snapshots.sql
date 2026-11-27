-- 0013_snapshots.sql: named snapshots of a sandbox, from which new sandboxes start.
-- The record is tenant-scoped; the memory and disk state live in the storage of the
-- runtime driver, keyed by id.

CREATE TABLE IF NOT EXISTS snapshots (
    id         TEXT        NOT NULL,
    tenant_id  TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    sandbox_id TEXT        NOT NULL,
    image      TEXT        NOT NULL DEFAULT '',
    metadata   JSONB       NOT NULL DEFAULT '{}'::jsonb,
    env        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    size_bytes BIGINT      NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
-- A snapshot name is unique within a tenant.
CREATE UNIQUE INDEX IF NOT EXISTS idx_snapshots_name ON snapshots (tenant_id, name);
