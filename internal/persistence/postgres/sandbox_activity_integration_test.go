//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
)

func idleSandbox(t *testing.T, id string, limit time.Duration, lastActive time.Time) *domain.Sandbox {
	t.Helper()
	sb, err := domain.NewSandbox(id, "tenant-a", "busybox:latest", time.Hour, fixedNow)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	if err := sb.TransitionTo(domain.StateRunning, fixedNow); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	sb.AutoPauseAfter, sb.LastActiveAt = limit, lastActive
	return sb
}

func TestSandboxStoreKeepsTheIdleLimitAndTheAutoPausedFlag(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	sb := idleSandbox(t, "sbx_1", 90*time.Second, fixedNow)
	sb.AutoPaused = true
	if err := store.Save(ctx, sb); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Get(ctx, "tenant-a", "sbx_1")

	if err != nil || got.AutoPauseAfter != 90*time.Second || !got.LastActiveAt.Equal(fixedNow) || !got.AutoPaused {
		t.Errorf("got %+v err = %v; want a 90 s limit, the last activity and auto_paused", got, err)
	}
}

func TestListIdleReturnsRunningSandboxesPastTheirLimitLeastRecentlyUsedFirst(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	paused := idleSandbox(t, "sbx_paused", time.Minute, fixedNow.Add(-time.Hour))
	if err := paused.TransitionTo(domain.StatePaused, fixedNow); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	for _, sb := range []*domain.Sandbox{
		idleSandbox(t, "sbx_idle", time.Minute, fixedNow.Add(-time.Minute)),
		idleSandbox(t, "sbx_idler", time.Minute, fixedNow.Add(-2*time.Minute)),
		idleSandbox(t, "sbx_active", time.Minute, fixedNow.Add(-59*time.Second)),
		idleSandbox(t, "sbx_no_limit", 0, fixedNow.Add(-time.Hour)),
		paused,
	} {
		if err := store.Save(ctx, sb); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	got, err := store.ListIdle(ctx, fixedNow, 10)

	if err != nil || len(got) != 2 || got[0].ID != "sbx_idler" || got[1].ID != "sbx_idle" {
		ids := []string{}
		for _, sb := range got {
			ids = append(ids, sb.ID)
		}
		t.Errorf("idle = %v err = %v; want sbx_idler then sbx_idle", ids, err)
	}
}

func TestTouchSandboxMovesTheLastActivityForwardOnly(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	if err := store.Save(ctx, idleSandbox(t, "sbx_1", time.Minute, fixedNow)); err != nil {
		t.Fatalf("Save: %v", err)
	}

	later := store.TouchSandbox(ctx, "tenant-a", "sbx_1", fixedNow.Add(time.Minute))
	earlier := store.TouchSandbox(ctx, "tenant-a", "sbx_1", fixedNow.Add(-time.Minute))

	got, err := store.Get(ctx, "tenant-a", "sbx_1")
	if later != nil || earlier != nil || err != nil || !got.LastActiveAt.Equal(fixedNow.Add(time.Minute)) {
		t.Errorf("touches: %v, %v; last activity = %v err = %v; want it one minute after the save", later, earlier, got.LastActiveAt, err)
	}
}

func TestSavingAnOlderCopyDoesNotUndoATouch(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	stale := idleSandbox(t, "sbx_1", time.Minute, fixedNow)
	if err := store.Save(ctx, stale); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.TouchSandbox(ctx, "tenant-a", "sbx_1", fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("TouchSandbox: %v", err)
	}

	err := store.Save(ctx, stale)

	got, getErr := store.Get(ctx, "tenant-a", "sbx_1")
	if err != nil || getErr != nil || !got.LastActiveAt.Equal(fixedNow.Add(time.Minute)) {
		t.Errorf("save: %v, get: %v, last activity = %v; want the touch kept", err, getErr, got.LastActiveAt)
	}
}
