//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
)

// saveSandbox stores a running sandbox created offset after a fixed base time.
func saveSandbox(t *testing.T, store *postgres.Store, tenant, id string, offset time.Duration, metadata, env map[string]string) *domain.Sandbox {
	t.Helper()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(offset)
	sb := &domain.Sandbox{
		ID: id, TenantID: tenant, Image: "busybox", State: domain.StateRunning,
		CreatedAt: created, UpdatedAt: created, ExpiresAt: created.Add(time.Hour),
		Metadata: metadata, Env: env,
	}
	if err := store.Save(context.Background(), sb); err != nil {
		t.Fatalf("Save %s: %v", id, err)
	}
	return sb
}

func ids(sandboxes []*domain.Sandbox) []string {
	out := make([]string, 0, len(sandboxes))
	for _, sb := range sandboxes {
		out = append(out, sb.ID)
	}
	return out
}

func TestSavedMetadataAndEnvAreReadBack(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	metadata := map[string]string{"team": "search", "app.kubernetes.io/name": "agent"}
	env := map[string]string{"MODE": "test", "EMPTY": ""}
	saveSandbox(t, store, "tenant-a", "sbx_1", 0, metadata, env)

	got, err := store.Get(context.Background(), "tenant-a", "sbx_1")

	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got.Metadata, metadata) || !reflect.DeepEqual(got.Env, env) {
		t.Errorf("metadata=%v env=%v, want %v and %v", got.Metadata, got.Env, metadata, env)
	}
}

func TestSandboxSavedWithoutAttributesReadsBackWithNone(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	saveSandbox(t, store, "tenant-a", "sbx_1", 0, nil, nil)

	got, err := store.Get(context.Background(), "tenant-a", "sbx_1")

	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata != nil || got.Env != nil {
		t.Errorf("metadata=%v env=%v, want nil and nil", got.Metadata, got.Env)
	}
}

func TestStateTransitionSaveKeepsTheAttributes(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	sb := saveSandbox(t, store, "tenant-a", "sbx_1", 0, map[string]string{"team": "search"}, map[string]string{"MODE": "test"})
	// A caller that re-saves a sandbox it loaded without the attributes must not erase them.
	sb.Metadata, sb.Env = nil, nil
	sb.State = domain.StatePaused

	if err := store.Save(ctx, sb); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Get(ctx, "tenant-a", "sbx_1")

	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != domain.StatePaused || got.Metadata["team"] != "search" || got.Env["MODE"] != "test" {
		t.Errorf("after re-save: state=%s metadata=%v env=%v", got.State, got.Metadata, got.Env)
	}
}

func TestListReturnsTheTenantsSandboxesNewestFirst(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	saveSandbox(t, store, "tenant-a", "sbx_old", 0, nil, nil)
	saveSandbox(t, store, "tenant-a", "sbx_new", 2*time.Second, nil, nil)
	saveSandbox(t, store, "tenant-a", "sbx_mid", time.Second, nil, nil)
	saveSandbox(t, store, "tenant-b", "sbx_other", time.Second, nil, nil)

	got, err := store.List(context.Background(), "tenant-a", domain.SandboxQuery{Limit: 10})

	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"sbx_new", "sbx_mid", "sbx_old"}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("List = %v, want %v", ids(got), want)
	}
}

func TestListKeysetWalksEverySandboxOnceEvenWithEqualTimestamps(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	want := map[string]bool{}
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("sbx_%d", i)
		// Pairs share a creation time, so the id tie-break is exercised.
		saveSandbox(t, store, "tenant-a", id, time.Duration(i/2)*time.Second, nil, nil)
		want[id] = true
	}

	seen := map[string]int{}
	var after *domain.SandboxCursor
	for page := 0; page < 10; page++ {
		got, err := store.List(ctx, "tenant-a", domain.SandboxQuery{Limit: 3, After: after})
		if err != nil {
			t.Fatalf("List page %d: %v", page, err)
		}
		if len(got) == 0 {
			break
		}
		for _, sb := range got {
			seen[sb.ID]++
		}
		last := got[len(got)-1]
		after = &domain.SandboxCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	if len(seen) != len(want) {
		t.Errorf("saw %d sandboxes, want %d: %v", len(seen), len(want), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("sandbox %s returned %d times, want 1", id, n)
		}
	}
}

func TestListFiltersByStateAndMetadata(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	saveSandbox(t, store, "tenant-a", "sbx_search", 0, map[string]string{"team": "search", "env": "prod"}, nil)
	saveSandbox(t, store, "tenant-a", "sbx_ads", time.Second, map[string]string{"team": "ads", "env": "prod"}, nil)
	paused := saveSandbox(t, store, "tenant-a", "sbx_paused", 2*time.Second, nil, nil)
	paused.State = domain.StatePaused
	if err := store.Save(ctx, paused); err != nil {
		t.Fatalf("Save paused: %v", err)
	}
	cases := []struct {
		name  string
		query domain.SandboxQuery
		want  []string
	}{
		{"one label", domain.SandboxQuery{Metadata: map[string]string{"env": "prod"}}, []string{"sbx_ads", "sbx_search"}},
		{"two labels", domain.SandboxQuery{Metadata: map[string]string{"env": "prod", "team": "search"}}, []string{"sbx_search"}},
		{"label nobody has", domain.SandboxQuery{Metadata: map[string]string{"team": "none"}}, []string{}},
		{"one state", domain.SandboxQuery{States: []domain.State{domain.StatePaused}}, []string{"sbx_paused"}},
		{"two states", domain.SandboxQuery{States: []domain.State{domain.StatePaused, domain.StateRunning}}, []string{"sbx_paused", "sbx_ads", "sbx_search"}},
		{"state and label", domain.SandboxQuery{States: []domain.State{domain.StateRunning}, Metadata: map[string]string{"team": "ads"}}, []string{"sbx_ads"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.query.Limit = 10

			got, err := store.List(ctx, "tenant-a", c.query)

			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if !reflect.DeepEqual(ids(got), c.want) {
				t.Errorf("List = %v, want %v", ids(got), c.want)
			}
		})
	}
}

func TestListRejectsANonPositiveLimit(t *testing.T) {
	store := postgres.New(setupPostgres(t))

	_, err := store.List(context.Background(), "tenant-a", domain.SandboxQuery{})

	if err == nil {
		t.Error("List with limit 0 returned no error")
	}
}
