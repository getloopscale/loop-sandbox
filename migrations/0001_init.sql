-- 0001_init.sql — M0 walking skeleton schema.
-- Multi-tenancy is first-class from the start: every row carries tenant_id and a
-- cell_id partition key (single cell in M0), so the M1/M11 sharding seam has a home.

CREATE TABLE IF NOT EXISTS sandboxes (
    id          TEXT        NOT NULL,
    tenant_id   TEXT        NOT NULL,
    cell_id     TEXT        NOT NULL DEFAULT 'cell-0',
    image       TEXT        NOT NULL DEFAULT '',
    state       TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id)
);

-- Reconciler/TTL (M4) will scan by state/expiry; tenant listing filters by tenant.
CREATE INDEX IF NOT EXISTS idx_sandboxes_tenant_state ON sandboxes (tenant_id, state);
CREATE INDEX IF NOT EXISTS idx_sandboxes_cell ON sandboxes (cell_id);
