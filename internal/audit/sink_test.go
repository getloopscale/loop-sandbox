package audit_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/audit"
	"github.com/getloopscale/loop-sandbox/internal/domain"
)

type memStore struct {
	mu     sync.Mutex
	events []*domain.AuditEvent
	block  chan struct{} // when non-nil, Append blocks until closed
}

func (m *memStore) Append(_ context.Context, e *domain.AuditEvent) error {
	if m.block != nil {
		<-m.block
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}
func (m *memStore) count() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.events) }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSink_drains_on_close(t *testing.T) {
	store := &memStore{}
	s := audit.NewSink(store, 16, quietLogger())
	for i := 0; i < 5; i++ {
		s.Record("tenant-a", "k1", "sandbox.create", "sandbox", "s1", "r1", time.Unix(0, 0))
	}
	s.Close() // must block until the buffer is fully flushed
	if store.count() != 5 {
		t.Errorf("flushed = %d, want 5", store.count())
	}
}

func TestSink_invalid_event_is_not_recorded(t *testing.T) {
	store := &memStore{}
	s := audit.NewSink(store, 16, quietLogger())
	s.Record("", "k1", "sandbox.create", "sandbox", "s1", "r1", time.Unix(0, 0)) // empty tenant → invalid
	s.Close()
	if store.count() != 0 {
		t.Errorf("recorded invalid event: count = %d, want 0", store.count())
	}
}

func TestSink_drops_when_buffer_full_without_blocking(t *testing.T) {
	// A blocking store + tiny buffer forces overflow; Emit must not block.
	block := make(chan struct{})
	store := &memStore{block: block}
	s := audit.NewSink(store, 1, quietLogger())

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.Record("tenant-a", "k1", "sandbox.create", "sandbox", "s1", "r1", time.Unix(0, 0))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked when buffer full — audit must never stall the hot path")
	}
	if s.Dropped() == 0 {
		t.Error("expected some dropped events under overflow")
	}
	close(block) // unblock the worker so Close can drain
	s.Close()
}
