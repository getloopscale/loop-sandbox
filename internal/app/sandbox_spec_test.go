package app_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// execRecorder is a driver that remembers the last exec request it was given.
type execRecorder struct {
	*fake.Driver
	last runtime.ExecRequest
}

func (d *execRecorder) Exec(ctx context.Context, id runtime.SandboxID, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	d.last = req
	return d.Driver.Exec(ctx, id, req)
}

func newRecordingService() (*app.Service, *execRecorder) {
	driver := &execRecorder{Driver: fake.New()}
	svc := app.NewService(memory.New(), driver, clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
	return svc, driver
}

func TestExecReceivesTheSandboxEnv(t *testing.T) {
	svc, driver := newRecordingService()
	ctx := context.Background()
	sb, err := svc.CreateSandboxFromSpec(ctx, actorA, app.SandboxSpec{Env: map[string]string{"MODE": "test", "REGION": "eu"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.Exec(ctx, actorA, sb.ID, runtime.ExecRequest{Cmd: []string{"true"}}); err != nil {
		t.Fatalf("exec: %v", err)
	}

	if want := map[string]string{"MODE": "test", "REGION": "eu"}; !reflect.DeepEqual(driver.last.Env, want) {
		t.Errorf("driver saw env %v, want %v", driver.last.Env, want)
	}
}

func TestExecEnvWinsOverTheSandboxEnv(t *testing.T) {
	svc, driver := newRecordingService()
	ctx := context.Background()
	sb, err := svc.CreateSandboxFromSpec(ctx, actorA, app.SandboxSpec{Env: map[string]string{"MODE": "test", "REGION": "eu"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err = svc.Exec(ctx, actorA, sb.ID, runtime.ExecRequest{Cmd: []string{"true"}, Env: map[string]string{"MODE": "override", "EXTRA": "1"}})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	want := map[string]string{"MODE": "override", "REGION": "eu", "EXTRA": "1"}
	if !reflect.DeepEqual(driver.last.Env, want) {
		t.Errorf("driver saw env %v, want %v", driver.last.Env, want)
	}
}

func TestExecWithAnInvalidEnvNameNeverReachesTheDriver(t *testing.T) {
	svc, driver := newRecordingService()
	ctx := context.Background()
	sb, err := svc.CreateSandbox(ctx, actorA, "", time.Minute)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err = svc.Exec(ctx, actorA, sb.ID, runtime.ExecRequest{Cmd: []string{"true"}, Env: map[string]string{"A=B": "x"}})

	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("exec err = %v, want ErrInvalidArgument", err)
	}
	if driver.last.Cmd != nil {
		t.Errorf("driver ran %v for a rejected request", driver.last.Cmd)
	}
}

func TestCreateFromSpecRejectsTemplateWithGPU(t *testing.T) {
	svc, _ := newRecordingService()

	_, err := svc.CreateSandboxFromSpec(context.Background(), actorA, app.SandboxSpec{Template: "tpl_1", GPU: "a10"})

	var verr *domain.ValidationError
	if !errors.As(err, &verr) || verr.Field != "template" {
		t.Errorf("err = %v, want a ValidationError on field template", err)
	}
}

func TestSetSandboxTTLRecordsAnAuditEvent(t *testing.T) {
	svc, _, aud := newService()
	ctx := context.Background()
	sb, err := svc.CreateSandbox(ctx, actorA, "", time.Minute)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.SetSandboxTTL(ctx, actorA, sb.ID, 10*time.Minute); err != nil {
		t.Fatalf("SetSandboxTTL: %v", err)
	}

	if got := aud.last(); got.action != "sandbox.set_ttl" || got.resourceID != sb.ID || got.tenantID != actorA.TenantID {
		t.Errorf("last audit event = %+v, want sandbox.set_ttl on %s", got, sb.ID)
	}
}
