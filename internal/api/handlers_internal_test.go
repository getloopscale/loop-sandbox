package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// TestReady_returns_503_when_dependency_down proves the readiness branch: when
// the ReadyCheck (DB ping) fails, /v1/readyz reports 503 so orchestrators stop
// routing traffic to an instance whose PostgreSQL is unreachable.
func TestReady_returns_503_when_dependency_down(t *testing.T) {
	h := &Handler{ReadyCheck: func(context.Context) error { return errors.New("db down") }}
	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest("GET", "/v1/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestReady_returns_200_when_no_check(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest("GET", "/v1/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// White-box test of the typed-domain-error → HTTP mapping (writeDomainError).
// Proves every branch (400/404/409/500) and the no-leak guarantee (TRD §7.4),
// which the black-box E2E cannot reach in M0 (some errors aren't triggerable
// through the happy-path endpoints yet).
func TestWriteDomainError_mapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"not_found", domain.ErrNotFound, 404, "not_found"},
		{"invalid_argument", domain.ErrInvalidArgument, 400, "invalid_request"},
		{"invalid_transition", domain.ErrInvalidTransition, 409, "conflict"},
		{"wrapped_not_found", fmt.Errorf("layer: %w", domain.ErrNotFound), 404, "not_found"},
		{"unknown_internal", errors.New("secret db dsn leaked?"), 500, "internal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeDomainError(rec, tc.err)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			var body errorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Error.Code != tc.wantBody {
				t.Errorf("error.code = %q, want %q", body.Error.Code, tc.wantBody)
			}
			// No-leak: the raw internal error text must never reach the client.
			if strings.Contains(rec.Body.String(), "secret db dsn") {
				t.Errorf("internal error detail leaked to client: %s", rec.Body.String())
			}
		})
	}
}
