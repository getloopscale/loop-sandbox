-- 0009_template_registry.sql — M17: extend templates into a versioned, digest-pinned,
-- scan-gated registry. A public template is visible to all tenants; a template may only be
-- published public after it passes the vulnerability scan gate.

ALTER TABLE templates ADD COLUMN IF NOT EXISTS version     TEXT    NOT NULL DEFAULT '1';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS digest      TEXT    NOT NULL DEFAULT '';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS public      BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS scan_status TEXT    NOT NULL DEFAULT 'unscanned';

-- List a tenant's own templates + all public ones (the registry read path).
CREATE INDEX IF NOT EXISTS idx_templates_public ON templates (public);
