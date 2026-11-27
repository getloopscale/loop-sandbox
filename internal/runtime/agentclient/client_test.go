package agentclient_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/agentclient"
)

// answering returns a client whose agent always answers resp, and records the
// last request and timeout.
func answering(resp *agentproto.Response, got **agentproto.Request, timeout *time.Duration) agentclient.Client {
	return agentclient.Client{RoundTrip: func(_ context.Context, _ runtime.SandboxID, req *agentproto.Request, t time.Duration) (*agentproto.Response, error) {
		if got != nil {
			*got = req
		}
		if timeout != nil {
			*timeout = t
		}
		return resp, nil
	}}
}

func TestProcessErrorsOfTheAgentBecomeRuntimeErrors(t *testing.T) {
	cases := map[string]error{
		agentproto.ErrNotFound:         runtime.ErrProcessNotFound,
		agentproto.ErrProcessExited:    runtime.ErrProcessExited,
		agentproto.ErrStdinClosed:      runtime.ErrStdinClosed,
		agentproto.ErrTooManyProcesses: runtime.ErrTooManyProcesses,
		agentproto.ErrStartFailed:      runtime.ErrInvalidRequest,
	}
	for agentErr, want := range cases {
		t.Run(agentErr, func(t *testing.T) {
			c := answering(&agentproto.Response{Err: agentErr, Stderr: []byte("why")}, nil, nil)

			err := c.SignalProcess(context.Background(), "sbx_1", 7, "TERM")

			if !errors.Is(err, want) {
				t.Errorf("err = %v, want %v", err, want)
			}
		})
	}
}

func TestWatchErrorsOfTheAgentBecomeRuntimeErrors(t *testing.T) {
	c := answering(&agentproto.Response{Err: agentproto.ErrNotFound}, nil, nil)

	_, startErr := c.StartWatch(context.Background(), "sbx_1", "/workspace/nope", false)
	_, eventsErr := c.WatchEvents(context.Background(), "sbx_1", "wch_1", 0, 0)

	if !errors.Is(startErr, runtime.ErrFileNotFound) || !errors.Is(eventsErr, runtime.ErrWatchNotFound) {
		t.Errorf("start err = %v, events err = %v; want file not found and watch not found", startErr, eventsErr)
	}
}

func TestAWatchPathOutsideTheWorkspaceIsAnInvalidRequest(t *testing.T) {
	c := answering(&agentproto.Response{Err: agentproto.ErrInvalidPath}, nil, nil)

	_, err := c.StartWatch(context.Background(), "sbx_1", "/workspace/escape", false)

	if !errors.Is(err, runtime.ErrInvalidRequest) {
		t.Errorf("err = %v, want an invalid request", err)
	}
}

func TestAnUnknownAgentErrorIsNotSwallowed(t *testing.T) {
	c := answering(&agentproto.Response{Err: "disk on fire"}, nil, nil)

	_, err := c.ListProcesses(context.Background(), "sbx_1")

	if err == nil || errors.Is(err, runtime.ErrProcessNotFound) {
		t.Errorf("err = %v, want an error that carries the agent's message", err)
	}
}

func TestProcessOutputGivesTheAgentMoreTimeThanTheWait(t *testing.T) {
	var req *agentproto.Request
	var timeout time.Duration
	c := answering(&agentproto.Response{Stdout: []byte("out"), Next: 3, Process: &agentproto.ProcessInfo{PID: 7, StartedAt: 1_700_000_000_000}}, &req, &timeout)

	out, err := c.ProcessOutput(context.Background(), "sbx_1", 7, 0, 5*time.Second)

	if err != nil || string(out.Stdout) != "out" || out.Next != 3 || out.Process.StartedAt.Unix() != 1_700_000_000 {
		t.Fatalf("out = %+v err = %v", out, err)
	}
	if req.WaitMS != 5000 || timeout <= 5*time.Second {
		t.Errorf("wait sent = %dms, round trip timeout = %s; want 5000ms and a longer timeout", req.WaitMS, timeout)
	}
}

func TestAnAnswerWithoutTheProcessIsAnError(t *testing.T) {
	c := answering(&agentproto.Response{}, nil, nil)

	_, startErr := c.StartProcess(context.Background(), "sbx_1", runtime.ProcessSpec{Cmd: []string{"ls"}})
	_, watchErr := c.StartWatch(context.Background(), "sbx_1", "/workspace", false)

	if startErr == nil || watchErr == nil {
		t.Errorf("start err = %v, watch err = %v; want both to fail", startErr, watchErr)
	}
}
