-- 0011_nodes.sql — M21: multi-node orchestration. Worker-node registry is the source of
-- truth for placement (no Nomad/Consul — ADR-1). Workers heartbeat their capacity + liveness;
-- the best-of-K scheduler reads the ready set of a cell to place sandboxes.

CREATE TABLE IF NOT EXISTS nodes (
    id             TEXT             NOT NULL PRIMARY KEY,
    cell_id        TEXT             NOT NULL DEFAULT 'cell-0',
    address        TEXT             NOT NULL DEFAULT '',
    cpu_count      BIGINT           NOT NULL DEFAULT 0,   -- total vCPUs
    cpu_allocated  BIGINT           NOT NULL DEFAULT 0,   -- vCPUs reserved by sandboxes
    cpu_percent    DOUBLE PRECISION NOT NULL DEFAULT 0,   -- live utilisation 0..100
    state          TEXT             NOT NULL DEFAULT 'ready', -- ready | draining | unhealthy
    last_heartbeat TIMESTAMPTZ      NOT NULL
);

-- Placement scans the ready nodes of a cell; index the candidate lookup.
CREATE INDEX IF NOT EXISTS idx_nodes_cell_state ON nodes (cell_id, state);
