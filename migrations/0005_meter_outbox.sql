-- M8: metering outbox. Usage increments append an append-only meter_event in the
-- SAME transaction (atomic outbox), drained asynchronously by the worker with a
-- claim-and-delete pass (FOR UPDATE SKIP LOCKED → emit → DELETE in one tx). This is
-- deliberately NOT a high-water cursor: a BIGSERIAL id committed out of order would
-- be skipped forever by an `id > cursor` scan and silently lost (unbilled). Claiming
-- live rows and deleting only on emit success gives no-loss + no concurrent
-- double-drain; a crash after emit re-claims the rows (at-least-once → the sink
-- dedupes by id). The PRIMARY KEY index serves the `ORDER BY id` claim — no extra idx.

CREATE TABLE IF NOT EXISTS meter_events (
    id         BIGSERIAL PRIMARY KEY,
    tenant_id  TEXT   NOT NULL,
    metric     TEXT   NOT NULL,
    amount     BIGINT NOT NULL,
    period     TEXT   NOT NULL,          -- "2026-07"
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
