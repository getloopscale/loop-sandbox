package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/ratelimit"
)

// RateLimit returns a middleware that refuses, with 429 and Retry-After, the
// requests of a credential beyond the limiter's rate. It runs after
// authentication: the bucket belongs to the API key, or to the tenant for a
// credential without a key id. now is the clock; nil means the wall clock.
func RateLimit(limiter *ratelimit.Limiter, now func() time.Time) Middleware {
	if now == nil {
		now = time.Now
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p := auth.PrincipalFrom(r.Context()); p != nil {
				key := p.TenantID + "/" + p.KeyID
				if ok, retry := limiter.Allow(key, now()); !ok {
					w.Header().Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(retry)))
					writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests for this API key")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Chain returns a middleware that applies outer first and inner second.
func Chain(outer, inner Middleware) Middleware {
	return func(next http.Handler) http.Handler { return outer(inner(next)) }
}
