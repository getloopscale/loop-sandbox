package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// brokenTemplateStore is a template store whose listing fails.
type brokenTemplateStore struct{ domain.TemplateStore }

func (brokenTemplateStore) ListTemplates(context.Context, string) ([]*domain.Template, error) {
	return nil, errors.New("connection refused")
}

func TestListingTemplatesFailsWhenTheRegistryCannotBeRead(t *testing.T) {
	store := memory.New()
	svc := app.NewService(store, fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	svc.SetTemplateRegistry(brokenTemplateStore{store}, stubScanner{ok: true})
	srv := api.NewRouter(api.NewHandler(svc, nil), auth.Middleware(stubAuth{}), nil)

	rec := call(srv, http.MethodGet, "/v1/templates", "key-a", "")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: a list that silently omits the tenant's templates looks complete (%s)", rec.Code, rec.Body.String())
	}
}

func TestListingTemplatesWithoutARegistryReturnsTheBuiltIns(t *testing.T) {
	svc := app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	srv := api.NewRouter(api.NewHandler(svc, nil), auth.Middleware(stubAuth{}), nil)

	rec := call(srv, http.MethodGet, "/v1/templates", "key-a", "")

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"builtin":true`) {
		t.Errorf("status = %d body = %s, want 200 with the built-in templates", rec.Code, rec.Body.String())
	}
}
