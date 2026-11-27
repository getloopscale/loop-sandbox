//go:build firecracker

package firecracker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

func streamVM(t *testing.T, id runtime.SandboxID) *Driver {
	t.Helper()
	d := testDriver(t)
	if _, err := d.Create(context.Background(), runtime.CreateRequest{SandboxID: id, TenantID: "t"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	return d
}

func TestExecStreamDeliversGuestOutputWhileTheCommandRuns(t *testing.T) {
	d := streamVM(t, "stream1")
	got := map[string]string{}
	var firstAt time.Time

	res, err := d.ExecStream(context.Background(), "stream1",
		runtime.ExecRequest{Cmd: []string{"sh", "-c", "echo first; echo warn >&2; sleep 2; echo second; exit 5"}},
		func(c runtime.StreamChunk) {
			if firstAt.IsZero() {
				firstAt = time.Now()
			}
			got[c.Stream] += string(c.Data)
		})
	ended := time.Now()

	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if res.ExitCode != 5 || got["stdout"] != "first\nsecond\n" || got["stderr"] != "warn\n" {
		t.Errorf("exit=%d stdout=%q stderr=%q", res.ExitCode, got["stdout"], got["stderr"])
	}
	if firstAt.IsZero() || ended.Sub(firstAt) < time.Second {
		t.Errorf("first chunk arrived %v before the end, want at least 1s", ended.Sub(firstAt))
	}
}

func TestExecStreamTimeoutKillsTheGuestCommand(t *testing.T) {
	d := streamVM(t, "stream2")
	ctx := context.Background()

	_, err := d.ExecStream(ctx, "stream2",
		runtime.ExecRequest{Cmd: []string{"sh", "-c", "sleep 300 & wait"}, Timeout: time.Second},
		func(runtime.StreamChunk) {})

	if !errors.Is(err, runtime.ErrTimeout) {
		t.Fatalf("ExecStream err = %v, want ErrTimeout", err)
	}
	// The agent kills the process group when the host hangs up; give it a moment.
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, perr := d.Exec(ctx, "stream2", runtime.ExecRequest{Cmd: []string{"sh", "-c", "pgrep -x sleep | wc -l"}})
		if perr != nil {
			t.Fatalf("pgrep: %v", perr)
		}
		if strings.TrimSpace(string(res.Stdout)) == "0" {
			return
		}
		if time.Now().After(deadline) {
			ps, _ := d.Exec(ctx, "stream2", runtime.ExecRequest{Cmd: []string{"sh", "-c", "for p in $(pgrep -x sleep); do grep -E 'Name|State|PPid' /proc/$p/status; done"}})
			t.Fatalf("sleep still running in the guest after the timeout: pgrep count = %q\n%s", res.Stdout, ps.Stdout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestExecStreamStopsWhenTheCallerCancels(t *testing.T) {
	d := streamVM(t, "stream3")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()
	start := time.Now()

	_, err := d.ExecStream(ctx, "stream3", runtime.ExecRequest{Cmd: []string{"sleep", "60"}}, func(runtime.StreamChunk) {})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("ExecStream err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("ExecStream returned %v after the cancel, want promptly", took)
	}
}

func TestExecStreamOnAnUnknownSandboxIsNotFound(t *testing.T) {
	d := testDriver(t)

	_, err := d.ExecStream(context.Background(), "nope", runtime.ExecRequest{Cmd: []string{"true"}}, func(runtime.StreamChunk) {})

	if !errors.Is(err, runtime.ErrSandboxNotFound) {
		t.Errorf("ExecStream err = %v, want ErrSandboxNotFound", err)
	}
}
