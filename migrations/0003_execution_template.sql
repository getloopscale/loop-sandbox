-- 0003_execution_template.sql — M1: minimal, forward-compatible persistence for
-- Execution and Template. Their behavior lands in M3 (exec) and the template
-- registry milestone; the schema here is intentionally small to avoid rework.

CREATE TABLE IF NOT EXISTS executions (
    id         TEXT        NOT NULL,
    sandbox_id TEXT        NOT NULL,
    tenant_id  TEXT        NOT NULL,
    status     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_executions_sandbox ON executions (tenant_id, sandbox_id);

CREATE TABLE IF NOT EXISTS templates (
    id         TEXT        NOT NULL,
    tenant_id  TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    image_ref  TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
