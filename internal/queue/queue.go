// Package queue is the JobQueue seam: a push queue (never poll). The MVP/OSS
// implementation is PostgreSQL (FOR UPDATE SKIP LOCKED for the claim, LISTEN/
// NOTIFY for the wakeup); a NATS JetStream implementation replaces it per cell at
// scale (M11) behind the same interface.
package queue

import "context"

// Job is a unit of work (e.g. a sandbox destroy). Payload is opaque JSON.
type Job struct {
	ID      string
	CellID  string
	Kind    string
	Payload []byte
}

// AckFunc finalizes a claimed job (removes it). Not calling it (crash) leaves the
// job reclaimable — at-least-once delivery, so consumers MUST be idempotent.
type AckFunc func(ctx context.Context) error

// JobQueue is push-based: Dequeue blocks until work arrives or ctx is cancelled.
type JobQueue interface {
	// Enqueue adds a job and wakes a waiting consumer (no polling).
	Enqueue(ctx context.Context, job Job) error
	// Dequeue claims the next pending job (exclusive across consumers via SKIP
	// LOCKED) and returns it with an ack. It blocks on a push signal until a job is
	// available or ctx is done (context.Canceled).
	Dequeue(ctx context.Context) (Job, AckFunc, error)
}
