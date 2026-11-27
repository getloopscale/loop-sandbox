package app_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	tcplan "github.com/getloopscale/loop-sandbox/internal/plan"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

type seqIDs struct{ n int }

func (s *seqIDs) New() string { s.n++; return fmt.Sprintf("sb-%d", s.n) }

// auditRec captures a full recorded audit event for content assertions.
type auditRec struct {
	tenantID, actorID, action, resourceType, resourceID, requestID string
}

// fakeAudit captures recorded audit events.
type fakeAudit struct {
	mu   sync.Mutex
	recs []auditRec
}

func (f *fakeAudit) Record(tenantID, actorID, action, resourceType, resourceID, requestID string, _ time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, auditRec{tenantID, actorID, action, resourceType, resourceID, requestID})
}
func (f *fakeAudit) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.recs) }
func (f *fakeAudit) last() auditRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recs[len(f.recs)-1]
}

func newService() (*app.Service, *memory.Store, *fakeAudit) {
	repo := memory.New()
	aud := &fakeAudit{}
	svc := app.NewService(repo, fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, aud, nil, nil)
	return svc, repo, aud
}

var actorA = app.Actor{TenantID: "tenant-a", KeyID: "key-a", RequestID: "req-1"}
var actorB = app.Actor{TenantID: "tenant-b", KeyID: "key-b", RequestID: "req-2"}

func TestCreateSandbox_persists_runs_and_audits(t *testing.T) {
	svc, _, aud := newService()

	sb, err := svc.CreateSandbox(context.Background(), actorA, "python:3", time.Hour)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if sb.State != domain.StateRunning {
		t.Errorf("State = %q, want running", sb.State)
	}
	if got, _ := svc.GetSandbox(context.Background(), actorA, sb.ID); got.State != domain.StateRunning {
		t.Errorf("persisted State = %q, want running", got.State)
	}
	if aud.count() != 1 {
		t.Fatalf("audit events = %d, want 1 (sandbox.create)", aud.count())
	}
	// The audit event must be attributable: correct tenant, actor, resource, request.
	got := aud.last()
	if got.action != "sandbox.create" || got.tenantID != "tenant-a" ||
		got.actorID != "key-a" || got.resourceType != "sandbox" ||
		got.resourceID != sb.ID || got.requestID != "req-1" {
		t.Errorf("audit event = %+v, want create/tenant-a/key-a/sandbox/%s/req-1", got, sb.ID)
	}
}

func TestCreateSandbox_compensates_on_driver_failure(t *testing.T) {
	repo := memory.New()
	boom := errors.New("image pull failed")
	svc := app.NewService(repo, fake.NewFailing(boom), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)

	_, err := svc.CreateSandbox(context.Background(), actorA, "python:3", time.Hour)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped driver error", err)
	}
	if _, gErr := repo.Get(context.Background(), "tenant-a", "sb-1"); !errors.Is(gErr, domain.ErrNotFound) {
		t.Errorf("orphaned row after failed create: get err = %v, want ErrNotFound", gErr)
	}
}

// A driver policy/validation refusal (runtime.ErrInvalidRequest, e.g. disallowed
// image) must surface as a client error (ErrInvalidArgument → 400), not a 500.
func TestCreateSandbox_invalid_request_maps_to_client_error(t *testing.T) {
	repo := memory.New()
	svc := app.NewService(repo, fake.NewFailing(runtime.ErrInvalidRequest), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)

	_, err := svc.CreateSandbox(context.Background(), actorA, "evil/x:latest", time.Hour)
	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument (client 4xx)", err)
	}
	// pending row compensated away.
	if _, gErr := repo.Get(context.Background(), "tenant-a", "sb-1"); !errors.Is(gErr, domain.ErrNotFound) {
		t.Errorf("orphan after refusal: %v", gErr)
	}
}

// TTL is a mandatory security bound: 0/negative → DefaultTTL, over-max → MaxTTL,
// in-range → passthrough.
func TestCreateSandbox_clamps_ttl(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{"zero_defaults", 0, app.DefaultTTL},
		{"negative_defaults", -time.Second, app.DefaultTTL},
		{"over_max_clamped", 2 * time.Hour, app.MaxTTL},
		{"in_range_passthrough", 30 * time.Minute, 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := newService()
			sb, err := svc.CreateSandbox(context.Background(), actorA, "", tc.ttl)
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			if got := sb.ExpiresAt.Sub(sb.CreatedAt); got != tc.want {
				t.Errorf("ttl %v → lifetime %v, want %v", tc.ttl, got, tc.want)
			}
		})
	}
}

func TestGetSandbox_cross_tenant_is_not_found(t *testing.T) {
	svc, _, _ := newService()
	sb, _ := svc.CreateSandbox(context.Background(), actorA, "", time.Hour)

	_, err := svc.GetSandbox(context.Background(), actorB, sb.ID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant Get err = %v, want ErrNotFound", err)
	}
}

func TestDestroySandbox_cross_tenant_is_not_found(t *testing.T) {
	svc, repo, _ := newService()
	sb, _ := svc.CreateSandbox(context.Background(), actorA, "", time.Hour)

	if err := svc.DestroySandbox(context.Background(), actorB, sb.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant Destroy err = %v, want ErrNotFound", err)
	}
	if _, err := repo.Get(context.Background(), "tenant-a", sb.ID); err != nil {
		t.Errorf("tenant-a sandbox affected by cross-tenant destroy: %v", err)
	}
}

func TestDestroySandbox_then_get_is_not_found_and_idempotent(t *testing.T) {
	svc, _, aud := newService()
	sb, _ := svc.CreateSandbox(context.Background(), actorA, "", time.Hour)

	if err := svc.DestroySandbox(context.Background(), actorA, sb.ID); err != nil {
		t.Fatalf("DestroySandbox: %v", err)
	}
	if _, err := svc.GetSandbox(context.Background(), actorA, sb.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get after destroy err = %v, want ErrNotFound", err)
	}
	if err := svc.DestroySandbox(context.Background(), actorA, sb.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second Destroy err = %v, want ErrNotFound", err)
	}
	// create + destroy = 2 audit events.
	if aud.count() != 2 {
		t.Errorf("audit events = %d, want 2", aud.count())
	}
}

// recObserver records observer calls for classification assertions.
type recObserver struct {
	execResults   []string
	created       int
	destroyed     int
	quotaDenied   []string
	quotaFailOpen []string
	cellsRouted   []string
	artifactURLs  int
}

func (o *recObserver) SandboxCreated(time.Duration) { o.created++ }
func (o *recObserver) SandboxDestroyed()            { o.destroyed++ }
func (o *recObserver) ExecObserved(r string)        { o.execResults = append(o.execResults, r) }
func (o *recObserver) QuotaDenied(m string)         { o.quotaDenied = append(o.quotaDenied, m) }
func (o *recObserver) QuotaFailOpen(m string)       { o.quotaFailOpen = append(o.quotaFailOpen, m) }
func (o *recObserver) CellRouted(c string)          { o.cellsRouted = append(o.cellsRouted, c) }
func (o *recObserver) ArtifactURLIssued()           { o.artifactURLs++ }

// execStubDriver embeds the fake driver (so Create/Destroy work) but returns a
// scripted Exec result/error, to drive the observer's result classification.
type execStubDriver struct {
	*fake.Driver
	res *runtime.ExecResult
	err error
}

func (d execStubDriver) Exec(context.Context, runtime.SandboxID, runtime.ExecRequest) (*runtime.ExecResult, error) {
	return d.res, d.err
}

// TestExec_observer_classifies_result proves the metric taxonomy: a non-zero exit
// is "nonzero_exit" (the user's program, US-02), a driver failure is "error", and a
// timeout is "timeout" — so alerting on result="error" is not polluted by user exits.
func TestExec_observer_classifies_result(t *testing.T) {
	for _, tc := range []struct {
		name    string
		res     *runtime.ExecResult
		err     error
		want    string
		wantErr bool
	}{
		{"exit_zero_ok", &runtime.ExecResult{ExitCode: 0}, nil, "ok", false},
		{"exit_nonzero", &runtime.ExecResult{ExitCode: 1}, nil, "nonzero_exit", false},
		{"driver_error", nil, errors.New("boom"), "error", true},
		{"driver_timeout", nil, runtime.ErrTimeout, "timeout", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := memory.New()
			drv := execStubDriver{Driver: fake.New(), res: tc.res, err: tc.err}
			obs := &recObserver{}
			svc := app.NewService(repo, drv, clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
			svc.SetObserver(obs)

			sb, err := svc.CreateSandbox(context.Background(), actorA, "", time.Hour)
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			_, execErr := svc.Exec(context.Background(), actorA, sb.ID, runtime.ExecRequest{Cmd: []string{"x"}})
			if tc.wantErr && execErr == nil {
				t.Errorf("Exec err = nil, want error")
			}
			if !tc.wantErr && execErr != nil {
				t.Errorf("Exec err = %v, want nil", execErr)
			}
			if len(obs.execResults) != 1 || obs.execResults[0] != tc.want {
				t.Errorf("ExecObserved = %v, want [%s]", obs.execResults, tc.want)
			}
		})
	}
}

// stubPlan resolves a fixed plan; stubUsage returns a fixed current usage.
type stubPlan struct{ tier tcplan.Plan }

func (s stubPlan) PlanFor(context.Context, string) (tcplan.Plan, error) { return s.tier, nil }

type stubUsage struct{ v int64 }

func (s stubUsage) Value(context.Context, string, string, int64) (int64, error) { return s.v, nil }

// TestCreateSandbox_over_plan_cap_returns_ExceededError proves the M8 gate: an
// over-cap tenant gets a plan.ExceededError (→ 402) and NO sandbox is created.
func TestCreateSandbox_over_plan_cap_returns_ExceededError(t *testing.T) {
	repo := memory.New()
	obs := &recObserver{}
	svc := app.NewService(repo, fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
	svc.SetObserver(obs)
	// starter creations cap = 50; usage already at 50 → next create blocked.
	svc.SetEnforcer(enforcement.New(stubUsage{v: 50}), stubPlan{tier: tcplan.PlanStarter})

	_, err := svc.CreateSandbox(context.Background(), actorA, "", time.Hour)
	if !errors.Is(err, tcplan.ErrCostCapExceeded) {
		t.Fatalf("err = %v, want ErrCostCapExceeded (402)", err)
	}
	// No sandbox row persisted.
	if _, gErr := repo.Get(context.Background(), "tenant-a", "sb-1"); !errors.Is(gErr, domain.ErrNotFound) {
		t.Errorf("sandbox persisted despite quota denial: %v", gErr)
	}
	if len(obs.quotaDenied) != 1 || obs.quotaDenied[0] != "sandbox_creations" {
		t.Errorf("quotaDenied = %v, want [sandbox_creations]", obs.quotaDenied)
	}
}

func TestCreateSandbox_enterprise_is_unlimited(t *testing.T) {
	svc, _, _ := newService()
	svc.SetEnforcer(enforcement.New(stubUsage{v: 1 << 40}), stubPlan{tier: tcplan.PlanEnterprise})
	if _, err := svc.CreateSandbox(context.Background(), actorA, "", time.Hour); err != nil {
		t.Errorf("enterprise create → %v, want nil (unlimited)", err)
	}
}

func TestExec_over_plan_cap_returns_ExceededError(t *testing.T) {
	svc, _, _ := newService()
	sb, _ := svc.CreateSandbox(context.Background(), actorA, "", time.Hour)
	// Now cap executions: starter exec cap = 500, usage at 500 → blocked.
	svc.SetEnforcer(enforcement.New(stubUsage{v: 500}), stubPlan{tier: tcplan.PlanStarter})
	_, err := svc.Exec(context.Background(), actorA, sb.ID, runtime.ExecRequest{Cmd: []string{"echo"}})
	if !errors.Is(err, tcplan.ErrCostCapExceeded) {
		t.Errorf("exec over cap → %v, want ErrCostCapExceeded (402)", err)
	}
}
