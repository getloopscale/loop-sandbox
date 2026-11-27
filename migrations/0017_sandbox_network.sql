-- 0017_sandbox_network.sql: the outbound rules of a sandbox.
--
-- Both lists are empty for a sandbox with the default policy. They are written
-- at creation and replaced by PUT /v1/sandboxes/{id}/network.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS network_allow_out JSONB NOT NULL DEFAULT '[]';
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS network_deny_out JSONB NOT NULL DEFAULT '[]';
