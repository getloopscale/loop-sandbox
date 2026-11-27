-- 0014_sandbox_auto_pause.sql: a sandbox that pauses itself when idle.
--
-- auto_pause_seconds is the idle limit set at creation (0: never). last_active_at
-- is the last time a request used the sandbox. auto_paused tells a pause made by
-- the idle loop, which the next request undoes, from one the caller asked for.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS auto_pause_seconds BIGINT NOT NULL DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS last_active_at TIMESTAMPTZ;
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS auto_paused BOOLEAN NOT NULL DEFAULT FALSE;

-- Serves the idle loop: running sandboxes with an idle limit, least recently used first.
CREATE INDEX IF NOT EXISTS idx_sandboxes_idle ON sandboxes (last_active_at)
    WHERE state = 'running' AND auto_pause_seconds > 0;
