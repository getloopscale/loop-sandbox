// Package metering emits per-tenant usage to LoopScale billing OFF the request hot path,
// batched and idempotently. The design is a claim-and-delete outbox: usage events
// are appended (by the cost path) to an append-only table in the SAME transaction as
// the counter bump; a drainer CLAIMS a batch under a row lock (FOR UPDATE SKIP
// LOCKED), emits it to the sink, and DELETES the rows in the same transaction — the
// delete IS the acknowledgement. This is deliberately NOT a high-water cursor: a
// BIGSERIAL id committed out of order (a slow transaction committing after a later
// one) would be skipped forever by a `id > cursor` scan and silently lost. Claiming
// live rows and deleting only on success gives:
//   - no LOSS: an un-emitted row is never removed, so it is re-claimed on a later pass;
//   - no CONCURRENT double-drain: SKIP LOCKED hands each row to exactly one drainer;
//   - at-least-once delivery: a crash after emit but before commit re-claims the rows,
//     so the sink MUST dedupe by EventID (idempotent sink) — the last-mile guarantee.
package metering

import "context"

// MeterEvent is one appended usage record (from the cost path).
type MeterEvent struct {
	ID       int64
	TenantID string
	Metric   string
	Amount   int64
	Period   string // "2026-07"
}

// MeterInput is the billing payload sent to the sink. EventID lets the sink dedupe.
type MeterInput struct {
	EventID  int64
	TenantID string
	Metric   string
	Amount   int64
	Period   string
}

// Sink delivers a batch to LoopScale billing. It MUST be idempotent per EventID (a repeat
// EventID is a no-op) because the outbox is at-least-once across a crash window.
type Sink interface {
	Emit(ctx context.Context, batch []MeterInput) error
}

// Outbox claims a batch of pending events under a lock, hands them to sink, and — only
// if the sink returns nil — removes them, atomically. It returns the number drained.
// SKIP LOCKED semantics let multiple drainers run without double-delivery.
type Outbox interface {
	Drain(ctx context.Context, limit int, sink Sink) (int, error)
}

// Emitter repeatedly drains the outbox until it is caught up. Its RunOnce is one
// claim-emit-delete cycle; Drain loops until empty.
type Emitter struct {
	outbox    Outbox
	sink      Sink
	batchSize int
}

// NewEmitter wires the emitter. batchSize <= 0 defaults to 100.
func NewEmitter(outbox Outbox, sink Sink, batchSize int) *Emitter {
	if batchSize <= 0 {
		batchSize = 100
	}
	return &Emitter{outbox: outbox, sink: sink, batchSize: batchSize}
}

// RunOnce drains at most one batch.
func (e *Emitter) RunOnce(ctx context.Context) (int, error) {
	return e.outbox.Drain(ctx, e.batchSize, e.sink)
}

// Drain emits batches until the outbox is caught up (or an error occurs).
func (e *Emitter) Drain(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := e.RunOnce(ctx)
		if err != nil {
			return total, err
		}
		total += n
		if n == 0 {
			return total, nil
		}
	}
}

// ToInputs maps claimed events to billing inputs (shared by outbox implementations).
func ToInputs(events []MeterEvent) []MeterInput {
	batch := make([]MeterInput, len(events))
	for i, ev := range events {
		batch[i] = MeterInput{EventID: ev.ID, TenantID: ev.TenantID, Metric: ev.Metric, Amount: ev.Amount, Period: ev.Period}
	}
	return batch
}
