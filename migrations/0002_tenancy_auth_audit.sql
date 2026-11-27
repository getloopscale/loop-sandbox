-- 0002_tenancy_auth_audit.sql — M1: tenants, API keys, audit, idempotency, quota.

CREATE TABLE IF NOT EXISTS tenants (
    id         TEXT        NOT NULL PRIMARY KEY,
    name       TEXT        NOT NULL,
    plan       TEXT        NOT NULL DEFAULT 'starter',
    status     TEXT        NOT NULL DEFAULT 'active',
    cell_id    TEXT        NOT NULL DEFAULT 'cell-0',
    created_at TIMESTAMPTZ NOT NULL
);

-- Only the SHA-256 hash of the key is stored (TRD §5.2). Lookup is by key_hash.
CREATE TABLE IF NOT EXISTS api_keys (
    id         TEXT        NOT NULL PRIMARY KEY,
    tenant_id  TEXT        NOT NULL REFERENCES tenants(id),
    name       TEXT        NOT NULL DEFAULT '',
    prefix     TEXT        NOT NULL,
    key_hash   TEXT        NOT NULL UNIQUE,
    scopes     TEXT[]      NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys (key_hash);
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys (tenant_id);

CREATE TABLE IF NOT EXISTS audit_events (
    id            TEXT        NOT NULL PRIMARY KEY,
    tenant_id     TEXT        NOT NULL,
    actor_type    TEXT        NOT NULL,
    actor_id      TEXT        NOT NULL,
    action        TEXT        NOT NULL,
    resource_type TEXT        NOT NULL,
    resource_id   TEXT        NOT NULL,
    request_id    TEXT        NOT NULL,
    ts            TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_tenant_ts ON audit_events (tenant_id, ts);

-- Idempotency (TRD §6.3): reserve-before-act. A row is inserted (pending) before
-- the resource is created; the unique PK serializes concurrent same-key requests.
-- same key + same payload → replay; different payload → conflict; concurrent → in-flight.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id       TEXT        NOT NULL,
    key             TEXT        NOT NULL,
    request_hash    TEXT        NOT NULL,
    completed       BOOLEAN     NOT NULL DEFAULT false,
    response_status INT         NOT NULL DEFAULT 0,
    response_body   BYTEA       NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, key)
);

CREATE TABLE IF NOT EXISTS cost_counters (
    tenant_id    TEXT   NOT NULL,
    metric       TEXT   NOT NULL,
    period_start BIGINT NOT NULL,
    period_end   BIGINT NOT NULL,
    value        BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, metric, period_start)
);
