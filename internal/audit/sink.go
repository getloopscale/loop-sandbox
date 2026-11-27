// Package audit provides an asynchronous audit sink so recording a mutation never
// blocks the request hot path (M1 DoD). Events are buffered and drained by a
// background worker into the AuditStore. A crash before flush may lose recent
// events — acceptable in M1; durable delivery is an M11 concern. Close() drains.
package audit

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/google/uuid"
)

// Sink buffers audit events and flushes them asynchronously.
type Sink struct {
	store   domain.AuditStore
	events  chan *domain.AuditEvent
	logger  *slog.Logger
	wg      sync.WaitGroup
	dropped int64
	mu      sync.Mutex
}

// NewSink starts the background worker. buffer bounds memory; when full, Emit
// drops (and counts) rather than blocking the caller.
func NewSink(store domain.AuditStore, buffer int, logger *slog.Logger) *Sink {
	if buffer <= 0 {
		buffer = 256
	}
	s := &Sink{store: store, events: make(chan *domain.AuditEvent, buffer), logger: logger}
	s.wg.Add(1)
	go s.run()
	return s
}

// Record builds an audit event (assigning a fresh id) and enqueues it. This is
// the app.AuditRecorder seam: use cases call it after a successful mutation.
// A malformed event (missing tenant/action) is dropped with a warning rather than
// failing the caller — audit must never break the operation it records.
func (s *Sink) Record(tenantID, actorID, action, resourceType, resourceID, requestID string, now time.Time) {
	e, err := domain.NewAuditEvent("aud_"+uuid.NewString(), tenantID, actorID, action, resourceType, resourceID, requestID, now)
	if err != nil {
		s.logger.Warn("audit event invalid, not recorded", "error", err, "action", action)
		return
	}
	s.Emit(e)
}

// Emit enqueues an event without blocking. If the buffer is full it drops the
// event (counted) — auditing must never stall the request path.
func (s *Sink) Emit(e *domain.AuditEvent) {
	select {
	case s.events <- e:
	default:
		s.mu.Lock()
		s.dropped++
		total := s.dropped
		s.mu.Unlock()
		// Error, not Warn: a dropped audit event is a compliance hole. Dropped() is
		// exported so the M5 observability baseline can wire it to a Prometheus counter.
		s.logger.Error("audit event dropped: buffer full (audit gap)", "action", e.Action, "tenant", e.TenantID, "dropped_total", total)
	}
}

// Dropped returns the count of dropped events (observability for backpressure).
func (s *Sink) Dropped() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

func (s *Sink) run() {
	defer s.wg.Done()
	for e := range s.events {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.store.Append(ctx, e); err != nil {
			s.logger.Error("audit append failed", "error", err, "action", e.Action)
		}
		cancel()
	}
}

// Close stops accepting events and drains the buffer.
func (s *Sink) Close() {
	close(s.events)
	s.wg.Wait()
}
