package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/ratelimit"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// limitedServer is an API with a rate limit of perSecond and burst, on a clock
// the test moves.
func limitedServer(perSecond float64, burst int, now *time.Time) http.Handler {
	svc := app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	mw := api.Chain(auth.Middleware(stubAuth{}), api.RateLimit(ratelimit.New(perSecond, burst), func() time.Time { return *now }))
	return api.NewRouter(api.NewHandler(svc, nil), mw, nil)
}

func TestRequestsBeyondTheLimitAnswer429WithRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	srv := limitedServer(1, 2, &now)
	first := call(srv, http.MethodGet, "/v1/sandboxes", "key-a", "")
	second := call(srv, http.MethodGet, "/v1/sandboxes", "key-a", "")

	third := call(srv, http.MethodGet, "/v1/sandboxes", "key-a", "")

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("the burst = %d and %d, want 200 and 200", first.Code, second.Code)
	}
	if third.Code != http.StatusTooManyRequests || third.Header().Get("Retry-After") != "1" || !strings.Contains(third.Body.String(), "rate_limited") {
		t.Errorf("third = %d, Retry-After %q, body %s; want 429, 1 and rate_limited", third.Code, third.Header().Get("Retry-After"), third.Body.String())
	}
}

func TestTheLimitRecoversAfterTheRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	srv := limitedServer(1, 1, &now)
	call(srv, http.MethodGet, "/v1/sandboxes", "key-a", "")
	refused := call(srv, http.MethodGet, "/v1/sandboxes", "key-a", "")

	now = now.Add(time.Second)
	allowed := call(srv, http.MethodGet, "/v1/sandboxes", "key-a", "")

	if refused.Code != http.StatusTooManyRequests || allowed.Code != http.StatusOK {
		t.Errorf("at once = %d, a second later = %d; want 429 and 200", refused.Code, allowed.Code)
	}
}

func TestOneKeyAtItsLimitDoesNotAffectAnother(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	srv := limitedServer(1, 1, &now)
	call(srv, http.MethodGet, "/v1/sandboxes", "key-a", "")

	limited := call(srv, http.MethodGet, "/v1/sandboxes", "key-a", "")
	other := call(srv, http.MethodGet, "/v1/sandboxes", "key-b", "")

	if limited.Code != http.StatusTooManyRequests || other.Code != http.StatusOK {
		t.Errorf("key-a = %d, key-b = %d; want 429 and 200", limited.Code, other.Code)
	}
}

func TestPublicAndUnauthenticatedRequestsAreNotCounted(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	srv := limitedServer(1, 1, &now)
	for range 5 {
		if rec := call(srv, http.MethodGet, "/v1/healthz", "", ""); rec.Code != http.StatusOK {
			t.Fatalf("healthz = %d, want 200", rec.Code)
		}
		if rec := call(srv, http.MethodGet, "/v1/sandboxes", "", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a request without credentials = %d, want 401", rec.Code)
		}
	}

	rec := call(srv, http.MethodGet, "/v1/sandboxes", "key-a", "")

	if rec.Code != http.StatusOK {
		t.Errorf("the first request of key-a = %d, want 200", rec.Code)
	}
}
