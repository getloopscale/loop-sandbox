package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

func newLimitedService(limit int) *app.Service {
	svc := app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
	svc.SetLiveSandboxLimit(limit)
	return svc
}

func TestCreateIsRefusedAtTheLiveSandboxLimit(t *testing.T) {
	svc, ctx := newLimitedService(2), context.Background()
	for i := 0; i < 2; i++ {
		if _, err := svc.CreateSandbox(ctx, actorA, "busybox", time.Minute); err != nil {
			t.Fatalf("create %d under the limit: %v", i+1, err)
		}
	}

	_, err := svc.CreateSandbox(ctx, actorA, "busybox", time.Minute)

	var limit *app.LiveSandboxLimitError
	if !errors.As(err, &limit) || limit.Limit != 2 || limit.Live != 2 {
		t.Fatalf("third create = %v; want a LiveSandboxLimitError with limit 2 and live 2", err)
	}
}

func TestAPausedSandboxCountsAgainstTheLiveSandboxLimit(t *testing.T) {
	svc, ctx := newLimitedService(1), context.Background()
	sb, err := svc.CreateSandbox(ctx, actorA, "busybox", time.Minute)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.PauseSandbox(ctx, actorA, sb.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}

	_, err = svc.CreateSandbox(ctx, actorA, "busybox", time.Minute)

	var limit *app.LiveSandboxLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("create beside a paused sandbox = %v; want a LiveSandboxLimitError", err)
	}
}

func TestADestroyedSandboxFreesTheLiveSandboxLimit(t *testing.T) {
	svc, ctx := newLimitedService(1), context.Background()
	sb, err := svc.CreateSandbox(ctx, actorA, "busybox", time.Minute)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.DestroySandbox(ctx, actorA, sb.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	if _, err := svc.CreateSandbox(ctx, actorA, "busybox", time.Minute); err != nil {
		t.Fatalf("create after a destroy = %v; want a sandbox", err)
	}
}

func TestTheLiveSandboxLimitIsPerTenant(t *testing.T) {
	svc, ctx := newLimitedService(1), context.Background()
	if _, err := svc.CreateSandbox(ctx, actorA, "busybox", time.Minute); err != nil {
		t.Fatalf("create for tenant A: %v", err)
	}

	if _, err := svc.CreateSandbox(ctx, actorB, "busybox", time.Minute); err != nil {
		t.Fatalf("create for tenant B = %v; want a sandbox", err)
	}
}
