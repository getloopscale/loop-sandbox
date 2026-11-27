-- 0015_secrets.sql: named secrets of a tenant, and the secrets a sandbox receives.
--
-- value is the ciphertext (AES-256-GCM, bound to the tenant and the name); key_id
-- names the key that sealed it. The plaintext is never stored.
CREATE TABLE IF NOT EXISTS secrets (
    tenant_id  TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    value      BYTEA       NOT NULL,
    key_id     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, name)
);

-- The names of the secrets a sandbox receives as environment variables. Names
-- only: the values are read from the table above when a command runs.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS secrets JSONB NOT NULL DEFAULT '[]';
