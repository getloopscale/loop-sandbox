-- 0010_sandbox_gpu.sql — M19: mark GPU-backed sandboxes. Non-empty gpu → scheduled onto the
-- isolated GPU cell and metered on the premium GPU tier.

ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS gpu TEXT NOT NULL DEFAULT '';
