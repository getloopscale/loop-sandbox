package app_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// movingClock is a clock the test moves forward.
type movingClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *movingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *movingClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type idleFixture struct {
	svc   *app.Service
	clock *movingClock
	audit *fakeAudit
}

func newIdleFixture() idleFixture {
	clk := &movingClock{t: time.Unix(1_700_000_000, 0).UTC()}
	aud := &fakeAudit{}
	return idleFixture{svc: app.NewService(memory.New(), fake.New(), clk, &seqIDs{}, aud, nil, nil), clock: clk, audit: aud}
}

// request runs fn with a context that ends when fn returns, as the context of
// an HTTP request does.
func request(fn func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn(ctx)
}

func (f idleFixture) create(t *testing.T, limit time.Duration) *domain.Sandbox {
	t.Helper()
	sb, err := f.svc.CreateSandboxFromSpec(context.Background(), actorA, app.SandboxSpec{Image: "busybox:latest", TTL: time.Hour, AutoPause: limit})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return sb
}

func (f idleFixture) state(t *testing.T, id string) domain.State {
	t.Helper()
	sb, err := f.svc.GetSandbox(context.Background(), actorA, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return sb.State
}

func (f idleFixture) pauseIdle(t *testing.T) int {
	t.Helper()
	n, err := f.svc.PauseIdleSandboxes(context.Background())
	if err != nil {
		t.Fatalf("PauseIdleSandboxes: %v", err)
	}
	return n
}

func TestASandboxPastItsIdleLimitIsPaused(t *testing.T) {
	f := newIdleFixture()
	sb := f.create(t, time.Minute)
	f.clock.advance(time.Minute)

	paused := f.pauseIdle(t)

	if paused != 1 || f.state(t, sb.ID) != domain.StatePaused {
		t.Errorf("paused %d and the sandbox is %s; want 1 and paused", paused, f.state(t, sb.ID))
	}
	if last := f.audit.last(); last.action != "sandbox.pause" || last.actorID != "system:idle" || last.resourceID != sb.ID {
		t.Errorf("audit = %+v, want sandbox.pause by system:idle", last)
	}
}

func TestASandboxIsNotPausedBeforeItsIdleLimit(t *testing.T) {
	f := newIdleFixture()
	sb := f.create(t, time.Minute)
	f.clock.advance(time.Minute - time.Second)

	paused := f.pauseIdle(t)

	if paused != 0 || f.state(t, sb.ID) != domain.StateRunning {
		t.Errorf("paused %d and the sandbox is %s; want 0 and running", paused, f.state(t, sb.ID))
	}
}

func TestASandboxWithoutAnIdleLimitIsNeverPaused(t *testing.T) {
	f := newIdleFixture()
	sb := f.create(t, 0)
	f.clock.advance(48 * time.Hour)

	paused := f.pauseIdle(t)

	if paused != 0 || f.state(t, sb.ID) != domain.StateRunning {
		t.Errorf("paused %d and the sandbox is %s; want 0 and running", paused, f.state(t, sb.ID))
	}
}

func TestUsingASandboxRestartsItsIdleLimit(t *testing.T) {
	f := newIdleFixture()
	sb := f.create(t, time.Minute)
	f.clock.advance(50 * time.Second)
	request(func(ctx context.Context) {
		if err := f.svc.WriteFile(ctx, actorA, sb.ID, "/workspace/a.txt", bytes.NewBufferString("x")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	})
	f.clock.advance(50 * time.Second)

	paused := f.pauseIdle(t)

	if paused != 0 || f.state(t, sb.ID) != domain.StateRunning {
		t.Errorf("paused %d and the sandbox is %s; want it running 50 s after its last use", paused, f.state(t, sb.ID))
	}
}

func TestReadingTheSandboxRecordDoesNotCountAsUse(t *testing.T) {
	f := newIdleFixture()
	sb := f.create(t, time.Minute)
	f.clock.advance(50 * time.Second)
	f.state(t, sb.ID)
	f.clock.advance(10 * time.Second)

	paused := f.pauseIdle(t)

	if paused != 1 {
		t.Errorf("paused %d, want 1: a GET of the sandbox is not use", paused)
	}
}

func TestARequestToAnIdlePausedSandboxResumesItAndIsServed(t *testing.T) {
	f := newIdleFixture()
	sb := f.create(t, time.Minute)
	if err := f.svc.WriteFile(context.Background(), actorA, sb.ID, "/workspace/a.txt", bytes.NewBufferString("kept")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f.clock.advance(time.Minute)
	f.pauseIdle(t)

	var (
		got []byte
		err error
	)
	request(func(ctx context.Context) {
		var rc io.ReadCloser
		if rc, err = f.svc.ReadFile(ctx, actorA, sb.ID, "/workspace/a.txt"); err == nil {
			defer rc.Close() //nolint:errcheck // a read-only stream
			got, err = io.ReadAll(rc)
		}
	})

	if err != nil || string(got) != "kept" || f.state(t, sb.ID) != domain.StateRunning {
		t.Errorf("read = %q err = %v and the sandbox is %s; want kept, nil and running", got, err, f.state(t, sb.ID))
	}
}

func TestASandboxInUseIsNotPausedHoweverLongTheRequestTakes(t *testing.T) {
	f := newIdleFixture()
	sb := f.create(t, time.Minute)
	ctx, endRequest := context.WithCancel(context.Background())
	defer endRequest()
	if _, err := f.svc.Exec(ctx, actorA, sb.ID, runtime.ExecRequest{Cmd: []string{"echo", "x"}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	f.clock.advance(10 * time.Minute) // the request context is still open

	paused := f.pauseIdle(t)

	if paused != 0 || f.state(t, sb.ID) != domain.StateRunning {
		t.Errorf("paused %d and the sandbox is %s; want 0 and running while a request is open", paused, f.state(t, sb.ID))
	}
}

func TestAPauseTheCallerAskedForIsNotUndoneByARequest(t *testing.T) {
	f := newIdleFixture()
	sb := f.create(t, time.Minute)
	if err := f.svc.PauseSandbox(context.Background(), actorA, sb.ID); err != nil {
		t.Fatalf("PauseSandbox: %v", err)
	}

	request(func(ctx context.Context) {
		_ = f.svc.WriteFile(ctx, actorA, sb.ID, "/workspace/a.txt", bytes.NewBufferString("x")) // the outcome is the state below
	})

	if got := f.state(t, sb.ID); got != domain.StatePaused {
		t.Errorf("the sandbox is %s after a request, want it still paused", got)
	}
}

func TestAResumedSandboxGetsAFullIdleLimitAgain(t *testing.T) {
	f := newIdleFixture()
	sb := f.create(t, time.Minute)
	f.clock.advance(time.Minute)
	f.pauseIdle(t)
	f.clock.advance(time.Hour)
	if _, err := f.svc.ResumeSandbox(context.Background(), actorA, sb.ID); err != nil {
		t.Fatalf("ResumeSandbox: %v", err)
	}
	f.clock.advance(30 * time.Second)

	paused := f.pauseIdle(t)

	if paused != 0 || f.state(t, sb.ID) != domain.StateRunning {
		t.Errorf("paused %d and the sandbox is %s; want it running 30 s after the resume", paused, f.state(t, sb.ID))
	}
}

func TestCreateRejectsAnIdleLimitOutOfBounds(t *testing.T) {
	for name, limit := range map[string]time.Duration{
		"below the floor":   domain.MinAutoPause - time.Second,
		"above the ceiling": domain.MaxAutoPause + time.Second,
		"negative":          -time.Second,
	} {
		t.Run(name, func(t *testing.T) {
			f := newIdleFixture()

			_, err := f.svc.CreateSandboxFromSpec(context.Background(), actorA, app.SandboxSpec{Image: "busybox:latest", AutoPause: limit})

			var invalid *domain.ValidationError
			if !errors.As(err, &invalid) || invalid.Field != "auto_pause_seconds" {
				t.Errorf("err = %v, want a validation error on auto_pause_seconds", err)
			}
		})
	}
}

func TestAForkHasTheIdleLimitOfItsParent(t *testing.T) {
	f := newIdleFixture()
	parent := f.create(t, time.Minute)

	forks, err := f.svc.ForkSandboxes(context.Background(), actorA, parent.ID, 1)

	if err != nil || len(forks) != 1 || forks[0].AutoPauseAfter != time.Minute {
		t.Errorf("forks = %+v err = %v, want one fork with a one minute idle limit", forks, err)
	}
}
