package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// notifyChannel is the LISTEN/NOTIFY channel producers signal on.
const notifyChannel = "lsb_jobs"

// safetyPoll bounds how long Dequeue waits on a NOTIFY before re-checking. A NOTIFY
// wakes the consumer immediately; this backstop bounds worst-case teardown latency
// to safetyPoll only in the rare case a NOTIFY is missed (it is not a busy-loop —
// it fires at most once per second while the queue is idle).
const safetyPoll = time.Second

// defaultReclaimAfter is how long a job may sit 'claimed' before a crashed consumer
// is assumed and the job is reclaimed to 'pending' (at-least-once delivery).
const defaultReclaimAfter = time.Minute

// PGQueue is the PostgreSQL push queue.
type PGQueue struct {
	pool *pgxpool.Pool
	ids  func() string
}

// NewPG builds a PostgreSQL job queue. ids generates opaque job ids.
func NewPG(pool *pgxpool.Pool, ids func() string) *PGQueue {
	return &PGQueue{pool: pool, ids: ids}
}

// Enqueue inserts the job and signals waiting consumers via pg_notify, atomically
// in one transaction. It is idempotent on job.ID (ON CONFLICT DO NOTHING) so a
// producer may safely re-enqueue the same logical job across ticks; a duplicate
// neither inserts a row nor fires a NOTIFY.
func (q *PGQueue) Enqueue(ctx context.Context, job Job) error {
	if job.ID == "" {
		job.ID = q.ids()
	}
	if job.CellID == "" {
		job.CellID = "cell-0"
	}
	payload := job.Payload
	if payload == nil {
		payload = []byte("{}")
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("enqueue begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Conflict target is the partitioned PK (cell_id, id) — a partitioned table's unique
	// constraint must include the partition key. A job's cell is fixed, so re-enqueuing
	// the same job id still dedups (same cell_id,id pair).
	const ins = `INSERT INTO jobs (id, cell_id, kind, payload, status, created_at)
		VALUES ($1,$2,$3,$4,'pending', now())
		ON CONFLICT (cell_id, id) DO NOTHING`
	tag, err := tx.Exec(ctx, ins, job.ID, job.CellID, job.Kind, string(payload))
	if err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return tx.Commit(ctx) // duplicate — no new work, no NOTIFY
	}
	if _, err := tx.Exec(ctx, "SELECT pg_notify($1,$2)", notifyChannel, job.ID); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return tx.Commit(ctx)
}

// ReclaimStuck resets jobs stuck in 'claimed' longer than olderThan back to
// 'pending' — recovering work abandoned by a crashed consumer (at-least-once). It
// returns the number reclaimed. Callers run it on the reconciler ticker.
func (q *PGQueue) ReclaimStuck(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		olderThan = defaultReclaimAfter
	}
	const rq = `UPDATE jobs SET status='pending', claimed_at=NULL
		WHERE status='claimed' AND claimed_at < now() - $1::interval`
	tag, err := q.pool.Exec(ctx, rq, olderThan.String())
	if err != nil {
		return 0, fmt.Errorf("reclaim stuck: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Dequeue claims the next pending job (SKIP LOCKED) or blocks on a NOTIFY wakeup.
func (q *PGQueue) Dequeue(ctx context.Context) (Job, AckFunc, error) {
	// A dedicated connection holds the LISTEN registration.
	conn, err := q.pool.Acquire(ctx)
	if err != nil {
		return Job{}, nil, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return Job{}, nil, fmt.Errorf("listen: %w", err)
	}

	for {
		job, ok, err := q.claim(ctx)
		if err != nil {
			return Job{}, nil, err
		}
		if ok {
			return job, q.ackFunc(job.ID), nil
		}
		// No job now — wait for a NOTIFY (push) or the safety timeout, then retry.
		wctx, cancel := context.WithTimeout(ctx, safetyPoll)
		_, werr := conn.Conn().WaitForNotification(wctx)
		cancel()
		if werr != nil && ctx.Err() != nil {
			return Job{}, nil, ctx.Err() // caller cancelled
		}
		// werr may be the safety timeout — loop and re-claim regardless.
	}
}

// claim atomically takes one pending job using FOR UPDATE SKIP LOCKED so concurrent
// consumers never get the same job.
//
// INVARIANT: job ids MUST be globally unique regardless of cell. The partitioned PK is
// (cell_id, id), which alone would permit the same id under two cells; claim/ack/reclaim
// address rows by id only (cross-partition), so a cross-cell id collision would target
// the wrong row. Producers satisfy this: worker ids are `job_`+uuid, and the reconciler
// keys destroys by the globally-unique sandbox id.
func (q *PGQueue) claim(ctx context.Context) (Job, bool, error) {
	const cl = `
		UPDATE jobs SET status='claimed', claimed_at=now(), attempts=attempts+1
		WHERE id = (
			SELECT id FROM jobs WHERE status='pending'
			ORDER BY created_at, id FOR UPDATE SKIP LOCKED LIMIT 1
		)
		RETURNING id, cell_id, kind, payload`
	var j Job
	var payload string
	err := q.pool.QueryRow(ctx, cl).Scan(&j.ID, &j.CellID, &j.Kind, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil // queue empty
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("claim: %w", err)
	}
	j.Payload = []byte(payload)
	return j, true, nil
}

func (q *PGQueue) ackFunc(id string) AckFunc {
	return func(ctx context.Context) error {
		if _, err := q.pool.Exec(ctx, "DELETE FROM jobs WHERE id=$1", id); err != nil {
			return fmt.Errorf("ack: %w", err)
		}
		return nil
	}
}
