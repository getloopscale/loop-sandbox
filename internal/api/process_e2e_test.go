package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// processDriver is a driver whose process operations answer from fixed data and
// record what they were asked.
type processDriver struct {
	runtime.Driver
	started  runtime.ProcessSpec
	startErr error
	opErr    error
	wait     time.Duration
	signal   string
	stdin    string
}

var epoch = time.Unix(1_700_000_000, 0).UTC()

func (d *processDriver) StartProcess(_ context.Context, _ runtime.SandboxID, spec runtime.ProcessSpec) (*runtime.ProcessInfo, error) {
	if d.startErr != nil {
		return nil, d.startErr
	}
	d.started = spec
	return &runtime.ProcessInfo{PID: 7, Cmd: spec.Cmd, StartedAt: epoch, Running: true}, nil
}

func (d *processDriver) ListProcesses(context.Context, runtime.SandboxID) ([]runtime.ProcessInfo, error) {
	return []runtime.ProcessInfo{
		{PID: 7, Cmd: []string{"sleep", "30"}, StartedAt: epoch, Running: true},
		{PID: 9, Cmd: []string{"false"}, StartedAt: epoch, ExitCode: 1},
	}, nil
}

func (d *processDriver) ProcessOutput(_ context.Context, _ runtime.SandboxID, pid int, after int64, wait time.Duration) (*runtime.ProcessOutput, error) {
	if d.opErr != nil {
		return nil, d.opErr
	}
	d.wait = wait
	return &runtime.ProcessOutput{Stdout: []byte("out"), Stderr: []byte("err"), Next: after + 6,
		Process: runtime.ProcessInfo{PID: pid, Cmd: []string{"x"}, StartedAt: epoch, ExitCode: 3}}, nil
}

func (d *processDriver) WriteProcessStdin(_ context.Context, _ runtime.SandboxID, _ int, data []byte) error {
	d.stdin = string(data)
	return d.opErr
}

func (d *processDriver) CloseProcessStdin(context.Context, runtime.SandboxID, int) error {
	return d.opErr
}

func (d *processDriver) SignalProcess(_ context.Context, _ runtime.SandboxID, _ int, signal string) error {
	d.signal = signal
	return d.opErr
}

func processServer(d runtime.Driver) http.Handler {
	svc := app.NewService(memory.New(), d, clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	return api.NewRouter(api.NewHandler(svc, nil), auth.Middleware(stubAuth{}), nil)
}

type processBody struct {
	PID       int      `json:"pid"`
	Cmd       []string `json:"cmd"`
	StartedAt string   `json:"started_at"`
	Running   bool     `json:"running"`
	ExitCode  *int     `json:"exit_code"`
}

func TestStartProcessAnswers201WithARunningProcess(t *testing.T) {
	d := &processDriver{Driver: fake.New()}
	srv := processServer(d)
	sb := createWith(t, srv, "key-a", `{"env":{"FROM_SANDBOX":"1","SHARED":"sandbox"}}`)

	got := decode[processBody](t, call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/processes", "key-a",
		`{"cmd":["sleep","30"],"env":{"SHARED":"request"},"workdir":"/workspace/app/"}`), http.StatusCreated)

	if got.PID != 7 || !got.Running || got.ExitCode != nil || got.StartedAt == "" || len(got.Cmd) != 2 {
		t.Errorf("process = %+v, want pid 7 running with a null exit code", got)
	}
	if d.started.WorkingDir != "/workspace/app" || d.started.Env["FROM_SANDBOX"] != "1" || d.started.Env["SHARED"] != "request" {
		t.Errorf("spec given to the driver = %+v, want a clean workdir and the sandbox env under the request's", d.started)
	}
}

func TestStartProcessRejectsInvalidRequests(t *testing.T) {
	cases := map[string]struct {
		key, body string
		startErr  error
		want      int
		code      string
	}{
		"no command":           {"key-a", `{"cmd":[]}`, nil, http.StatusBadRequest, "invalid_request"},
		"empty command name":   {"key-a", `{"cmd":[""]}`, nil, http.StatusBadRequest, "invalid_request"},
		"workdir outside":      {"key-a", `{"cmd":["ls"],"workdir":"/etc"}`, nil, http.StatusBadRequest, "invalid_request"},
		"malformed JSON":       {"key-a", `{"cmd":`, nil, http.StatusBadRequest, "invalid_request"},
		"command cannot start": {"key-a", `{"cmd":["nope"]}`, runtime.ErrInvalidRequest, http.StatusBadRequest, "invalid_request"},
		"too many processes":   {"key-a", `{"cmd":["ls"]}`, runtime.ErrTooManyProcesses, http.StatusConflict, "too_many_processes"},
		"another tenant":       {"key-b", `{"cmd":["ls"]}`, nil, http.StatusNotFound, "not_found"},
		"without the scope":    {"key-noscope", `{"cmd":["ls"]}`, nil, http.StatusForbidden, "forbidden"},
		"without credentials":  {"", `{"cmd":["ls"]}`, nil, http.StatusUnauthorized, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := processServer(&processDriver{Driver: fake.New(), startErr: c.startErr})
			sb := createWith(t, srv, "key-a", `{}`)

			rec := call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/processes", c.key, c.body)

			if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.code) {
				t.Errorf("status = %d body = %s, want %d with code %q", rec.Code, rec.Body.String(), c.want, c.code)
			}
		})
	}
}

func TestListProcessesShowsExitCodeOnlyForEndedProcesses(t *testing.T) {
	srv := processServer(&processDriver{Driver: fake.New()})
	sb := createWith(t, srv, "key-a", `{}`)

	got := decode[struct {
		Processes []processBody `json:"processes"`
	}](t, call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/processes", "key-a", ""), http.StatusOK)

	if len(got.Processes) != 2 || got.Processes[0].ExitCode != nil || got.Processes[1].ExitCode == nil || *got.Processes[1].ExitCode != 1 {
		t.Errorf("processes = %+v, want a running one without exit code and an ended one with 1", got.Processes)
	}
}

func TestGetProcessFindsOneByPID(t *testing.T) {
	srv := processServer(&processDriver{Driver: fake.New()})
	sb := createWith(t, srv, "key-a", `{}`)
	cases := map[string]int{"9": http.StatusOK, "8": http.StatusNotFound, "abc": http.StatusNotFound, "-1": http.StatusNotFound}
	for pid, want := range cases {
		t.Run(pid, func(t *testing.T) {
			rec := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/processes/"+pid, "key-a", "")

			if rec.Code != want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, want, rec.Body.String())
			}
		})
	}
}

func TestProcessOutputReturnsTheStreamsTheCursorAndTheProcess(t *testing.T) {
	d := &processDriver{Driver: fake.New()}
	srv := processServer(d)
	sb := createWith(t, srv, "key-a", `{}`)

	got := decode[struct {
		Stdout, Stderr string
		Next           int64
		Truncated      bool
		Process        processBody
	}](t, call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/processes/7/output?after=10&wait_ms=1500", "key-a", ""), http.StatusOK)

	if got.Stdout != "out" || got.Stderr != "err" || got.Next != 16 || got.Process.ExitCode == nil || *got.Process.ExitCode != 3 {
		t.Errorf("output = %+v", got)
	}
	if d.wait != 1500*time.Millisecond {
		t.Errorf("wait given to the driver = %s, want 1.5s", d.wait)
	}
}

func TestProcessOutputRejectsInvalidCursorsAndWaits(t *testing.T) {
	srv := processServer(&processDriver{Driver: fake.New()})
	sb := createWith(t, srv, "key-a", `{}`)
	for _, query := range []string{"after=-1", "after=x", "wait_ms=30001", "wait_ms=-5", "wait_ms=99999999999999999", "wait_ms=1e3"} {
		t.Run(query, func(t *testing.T) {
			rec := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/processes/7/output?"+query, "key-a", "")

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestProcessOperationsMapDriverErrors(t *testing.T) {
	cases := map[string]struct {
		err  error
		want int
		code string
	}{
		"unknown process": {runtime.ErrProcessNotFound, http.StatusNotFound, "not_found"},
		"ended process":   {runtime.ErrProcessExited, http.StatusConflict, "process_exited"},
		"closed stdin":    {runtime.ErrStdinClosed, http.StatusConflict, "stdin_closed"},
	}
	routes := map[string]struct{ method, path, body string }{
		"output": {http.MethodGet, "/output", ""},
		"stdin":  {http.MethodPost, "/stdin", "data"},
		"close":  {http.MethodDelete, "/stdin", ""},
		"signal": {http.MethodPost, "/signal", `{"signal":"TERM"}`},
	}
	for name, c := range cases {
		for routeName, route := range routes {
			t.Run(name+"/"+routeName, func(t *testing.T) {
				srv := processServer(&processDriver{Driver: fake.New(), opErr: c.err})
				sb := createWith(t, srv, "key-a", `{}`)

				rec := call(srv, route.method, "/v1/sandboxes/"+sb.ID+"/processes/7"+route.path, "key-a", route.body)

				if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.code) {
					t.Errorf("status = %d body = %s, want %d with code %q", rec.Code, rec.Body.String(), c.want, c.code)
				}
			})
		}
	}
}

func TestProcessStdinPassesTheBodyAsIsAndLimitsItsSize(t *testing.T) {
	d := &processDriver{Driver: fake.New()}
	srv := processServer(d)
	sb := createWith(t, srv, "key-a", `{}`)
	path := "/v1/sandboxes/" + sb.ID + "/processes/7/stdin"

	written := call(srv, http.MethodPost, path, "key-a", "line one\n\x00two")
	tooBig := call(srv, http.MethodPost, path, "key-a", strings.Repeat("x", app.MaxProcessStdinBytes+1))

	if written.Code != http.StatusNoContent || d.stdin != "line one\n\x00two" {
		t.Errorf("write = %d, driver got %q", written.Code, d.stdin)
	}
	if tooBig.Code != http.StatusBadRequest {
		t.Errorf("a body over the limit = %d, want 400", tooBig.Code)
	}
}

func TestSignalProcessAcceptsOnlyKnownSignals(t *testing.T) {
	cases := map[string]int{
		`{"signal":"TERM"}`: http.StatusNoContent, `{"signal":"KILL"}`: http.StatusNoContent,
		`{"signal":"term"}`: http.StatusBadRequest, `{"signal":"SIGTERM"}`: http.StatusBadRequest,
		`{"signal":"9"}`: http.StatusBadRequest, `{}`: http.StatusBadRequest, `not json`: http.StatusBadRequest,
	}
	for body, want := range cases {
		t.Run(body, func(t *testing.T) {
			d := &processDriver{Driver: fake.New()}
			srv := processServer(d)
			sb := createWith(t, srv, "key-a", `{}`)

			rec := call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/processes/7/signal", "key-a", body)

			if rec.Code != want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, want, rec.Body.String())
			}
			if want != http.StatusNoContent && d.signal != "" {
				t.Errorf("a refused signal reached the driver: %q", d.signal)
			}
		})
	}
}

func TestProcessRoutesAnswer501OnADriverWithoutProcesses(t *testing.T) {
	srv := processServer(plainDriver{fake.New()})
	sb := createWith(t, srv, "key-a", `{}`)
	routes := map[string]struct{ method, path, body string }{
		"start":  {http.MethodPost, "", `{"cmd":["ls"]}`},
		"list":   {http.MethodGet, "", ""},
		"get":    {http.MethodGet, "/7", ""},
		"output": {http.MethodGet, "/7/output", ""},
		"stdin":  {http.MethodPost, "/7/stdin", "x"},
		"close":  {http.MethodDelete, "/7/stdin", ""},
		"signal": {http.MethodPost, "/7/signal", `{"signal":"TERM"}`},
	}
	for name, route := range routes {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, route.method, "/v1/sandboxes/"+sb.ID+"/processes"+route.path, "key-a", route.body)

			if rec.Code != http.StatusNotImplemented {
				t.Errorf("status = %d, want 501 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}
