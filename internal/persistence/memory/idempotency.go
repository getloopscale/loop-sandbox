package memory

import (
	"context"
	"sync"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

type idemEntry struct {
	requestHash string
	completed   bool
	status      int
	body        []byte
}

// IdempotencyStore is an in-memory domain.IdempotencyStore. The mutex is the
// serialization point (mirrors the DB unique-constraint claim), so concurrent
// same-key Reserve calls resolve to exactly one Claimed.
type IdempotencyStore struct {
	mu   sync.Mutex
	data map[string]*idemEntry
}

// NewIdempotency returns an empty in-memory idempotency store.
func NewIdempotency() *IdempotencyStore {
	return &IdempotencyStore{data: make(map[string]*idemEntry)}
}

func (s *IdempotencyStore) Reserve(_ context.Context, tenantID, idemKey, requestHash string, _ int64) (domain.ReserveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(tenantID, idemKey)
	e, ok := s.data[k]
	if !ok {
		s.data[k] = &idemEntry{requestHash: requestHash}
		return domain.ReserveResult{Outcome: domain.Claimed}, nil
	}
	if e.requestHash != requestHash {
		return domain.ReserveResult{Outcome: domain.Conflict}, nil
	}
	if !e.completed {
		return domain.ReserveResult{Outcome: domain.InProgress}, nil
	}
	return domain.ReserveResult{Outcome: domain.Replay, Record: &domain.IdempotencyRecord{
		Status: e.status, Body: e.body, RequestHash: e.requestHash,
	}}, nil
}

func (s *IdempotencyStore) Complete(_ context.Context, tenantID, idemKey string, status int, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.data[key(tenantID, idemKey)]; ok {
		e.completed, e.status, e.body = true, status, body
	}
	return nil
}

func (s *IdempotencyStore) Release(_ context.Context, tenantID, idemKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(tenantID, idemKey)
	if e, ok := s.data[k]; ok && !e.completed {
		delete(s.data, k)
	}
	return nil
}

var _ domain.IdempotencyStore = (*IdempotencyStore)(nil)
