// Package ratelimit bounds how many requests one key may make per second, with
// a token bucket per key held in this process.
package ratelimit

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// idleAfter is how long a key may go unseen before its bucket is dropped. By
// then the bucket is full again, so dropping it changes no decision.
const idleAfter = 10 * time.Minute

// sweepEvery is how many decisions pass between sweeps for idle keys.
const sweepEvery = 4096

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Limiter decides, per key, whether one more request fits the rate.
type Limiter struct {
	perSecond rate.Limit
	burst     int
	mu        sync.Mutex
	buckets   map[string]*bucket
	decisions int
}

// New returns a Limiter that lets each key make perSecond requests per second on
// average and burst at once. It returns nil, which allows everything, when
// perSecond is not positive.
func New(perSecond float64, burst int) *Limiter {
	if perSecond <= 0 {
		return nil
	}
	if burst < 1 {
		burst = 1
	}
	return &Limiter{perSecond: rate.Limit(perSecond), burst: burst, buckets: map[string]*bucket{}}
}

// Allow reports whether key may make a request at now. When it may not,
// retryAfter is how long until one would be allowed.
func (l *Limiter) Allow(key string, now time.Time) (ok bool, retryAfter time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{limiter: rate.NewLimiter(l.perSecond, l.burst)}
		l.buckets[key] = b
	}
	b.lastSeen = now
	l.decisions++
	if l.decisions%sweepEvery == 0 {
		for k, other := range l.buckets {
			if now.Sub(other.lastSeen) > idleAfter {
				delete(l.buckets, k)
			}
		}
	}
	l.mu.Unlock()

	r := b.limiter.ReserveN(now, 1)
	if delay := r.DelayFrom(now); delay > 0 {
		r.CancelAt(now) // the request is refused, so it must not use up a token
		return false, delay
	}
	return true, 0
}

// RetryAfterSeconds is d as the whole seconds of a Retry-After header, at least 1.
func RetryAfterSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}
