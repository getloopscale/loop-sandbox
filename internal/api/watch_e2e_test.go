package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// watchDriver is a driver whose watch operations answer from fixed data.
type watchDriver struct {
	runtime.Driver
	err       error
	path      string
	recursive bool
	wait      time.Duration
}

func (d *watchDriver) StartWatch(_ context.Context, _ runtime.SandboxID, path string, recursive bool) (string, error) {
	d.path, d.recursive = path, recursive
	return "wch_1", d.err
}

func (d *watchDriver) WatchEvents(_ context.Context, _ runtime.SandboxID, _ string, after int64, wait time.Duration) (*runtime.FileEvents, error) {
	if d.err != nil {
		return nil, d.err
	}
	d.wait = wait
	return &runtime.FileEvents{Next: after + 2, Events: []runtime.FileEvent{
		{Type: "create", Path: "/workspace/out/a.txt"}, {Type: "overflow"},
	}}, nil
}

func (d *watchDriver) StopWatch(context.Context, runtime.SandboxID, string) error { return d.err }

func TestStartWatchAnswers201WithTheCleanPath(t *testing.T) {
	d := &watchDriver{Driver: fake.New()}
	srv := processServer(d)
	sb := createWith(t, srv, "key-a", `{}`)

	rec := call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/watches", "key-a", `{"path":"/workspace/out/","recursive":true}`)

	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"watch_id":"wch_1"`) || !strings.Contains(rec.Body.String(), `"path":"/workspace/out"`) {
		t.Errorf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if d.path != "/workspace/out" || !d.recursive {
		t.Errorf("driver got path %q recursive %v", d.path, d.recursive)
	}
}

func TestStartWatchRejectsInvalidRequests(t *testing.T) {
	cases := map[string]struct {
		key, body string
		err       error
		want      int
		code      string
	}{
		"path outside":      {"key-a", `{"path":"/etc"}`, nil, http.StatusBadRequest, "invalid_request"},
		"no path":           {"key-a", `{}`, nil, http.StatusBadRequest, "invalid_request"},
		"malformed JSON":    {"key-a", `{"path":`, nil, http.StatusBadRequest, "invalid_request"},
		"missing directory": {"key-a", `{"path":"/workspace/nope"}`, runtime.ErrFileNotFound, http.StatusNotFound, "not_found"},
		"not a directory":   {"key-a", `{"path":"/workspace/a.txt"}`, runtime.ErrNotDirectory, http.StatusBadRequest, "not a directory"},
		"too many watches":  {"key-a", `{"path":"/workspace"}`, runtime.ErrTooManyWatches, http.StatusConflict, "too_many_watches"},
		"another tenant":    {"key-b", `{"path":"/workspace"}`, nil, http.StatusNotFound, "not_found"},
		"without the scope": {"key-noscope", `{"path":"/workspace"}`, nil, http.StatusForbidden, "forbidden"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := processServer(&watchDriver{Driver: fake.New(), err: c.err})
			sb := createWith(t, srv, "key-a", `{}`)

			rec := call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/watches", c.key, c.body)

			if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.code) {
				t.Errorf("status = %d body = %s, want %d with %q", rec.Code, rec.Body.String(), c.want, c.code)
			}
		})
	}
}

func TestWatchEventsReturnsEventsAndTheCursor(t *testing.T) {
	d := &watchDriver{Driver: fake.New()}
	srv := processServer(d)
	sb := createWith(t, srv, "key-a", `{}`)

	got := decode[struct {
		Events []struct {
			Type  string `json:"type"`
			Path  string `json:"path"`
			IsDir bool   `json:"is_dir"`
		} `json:"events"`
		Next      int64 `json:"next"`
		Truncated bool  `json:"truncated"`
		Closed    bool  `json:"closed"`
	}](t, call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/watches/wch_1/events?after=5&wait_ms=250", "key-a", ""), http.StatusOK)

	if len(got.Events) != 2 || got.Events[0].Type != "create" || got.Events[0].Path != "/workspace/out/a.txt" || got.Events[1].Type != "overflow" || got.Next != 7 {
		t.Errorf("events = %+v", got)
	}
	if d.wait != 250*time.Millisecond {
		t.Errorf("wait given to the driver = %s, want 250ms", d.wait)
	}
}

func TestWatchEventsRejectsInvalidCursorsAndWaits(t *testing.T) {
	srv := processServer(&watchDriver{Driver: fake.New()})
	sb := createWith(t, srv, "key-a", `{}`)
	for _, query := range []string{"after=-1", "after=x", "wait_ms=30001", "wait_ms=-5", "wait_ms=99999999999999999"} {
		t.Run(query, func(t *testing.T) {
			rec := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/watches/wch_1/events?"+query, "key-a", "")

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAnUnknownWatchIs404(t *testing.T) {
	srv := processServer(&watchDriver{Driver: fake.New(), err: runtime.ErrWatchNotFound})
	sb := createWith(t, srv, "key-a", `{}`)

	events := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/watches/wch_x/events", "key-a", "")
	stop := call(srv, http.MethodDelete, "/v1/sandboxes/"+sb.ID+"/watches/wch_x", "key-a", "")

	if events.Code != http.StatusNotFound || stop.Code != http.StatusNotFound {
		t.Errorf("events = %d, stop = %d; want 404 and 404", events.Code, stop.Code)
	}
}

func TestStopWatchAnswers204(t *testing.T) {
	srv := processServer(&watchDriver{Driver: fake.New()})
	sb := createWith(t, srv, "key-a", `{}`)

	rec := call(srv, http.MethodDelete, "/v1/sandboxes/"+sb.ID+"/watches/wch_1", "key-a", "")

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
}

func TestWatchRoutesAnswer501OnADriverWithoutWatches(t *testing.T) {
	srv := processServer(plainDriver{fake.New()})
	sb := createWith(t, srv, "key-a", `{}`)
	routes := map[string]struct{ method, path, body string }{
		"start":  {http.MethodPost, "", `{"path":"/workspace"}`},
		"events": {http.MethodGet, "/wch_1/events", ""},
		"stop":   {http.MethodDelete, "/wch_1", ""},
	}
	for name, route := range routes {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, route.method, "/v1/sandboxes/"+sb.ID+"/watches"+route.path, "key-a", route.body)

			if rec.Code != http.StatusNotImplemented {
				t.Errorf("status = %d, want 501 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}
