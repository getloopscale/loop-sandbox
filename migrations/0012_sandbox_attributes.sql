-- 0012_sandbox_attributes.sql: caller-supplied attributes of a sandbox.
--
-- metadata holds labels returned as given and usable as list filters; env is the
-- default environment of every command run in the sandbox. Both are set at creation.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS env JSONB NOT NULL DEFAULT '{}';

-- Serves GET /v1/sandboxes: one tenant's sandboxes, newest first, by keyset.
CREATE INDEX IF NOT EXISTS idx_sandboxes_tenant_created ON sandboxes (tenant_id, created_at DESC, id DESC);
