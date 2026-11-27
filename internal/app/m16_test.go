package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	tcplan "github.com/getloopscale/loop-sandbox/internal/plan"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// TestCreateVolume_storage_quota_is_a_live_gauge: the storage cap is enforced against the SUM
// of the tenant's volumes (source of truth), not a period counter — so it holds within a
// billing period and never desyncs (M16 review MEDIUM regression).
func TestCreateVolume_storage_quota_enforced_from_table(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	svc := app.NewService(store, fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
	svc.SetVolumeStore(store)
	// Starter volume cap = 10 GB. stubUsage is irrelevant to storage (summed from the table).
	svc.SetEnforcer(enforcement.New(stubUsage{v: 0}), stubPlan{tier: tcplan.PlanStarter})

	sb, err := svc.CreateSandbox(ctx, actorA, "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	// 10 GB fits exactly.
	if _, err := svc.CreateVolume(ctx, actorA, sb.ID, "big", 10); err != nil {
		t.Fatalf("first 10GB volume: %v", err)
	}
	// One more GB exceeds the 10 GB cap → 402.
	if _, err := svc.CreateVolume(ctx, actorA, sb.ID, "extra", 1); !errors.Is(err, tcplan.ErrCostCapExceeded) {
		t.Errorf("over-cap volume err = %v, want ErrCostCapExceeded", err)
	}
	// After deleting the 10GB volume, the gauge drops and a new one fits again.
	vols, _ := store.ListVolumes(ctx, "tenant-a", sb.ID)
	// detach then delete (delete refuses an attached volume by design)
	for _, v := range vols {
		v.Detach()
		_ = store.SaveVolume(ctx, v)
		if err := svc.DeleteVolume(ctx, actorA, v.ID); err != nil {
			t.Fatalf("DeleteVolume: %v", err)
		}
	}
	if _, err := svc.CreateVolume(ctx, actorA, sb.ID, "again", 5); err != nil {
		t.Errorf("after delete, 5GB should fit: %v", err)
	}
}

func TestCreateVolume_rejects_unsafe_name(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	svc := app.NewService(store, fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
	svc.SetVolumeStore(store)
	sb, _ := svc.CreateSandbox(ctx, actorA, "", time.Hour)
	for _, bad := range []string{"../etc", "a b", "with/slash", "dot.name", "", "UPPER"} {
		if _, err := svc.CreateVolume(ctx, actorA, sb.ID, bad, 1); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("name %q err = %v, want ErrInvalidArgument", bad, err)
		}
	}
}

// TestCreateVolume_concurrent_same_name_is_single: N concurrent creates of the SAME named
// volume must converge to ONE volume (the named-volume model), not N duplicate rows/images
// (M16 review MEDIUM). Run with -race.
func TestCreateVolume_concurrent_same_name_is_single(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	svc := app.NewService(store, fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
	svc.SetVolumeStore(store)
	sb, _ := svc.CreateSandbox(ctx, actorA, "", time.Hour)

	const n = 8
	ids := make(chan string, n)
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			v, err := svc.CreateVolume(ctx, actorA, sb.ID, "shared", 1)
			if err != nil {
				errs <- err
				return
			}
			ids <- v.ID
		}()
	}
	close(start)
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		select {
		case id := <-ids:
			seen[id] = true
		case err := <-errs:
			t.Fatalf("concurrent CreateVolume: %v", err)
		}
	}
	if len(seen) != 1 {
		t.Errorf("got %d distinct volume ids for one name, want 1", len(seen))
	}
	all, _ := store.ListVolumes(ctx, "tenant-a", sb.ID)
	if len(all) != 1 {
		t.Errorf("store holds %d volumes for one name, want 1", len(all))
	}
}

func TestDeleteVolume_refuses_attached(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	svc := app.NewService(store, fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
	svc.SetVolumeStore(store)
	sb, _ := svc.CreateSandbox(ctx, actorA, "", time.Hour)
	vol, err := svc.CreateVolume(ctx, actorA, sb.ID, "data", 1)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	// Still attached → delete refused (409 / invalid transition).
	if err := svc.DeleteVolume(ctx, actorA, vol.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("delete attached err = %v, want ErrInvalidTransition", err)
	}
}
