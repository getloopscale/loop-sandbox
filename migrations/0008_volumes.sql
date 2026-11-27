-- 0008_volumes.sql — M16: durable named volumes (persistent sessions tier).
-- A volume's data survives sandbox destroy and can be re-attached to a new sandbox.
-- Tenant-scoped + cell-pinned (B2 partition model); sandbox_id is NULL/'' when detached.

CREATE TABLE IF NOT EXISTS volumes (
    id         TEXT        NOT NULL,
    tenant_id  TEXT        NOT NULL,
    cell_id    TEXT        NOT NULL DEFAULT 'cell-0',
    name       TEXT        NOT NULL,
    size_gb    BIGINT      NOT NULL,
    sandbox_id TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
-- List a sandbox's attached volumes (tenant-scoped).
CREATE INDEX IF NOT EXISTS idx_volumes_sandbox ON volumes (tenant_id, sandbox_id);
-- Volume names are unique per tenant (named-volume model): re-attaching by name resolves to
-- the SAME durable backing, so a volume's data survives sandbox destroy + re-attach.
CREATE UNIQUE INDEX IF NOT EXISTS idx_volumes_name ON volumes (tenant_id, name);
