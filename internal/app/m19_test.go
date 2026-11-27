package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	tcplan "github.com/getloopscale/loop-sandbox/internal/plan"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/cells"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// GPU is a premium class: a Starter tenant (GPU cap 0) is denied a GPU sandbox before any
// scheduling, while a CPU sandbox for the same tenant is allowed (M19).
func TestCreateSandboxGPU_denied_on_non_premium_tier(t *testing.T) {
	ctx := context.Background()
	svc := app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
	svc.SetEnforcer(enforcement.New(stubUsage{v: 0}), stubPlan{tier: tcplan.PlanStarter})

	if _, err := svc.CreateSandboxGPU(ctx, actorA, "", "1", time.Hour); !errors.Is(err, tcplan.ErrCostCapExceeded) {
		t.Errorf("GPU on starter err = %v, want ErrCostCapExceeded (402)", err)
	}
	// A plain CPU sandbox is still allowed.
	if _, err := svc.CreateSandbox(ctx, actorA, "", time.Hour); err != nil {
		t.Errorf("CPU sandbox on starter: %v", err)
	}
}

// A GPU sandbox on a premium tier is created, marked GPU, and pinned to the isolated GPU cell.
func TestCreateSandboxGPU_pins_gpu_cell_and_marks_gpu(t *testing.T) {
	ctx := context.Background()
	repo := memory.New()
	svc := app.NewService(repo, fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
	svc.SetEnforcer(enforcement.New(stubUsage{v: 0}), stubPlan{tier: tcplan.PlanTeam})
	router, _ := cells.NewRouter([]string{"cell-0", "cell-1"})
	svc.SetCellRouter(router)

	sb, err := svc.CreateSandboxGPU(ctx, actorA, "", "a100", time.Hour)
	if err != nil {
		t.Fatalf("CreateSandboxGPU: %v", err)
	}
	if sb.GPU != "a100" {
		t.Errorf("GPU = %q, want a100", sb.GPU)
	}
	if sb.CellID != cells.GPUCellID {
		t.Errorf("CellID = %q, want the isolated GPU cell %q", sb.CellID, cells.GPUCellID)
	}
	// Persisted with the GPU marking.
	got, _ := repo.Get(ctx, "tenant-a", sb.ID)
	if got.GPU != "a100" {
		t.Errorf("persisted GPU = %q, want a100", got.GPU)
	}
}
