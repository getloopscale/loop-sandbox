package metering_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/metering"
)

// memOutbox is an in-memory claim-and-delete outbox mirroring the postgres semantics:
// Drain claims up to limit rows, emits, and deletes them ONLY on emit success. A
// commitFailN option simulates a crash AFTER a successful emit but BEFORE the delete
// commits (rows survive → re-claimed → re-emitted → sink must dedupe).
type memOutbox struct {
	mu          sync.Mutex
	events      []metering.MeterEvent
	commitFailN int // fail the delete-commit for the first N drains (post-emit)
	drains      int
}

func (o *memOutbox) append(n int) {
	for i := 1; i <= n; i++ {
		o.events = append(o.events, metering.MeterEvent{ID: int64(i), TenantID: "t", Metric: "sandbox_creations", Amount: 1, Period: "2026-07"})
	}
}

func (o *memOutbox) Drain(ctx context.Context, limit int, sink metering.Sink) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drains++
	if len(o.events) == 0 {
		return 0, nil
	}
	n := limit
	if n > len(o.events) {
		n = len(o.events)
	}
	claimed := o.events[:n]
	if err := sink.Emit(ctx, metering.ToInputs(claimed)); err != nil {
		return 0, err // rows NOT deleted (tx rollback)
	}
	if o.drains <= o.commitFailN {
		// emit succeeded but the delete-commit "failed" → rows survive (at-least-once).
		return 0, errors.New("commit failed after emit")
	}
	o.events = o.events[n:] // delete claimed rows
	return n, nil
}

// recSink records EventID delivery counts; dedupe-by-id models an idempotent sink.
type recSink struct {
	mu   sync.Mutex
	seen map[int64]int
	fail int
	call int
}

func newRecSink() *recSink { return &recSink{seen: map[int64]int{}} }

func (s *recSink) Emit(_ context.Context, batch []metering.MeterInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.call++
	if s.call <= s.fail {
		return errors.New("sink down")
	}
	for _, m := range batch {
		s.seen[m.EventID]++
	}
	return nil
}

func (s *recSink) distinct() int { return len(s.seen) }
func (s *recSink) maxDeliveries() int {
	m := 0
	for _, c := range s.seen {
		if c > m {
			m = c
		}
	}
	return m
}

func TestEmitter_drains_all(t *testing.T) {
	ob := &memOutbox{}
	ob.append(5)
	sink := newRecSink()
	total, err := metering.NewEmitter(ob, sink, 2).Drain(context.Background())
	if err != nil || total != 5 {
		t.Fatalf("Drain = %d (err %v), want 5", total, err)
	}
	if sink.distinct() != 5 || sink.maxDeliveries() != 1 {
		t.Errorf("distinct=%d max=%d, want 5/1", sink.distinct(), sink.maxDeliveries())
	}
	if len(ob.events) != 0 {
		t.Errorf("outbox not emptied: %d left", len(ob.events))
	}
}

// A SINK failure leaves the rows intact (nothing delivered) and the retry succeeds.
func TestEmitter_sink_failure_no_delete_then_retry(t *testing.T) {
	ob := &memOutbox{}
	ob.append(3)
	sink := newRecSink()
	sink.fail = 1
	e := metering.NewEmitter(ob, sink, 10)
	if _, err := e.RunOnce(context.Background()); err == nil {
		t.Fatal("expected sink error")
	}
	if len(ob.events) != 3 {
		t.Errorf("rows deleted despite sink failure: %d left, want 3", len(ob.events))
	}
	total, err := e.Drain(context.Background())
	if err != nil || total != 3 {
		t.Fatalf("retry = %d (err %v), want 3", total, err)
	}
	if sink.maxDeliveries() != 1 {
		t.Errorf("max deliveries = %d, want 1", sink.maxDeliveries())
	}
}

// The dangerous path: emit SUCCEEDS, then the delete-commit FAILS → the rows survive
// and are re-claimed → the same EventIDs are re-emitted. An idempotent sink dedupes,
// so no double count. This is the at-least-once contract the outbox depends on.
func TestEmitter_commit_failure_after_emit_reemits_and_sink_dedupes(t *testing.T) {
	ob := &memOutbox{commitFailN: 1}
	ob.append(2)
	sink := newRecSink()
	e := metering.NewEmitter(ob, sink, 10)

	// First run: emit ok, commit fails → error, rows still present.
	if _, err := e.RunOnce(context.Background()); err == nil {
		t.Fatal("expected commit error after emit")
	}
	if len(ob.events) != 2 {
		t.Errorf("rows removed despite commit failure: %d, want 2", len(ob.events))
	}
	// Retry: re-emits the SAME ids (sink saw them twice) then deletes.
	if _, err := e.Drain(context.Background()); err != nil {
		t.Fatalf("retry drain: %v", err)
	}
	if sink.distinct() != 2 {
		t.Errorf("distinct = %d, want 2", sink.distinct())
	}
	if sink.maxDeliveries() != 2 {
		t.Errorf("max deliveries = %d, want 2 (re-emitted once); idempotent sink dedupes downstream", sink.maxDeliveries())
	}
}
