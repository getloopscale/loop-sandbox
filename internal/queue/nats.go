package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// validCellID is a single NATS subject token: no dots/wildcards/spaces/slashes, so two
// distinct cell ids can never collide onto the same subject/durable.
var validCellID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// streamName is the single JetStream stream holding all cells' jobs; per-cell isolation
// comes from the subject (jobs.<cellID>) + a per-cell durable consumer filtered to it.
const streamName = "LSB_JOBS"

// ackWait is how long JetStream waits for an ack before redelivering (crash safety /
// at-least-once). Kept short so a crashed consumer's job returns quickly.
const ackWait = 3 * time.Second

// NATSQueue is the JobQueue backed by NATS JetStream, scoped to ONE cell (M11 B1 —
// "NATS JetStream por cell"). Each cell has its own durable pull consumer over its own
// subject, so a cell only ever sees its own work. Delivery is at-least-once with
// explicit ack; consumers MUST be idempotent (per the JobQueue contract).
type NATSQueue struct {
	js       jetstream.JetStream
	cellID   string
	consumer jetstream.Consumer
}

// Option configures a NATSQueue at construction.
type Option func(*config)

type config struct{ replicas int }

// WithReplicas sets the JetStream stream replica count (1 = single node; 3/5 for an HA
// cluster so a node failure does not lose the queue). Clamped to [1,5] (JetStream max).
func WithReplicas(n int) Option {
	return func(c *config) {
		if n < 1 {
			n = 1
		}
		if n > 5 {
			n = 5
		}
		c.replicas = n
	}
}

// NewNATSQueue ensures the shared work-queue stream and this cell's durable consumer,
// then returns a queue bound to the cell.
func NewNATSQueue(ctx context.Context, js jetstream.JetStream, cellID string, opts ...Option) (*NATSQueue, error) {
	if !validCellID.MatchString(cellID) {
		// Fail-fast: a normalized-away cell id could silently merge two cells' queues.
		return nil, fmt.Errorf("queue: invalid cell id %q (want [A-Za-z0-9_-]+)", cellID)
	}
	cfg := config{replicas: 1}
	for _, o := range opts {
		o(&cfg)
	}
	// WorkQueue retention: a message is removed once acked by its (single) consumer —
	// exactly the semantics of a job queue. Per-cell filter subjects never overlap.
	// Replicas > 1 keeps the queue available across a node failure (RAFT).
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"jobs.>"},
		Retention: jetstream.WorkQueuePolicy,
		Replicas:  cfg.replicas,
	}); err != nil {
		return nil, fmt.Errorf("queue: ensure stream: %w", err)
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Durable:       "cell_" + cellID,
		FilterSubject: subjectFor(cellID),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
	})
	if err != nil {
		return nil, fmt.Errorf("queue: ensure consumer: %w", err)
	}
	return &NATSQueue{js: js, cellID: cellID, consumer: cons}, nil
}

// Enqueue publishes the job to its cell's subject and wakes that cell's consumer.
func (q *NATSQueue) Enqueue(ctx context.Context, job Job) error {
	cell := job.CellID
	if cell == "" {
		cell = q.cellID
	}
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("queue: marshal job: %w", err)
	}
	// WithMsgID engages JetStream's dedup window keyed on the job id — parity with the
	// PG backend's INSERT ... ON CONFLICT (id) DO NOTHING, so a producer that re-enqueues
	// the same logical job each interval does not pile up duplicates.
	var opts []jetstream.PublishOpt
	if job.ID != "" {
		opts = append(opts, jetstream.WithMsgID(job.ID))
	}
	if _, err := q.js.Publish(ctx, subjectFor(cell), data, opts...); err != nil {
		return fmt.Errorf("queue: publish: %w", err)
	}
	return nil
}

// Dequeue blocks until a job for this cell arrives or ctx is done. It pulls one message
// at a time (exclusive delivery) and returns an ack that finalizes it.
func (q *NATSQueue) Dequeue(ctx context.Context) (Job, AckFunc, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Job{}, nil, err
		}
		wait := time.Second
		if dl, ok := ctx.Deadline(); ok {
			if r := time.Until(dl); r < wait {
				wait = r
			}
		}
		if wait <= 0 {
			return Job{}, nil, context.DeadlineExceeded
		}
		batch, err := q.consumer.Fetch(1, jetstream.FetchMaxWait(wait))
		if err != nil {
			return Job{}, nil, fmt.Errorf("queue: fetch: %w", err)
		}
		var msg jetstream.Msg
		for m := range batch.Messages() {
			msg = m
		}
		if err := batch.Error(); err != nil {
			return Job{}, nil, fmt.Errorf("queue: batch: %w", err)
		}
		if msg == nil {
			continue // no job this round; loop re-checks ctx (blocks until work or done)
		}
		var job Job
		if err := json.Unmarshal(msg.Data(), &job); err != nil {
			// Poison message: terminate so it is not redelivered forever, and surface it.
			_ = msg.Term()
			return Job{}, nil, fmt.Errorf("queue: unmarshal job: %w", err)
		}
		ack := func(ctx context.Context) error { return msg.DoubleAck(ctx) }
		return job, ack, nil
	}
}

func subjectFor(cellID string) string { return "jobs." + cellID }
