package api_test

import (
	"net/http"
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

type fileInfoBody struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	IsDir      bool   `json:"is_dir"`
	Mode       string `json:"mode"`
	ModifiedAt string `json:"modified_at"`
}

func putFile(t *testing.T, srv http.Handler, id, path, content string) {
	t.Helper()
	if rec := call(srv, http.MethodPut, "/v1/sandboxes/"+id+"/files"+path, "key-a", content); rec.Code != http.StatusNoContent {
		t.Fatalf("put %s = %d (%s)", path, rec.Code, rec.Body.String())
	}
}

func move(srv http.Handler, id, key, body string) int {
	return call(srv, http.MethodPost, "/v1/sandboxes/"+id+"/file-moves", key, body).Code
}

func TestFileInfoDescribesAFile(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, sb.ID, "/workspace/dir/a.txt", "hello")

	info := decode[fileInfoBody](t, call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/file-info/workspace/dir/a.txt", "key-a", ""), http.StatusOK)

	if info.Name != "a.txt" || info.Path != "/workspace/dir/a.txt" || info.Size != 5 || info.IsDir || info.Mode != "0644" || info.ModifiedAt == "" {
		t.Errorf("info = %+v", info)
	}
}

func TestFileInfoDescribesADirectory(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, sb.ID, "/workspace/dir/a.txt", "hello")

	info := decode[fileInfoBody](t, call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/file-info/workspace/dir", "key-a", ""), http.StatusOK)

	if !info.IsDir || info.Size != 0 || info.Name != "dir" {
		t.Errorf("info = %+v, want the directory dir", info)
	}
}

func TestFileInfoErrors(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, sb.ID, "/workspace/a.txt", "x")
	cases := map[string]struct {
		key, path string
		want      int
	}{
		"missing path":           {"key-a", "/workspace/nope", http.StatusNotFound},
		"path outside workspace": {"key-a", "/etc/passwd", http.StatusBadRequest},
		"another tenant":         {"key-b", "/workspace/a.txt", http.StatusNotFound},
		"without credentials":    {"", "/workspace/a.txt", http.StatusUnauthorized},
		"without the scope":      {"key-noscope", "/workspace/a.txt", http.StatusForbidden},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/file-info"+c.path, c.key, "")

			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestMakeDirectoryIsRepeatableAndVisible(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{}`)
	path := "/v1/sandboxes/" + sb.ID + "/directories/workspace/a/b"

	first := call(srv, http.MethodPut, path, "key-a", "")
	second := call(srv, http.MethodPut, path, "key-a", "")
	info := decode[fileInfoBody](t, call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/file-info/workspace/a/b", "key-a", ""), http.StatusOK)

	if first.Code != http.StatusNoContent || second.Code != http.StatusNoContent {
		t.Errorf("mkdir = %d then %d, want 204 and 204", first.Code, second.Code)
	}
	if !info.IsDir {
		t.Errorf("info = %+v, want a directory", info)
	}
}

func TestMakeDirectoryOverAFileIs409(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, sb.ID, "/workspace/a.txt", "keep")

	rec := call(srv, http.MethodPut, "/v1/sandboxes/"+sb.ID+"/directories/workspace/a.txt", "key-a", "")

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
}

func TestMoveRenamesAFile(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, sb.ID, "/workspace/a.txt", "hello")

	status := move(srv, sb.ID, "key-a", `{"from":"/workspace/a.txt","to":"/workspace/new/b.txt"}`)
	moved := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files/workspace/new/b.txt", "key-a", "")
	old := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files/workspace/a.txt", "key-a", "")

	if status != http.StatusNoContent {
		t.Fatalf("move = %d, want 204", status)
	}
	if moved.Code != http.StatusOK || moved.Body.String() != "hello" {
		t.Errorf("destination = %d %q, want 200 hello", moved.Code, moved.Body.String())
	}
	if old.Code != http.StatusNotFound {
		t.Errorf("source after the move = %d, want 404", old.Code)
	}
}

func TestMoveOntoAnExistingPathIs409UnlessOverwriteIsSet(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, sb.ID, "/workspace/a.txt", "source")
	putFile(t, srv, sb.ID, "/workspace/b.txt", "destination")

	refused := call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/file-moves", "key-a", `{"from":"/workspace/a.txt","to":"/workspace/b.txt"}`)
	kept := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files/workspace/b.txt", "key-a", "")
	forced := move(srv, sb.ID, "key-a", `{"from":"/workspace/a.txt","to":"/workspace/b.txt","overwrite":true}`)
	replaced := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files/workspace/b.txt", "key-a", "")

	if refused.Code != http.StatusConflict || kept.Body.String() != "destination" {
		t.Errorf("without overwrite: status %d, destination %q; want 409 and destination", refused.Code, kept.Body.String())
	}
	if forced != http.StatusNoContent || replaced.Body.String() != "source" {
		t.Errorf("with overwrite: status %d, destination %q; want 204 and source", forced, replaced.Body.String())
	}
}

func TestMoveRejectsInvalidRequests(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, sb.ID, "/workspace/dir/a.txt", "x")
	cases := map[string]struct {
		key, body string
		want      int
	}{
		"missing source":      {"key-a", `{"from":"/workspace/nope","to":"/workspace/b"}`, http.StatusNotFound},
		"source outside":      {"key-a", `{"from":"/etc/passwd","to":"/workspace/b"}`, http.StatusBadRequest},
		"destination outside": {"key-a", `{"from":"/workspace/dir/a.txt","to":"/etc/evil"}`, http.StatusBadRequest},
		"dot-dot escape":      {"key-a", `{"from":"/workspace/dir/a.txt","to":"/workspace/../evil"}`, http.StatusBadRequest},
		"same path":           {"key-a", `{"from":"/workspace/dir","to":"/workspace/dir/"}`, http.StatusBadRequest},
		"into itself":         {"key-a", `{"from":"/workspace/dir","to":"/workspace/dir/sub"}`, http.StatusBadRequest},
		"the workspace root":  {"key-a", `{"from":"/workspace","to":"/workspace/x"}`, http.StatusBadRequest},
		"onto the root":       {"key-a", `{"from":"/workspace/dir","to":"/workspace"}`, http.StatusBadRequest},
		"empty body":          {"key-a", `{}`, http.StatusBadRequest},
		"malformed JSON":      {"key-a", `{"from":`, http.StatusBadRequest},
		"another tenant":      {"key-b", `{"from":"/workspace/dir/a.txt","to":"/workspace/b"}`, http.StatusNotFound},
		"without the scope":   {"key-noscope", `{"from":"/workspace/dir/a.txt","to":"/workspace/b"}`, http.StatusForbidden},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := move(srv, sb.ID, c.key, c.body); got != c.want {
				t.Errorf("status = %d, want %d", got, c.want)
			}
		})
	}
	if rec := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files/workspace/dir/a.txt", "key-a", ""); rec.Body.String() != "x" {
		t.Errorf("the file changed after refused moves: %d %q", rec.Code, rec.Body.String())
	}
}

// plainDriver hides every optional interface of the fake, leaving only runtime.Driver.
type plainDriver struct{ runtime.Driver }

func TestFileOpsAnswer501OnADriverWithoutThem(t *testing.T) {
	svc := app.NewService(memory.New(), plainDriver{fake.New()}, clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	srv := api.NewRouter(api.NewHandler(svc, nil), auth.Middleware(stubAuth{}), nil)
	sb := createWith(t, srv, "key-a", `{}`)
	cases := map[string]struct{ method, path, body string }{
		"file info": {http.MethodGet, "/file-info/workspace/a", ""},
		"mkdir":     {http.MethodPut, "/directories/workspace/a", ""},
		"move":      {http.MethodPost, "/file-moves", `{"from":"/workspace/a","to":"/workspace/b"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, c.method, "/v1/sandboxes/"+sb.ID+c.path, "key-a", c.body)

			if rec.Code != http.StatusNotImplemented {
				t.Errorf("status = %d, want 501 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}
