package enforcement

import (
	"context"
	"sync"
	"time"
)

// CachedUsageReader wraps a UsageReader with a short TTL cache so the quota check on
// the request hot path does not hit the cost store on every call (B9). Usage counters
// are monotonic within a period, so a slightly stale read only ever UNDER-counts by
// at most the TTL window — acceptable for a best-effort cap, and it never blocks a
// legitimate request on store latency.
type CachedUsageReader struct {
	inner UsageReader
	ttl   time.Duration
	now   func() time.Time

	mu    sync.Mutex
	cache map[cacheKey]cacheEntry
}

type cacheKey struct {
	tenantID    string
	metric      string
	periodStart int64
}

type cacheEntry struct {
	value   int64
	expires time.Time
}

// NewCachedUsageReader wraps inner with a TTL cache. now defaults to time.Now.
func NewCachedUsageReader(inner UsageReader, ttl time.Duration, now func() time.Time) *CachedUsageReader {
	if now == nil {
		now = time.Now
	}
	return &CachedUsageReader{inner: inner, ttl: ttl, now: now, cache: map[cacheKey]cacheEntry{}}
}

// Value returns the cached usage when fresh, else reads through and caches it.
func (c *CachedUsageReader) Value(ctx context.Context, tenantID, metric string, periodStart int64) (int64, error) {
	k := cacheKey{tenantID, metric, periodStart}
	now := c.now()

	c.mu.Lock()
	if e, ok := c.cache[k]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.value, nil
	}
	c.mu.Unlock()

	v, err := c.inner.Value(ctx, tenantID, metric, periodStart)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.cache[k] = cacheEntry{value: v, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return v, nil
}
