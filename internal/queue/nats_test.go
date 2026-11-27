package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/getloopscale/loop-sandbox/internal/queue"
)

// embeddedNATS starts a real nats-server with JetStream enabled, in-process (no external
// broker) — the queue talks the genuine NATS+JetStream wire protocol, hermetically.
func embeddedNATS(t *testing.T) jetstream.JetStream {
	t.Helper()
	opts := &natsserver.Options{
		Port:      -1, // random free port
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}

// TestNATSQueue_enqueue_dequeue_ack proves the per-cell JobQueue over real JetStream:
// a job enqueued for a cell is delivered to that cell's consumer, then acked.
func TestNATSQueue_enqueue_dequeue_ack(t *testing.T) {
	js := embeddedNATS(t)
	ctx := context.Background()

	q, err := queue.NewNATSQueue(ctx, js, "cell-a")
	if err != nil {
		t.Fatalf("NewNATSQueue: %v", err)
	}
	want := queue.Job{ID: "job-1", CellID: "cell-a", Kind: "destroy", Payload: []byte(`{"sandbox":"s1"}`)}
	if err := q.Enqueue(ctx, want); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got, ack, err := q.Dequeue(dctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if got.ID != want.ID || got.CellID != want.CellID || got.Kind != want.Kind || string(got.Payload) != string(want.Payload) {
		t.Errorf("Dequeue = %+v, want %+v", got, want)
	}
	if err := ack(ctx); err != nil {
		t.Errorf("ack: %v", err)
	}
}

// TestNATSQueue_per_cell_isolation proves a job for cell-a is NOT delivered to cell-b's
// consumer (per-cell sharding of work distribution — B1).
func TestNATSQueue_per_cell_isolation(t *testing.T) {
	js := embeddedNATS(t)
	ctx := context.Background()

	qa, err := queue.NewNATSQueue(ctx, js, "cell-a")
	if err != nil {
		t.Fatalf("NewNATSQueue a: %v", err)
	}
	qb, err := queue.NewNATSQueue(ctx, js, "cell-b")
	if err != nil {
		t.Fatalf("NewNATSQueue b: %v", err)
	}
	if err := qa.Enqueue(ctx, queue.Job{ID: "j", CellID: "cell-a", Kind: "destroy"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// cell-b must NOT see cell-a's job — its Dequeue times out.
	bctx, bcancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer bcancel()
	if _, _, err := qb.Dequeue(bctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cell-b Dequeue err = %v, want DeadlineExceeded (isolation breached)", err)
	}

	// cell-a still gets its own job.
	actx, acancel := context.WithTimeout(ctx, 5*time.Second)
	defer acancel()
	got, ack, err := qa.Dequeue(actx)
	if err != nil {
		t.Fatalf("cell-a Dequeue: %v", err)
	}
	if got.ID != "j" {
		t.Errorf("cell-a got %q, want j", got.ID)
	}
	_ = ack(ctx)
}

// TestNATSQueue_dedups_same_job_id proves parity with the PG backend's ON CONFLICT:
// re-enqueuing the same job id within the dedup window yields ONE delivery, not two.
func TestNATSQueue_dedups_same_job_id(t *testing.T) {
	js := embeddedNATS(t)
	ctx := context.Background()
	q, err := queue.NewNATSQueue(ctx, js, "cell-a")
	if err != nil {
		t.Fatalf("NewNATSQueue: %v", err)
	}
	job := queue.Job{ID: "dupe", CellID: "cell-a", Kind: "destroy"}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatalf("Enqueue 1: %v", err)
	}
	if err := q.Enqueue(ctx, job); err != nil { // same id → deduped by JetStream
		t.Fatalf("Enqueue 2: %v", err)
	}
	// First dequeue+ack succeeds.
	d1, c1 := context.WithTimeout(ctx, 5*time.Second)
	defer c1()
	got, ack, err := q.Dequeue(d1)
	if err != nil || got.ID != "dupe" {
		t.Fatalf("Dequeue 1 = (%+v, %v), want job dupe", got, err)
	}
	_ = ack(ctx)
	// Second dequeue must find nothing — the duplicate was suppressed.
	d2, c2 := context.WithTimeout(ctx, 800*time.Millisecond)
	defer c2()
	if _, _, err := q.Dequeue(d2); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second Dequeue err = %v, want DeadlineExceeded (duplicate not suppressed)", err)
	}
}

// Negative case: an invalid cell id is rejected fail-fast (never silently normalized
// onto a colliding subject).
func TestNewNATSQueue_rejects_invalid_cell_id(t *testing.T) {
	js := embeddedNATS(t)
	for _, bad := range []string{"", "a.b", "a b", "a*", "a>", "a/b"} {
		if _, err := queue.NewNATSQueue(context.Background(), js, bad); err == nil {
			t.Errorf("NewNATSQueue(%q) accepted an invalid cell id", bad)
		}
	}
}

// TestNATSQueue_at_least_once_redelivery proves an un-acked job is redelivered (crash
// safety) — the consumer contract is at-least-once, so consumers must be idempotent.
func TestNATSQueue_at_least_once_redelivery(t *testing.T) {
	js := embeddedNATS(t)
	ctx := context.Background()
	q, err := queue.NewNATSQueue(ctx, js, "cell-a")
	if err != nil {
		t.Fatalf("NewNATSQueue: %v", err)
	}
	if err := q.Enqueue(ctx, queue.Job{ID: "j", CellID: "cell-a", Kind: "destroy"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// First delivery: do NOT ack (simulate a crash) — the job must return.
	d1, cancel1 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel1()
	if _, _, err := q.Dequeue(d1); err != nil {
		t.Fatalf("first Dequeue: %v", err)
	}
	// Redelivery after the ack-wait window: the same job comes back.
	d2, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	got, ack, err := q.Dequeue(d2)
	if err != nil {
		t.Fatalf("redelivery Dequeue: %v", err)
	}
	if got.ID != "j" {
		t.Errorf("redelivered %q, want j", got.ID)
	}
	_ = ack(ctx)
}
