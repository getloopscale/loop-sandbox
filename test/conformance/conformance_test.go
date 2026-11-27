//go:build conformance

package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func randomName(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random name: %v", err)
	}
	return prefix + hex.EncodeToString(b)
}

// The lifecycle is not an optional capability: every driver must pass it. It is
// recorded under "lifecycle" in the summary.
func TestLifecycle(t *testing.T) {
	supported["lifecycle"] = true
	capability(t, "lifecycle", nil, map[string]func(t *testing.T){
		"sandbox_past_its_ttl_is_destroyed": func(t *testing.T) {
			resp := call(t, apiKey, http.MethodPost, "/v1/sandboxes", map[string]any{"image": shellImage, "ttl_seconds": 2})
			resp.want(t, http.StatusCreated, "create a sandbox with a 2s TTL")
			var sb sandbox
			resp.json(t, &sb)
			t.Cleanup(func() { destroy(t, sb.ID) })

			// The deployment's reconcile interval bounds how late the destroy can be.
			deadline := time.Now().Add(ttlGrace)
			for {
				status := call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID, nil).status
				if status == http.StatusNotFound {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("sandbox %s still answers %d, %v after its 2s TTL", sb.ID, status, ttlGrace)
				}
				time.Sleep(500 * time.Millisecond)
			}
		},
		"create_returns_a_running_sandbox_with_an_expiry": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			if sb.State != "running" {
				t.Errorf("state = %q, want running", sb.State)
			}
			if sb.ExpiresAt == "" {
				t.Error("expires_at is empty for a sandbox created with ttl_seconds")
			}
		},
		"get_returns_the_created_sandbox": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			resp := call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID, nil).want(t, http.StatusOK, "get")

			var got sandbox
			resp.json(t, &got)
			if got.ID != sb.ID || got.State != "running" {
				t.Errorf("got id=%q state=%q, want id=%q state=running", got.ID, got.State, sb.ID)
			}
		},
		"get_after_destroy_answers_404": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			call(t, apiKey, http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil).want(t, http.StatusNoContent, "destroy")

			call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID, nil).want(t, http.StatusNotFound, "get after destroy")
		},
		"another_tenant_cannot_see_the_sandbox": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			call(t, otherAPIKey, http.MethodGet, "/v1/sandboxes/"+sb.ID, nil).want(t, http.StatusNotFound, "get as other tenant")
		},
		"another_tenant_cannot_destroy_the_sandbox": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			call(t, otherAPIKey, http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil).want(t, http.StatusNotFound, "destroy as other tenant")

			call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID, nil).want(t, http.StatusOK, "get as owner")
		},
		"created_sandbox_returns_its_metadata_and_env": func(t *testing.T) {
			sb := newSandboxWith(t, map[string]any{
				"metadata": map[string]string{"suite": "conformance"},
				"env":      map[string]string{"LSB_MODE": "test"},
			})

			var got sandbox
			call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID, nil).want(t, http.StatusOK, "get").json(t, &got)

			if got.Metadata["suite"] != "conformance" || got.Env["LSB_MODE"] != "test" {
				t.Errorf("metadata=%v env=%v, want suite=conformance and LSB_MODE=test", got.Metadata, got.Env)
			}
		},
		"list_filtered_by_metadata_returns_only_matching_sandboxes": func(t *testing.T) {
			run := randomName(t, "run-")
			a := newSandboxWith(t, map[string]any{"metadata": map[string]string{"run": run}})
			b := newSandboxWith(t, map[string]any{"metadata": map[string]string{"run": run}})
			newSandboxWith(t, map[string]any{"metadata": map[string]string{"run": run + "-other"}})

			var first, second sandboxList
			path := "/v1/sandboxes?limit=1&state=running&metadata=" + url.QueryEscape("run="+run)
			call(t, apiKey, http.MethodGet, path, nil).want(t, http.StatusOK, "first page").json(t, &first)
			if first.NextCursor == "" {
				t.Fatalf("first page of 2 matches with limit=1 has no next_cursor: %+v", first)
			}
			call(t, apiKey, http.MethodGet, path+"&cursor="+url.QueryEscape(first.NextCursor), nil).
				want(t, http.StatusOK, "second page").json(t, &second)

			if len(first.Sandboxes) != 1 || len(second.Sandboxes) != 1 {
				t.Fatalf("pages have %d and %d sandboxes, want 1 and 1", len(first.Sandboxes), len(second.Sandboxes))
			}
			got := map[string]bool{first.Sandboxes[0].ID: true, second.Sandboxes[0].ID: true}
			if !got[a.ID] || !got[b.ID] {
				t.Errorf("pages returned %v, want %s and %s", got, a.ID, b.ID)
			}
			if second.NextCursor != "" {
				t.Errorf("last page still has next_cursor %q", second.NextCursor)
			}
		},
		"list_never_shows_another_tenants_sandbox": func(t *testing.T) {
			run := randomName(t, "run-")
			newSandboxWith(t, map[string]any{"metadata": map[string]string{"run": run}})

			var list sandboxList
			call(t, otherAPIKey, http.MethodGet, "/v1/sandboxes?metadata="+url.QueryEscape("run="+run), nil).
				want(t, http.StatusOK, "list as other tenant").json(t, &list)

			if len(list.Sandboxes) != 0 {
				t.Errorf("other tenant sees %d sandboxes of this tenant", len(list.Sandboxes))
			}
		},
		"timeout_moves_the_expiry": func(t *testing.T) {
			sb := newSandboxWith(t, map[string]any{"ttl_seconds": 60})

			var updated sandbox
			call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/timeout", map[string]any{"ttl_seconds": 1800}).
				want(t, http.StatusOK, "set timeout").json(t, &updated)

			before, err1 := time.Parse(time.RFC3339, sb.ExpiresAt)
			after, err2 := time.Parse(time.RFC3339, updated.ExpiresAt)
			if err1 != nil || err2 != nil {
				t.Fatalf("expires_at not RFC 3339: %q, %q", sb.ExpiresAt, updated.ExpiresAt)
			}
			// 60s became 1800s; the difference is 1740s give or take the request time.
			if moved := after.Sub(before); moved < 1700*time.Second || moved > 1780*time.Second {
				t.Errorf("expiry moved by %v, want about 1740s", moved)
			}
		},
		"sandbox_with_a_shortened_timeout_is_destroyed": func(t *testing.T) {
			sb := newSandboxWith(t, map[string]any{"ttl_seconds": 600})

			call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/timeout", map[string]any{"ttl_seconds": 1}).
				want(t, http.StatusOK, "shorten timeout")

			deadline := time.Now().Add(ttlGrace)
			for {
				status := call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID, nil).status
				if status == http.StatusNotFound {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("sandbox %s still answers %d, %v after its timeout was cut to 1s", sb.ID, status, ttlGrace)
				}
				time.Sleep(500 * time.Millisecond)
			}
		},
		"request_without_a_key_answers_401": func(t *testing.T) {
			call(t, "", http.MethodPost, "/v1/sandboxes", map[string]any{"image": shellImage}).
				want(t, http.StatusUnauthorized, "create without key")
		},
		"same_idempotency_key_creates_one_sandbox": func(t *testing.T) {
			key := randomName(t, "idem-")
			body := map[string]any{"image": shellImage, "ttl_seconds": 600}

			first := call(t, apiKey, http.MethodPost, "/v1/sandboxes", body, "Idempotency-Key", key).want(t, http.StatusCreated, "first create")
			second := call(t, apiKey, http.MethodPost, "/v1/sandboxes", body, "Idempotency-Key", key).want(t, http.StatusCreated, "replayed create")

			var a, b sandbox
			first.json(t, &a)
			second.json(t, &b)
			t.Cleanup(func() { destroy(t, a.ID) })
			if a.ID != b.ID {
				t.Errorf("replay created a second sandbox: %q and %q", a.ID, b.ID)
			}
			if second.header.Get("Idempotent-Replay") != "true" {
				t.Error("replayed response is missing the Idempotent-Replay: true header")
			}
		},
	})
}

func TestExec(t *testing.T) {
	capability(t, "exec", nil, map[string]func(t *testing.T){
		"command_sees_the_env_given_at_creation": func(t *testing.T) {
			sb := newSandboxWith(t, map[string]any{"env": map[string]string{"LSB_MODE": "from-sandbox"}})

			res := sh(t, sb.ID, `printf %s "$LSB_MODE"`)

			if res.Stdout != "from-sandbox" {
				t.Errorf("stdout = %q (exit %d, stderr %q), want from-sandbox", res.Stdout, res.ExitCode, res.Stderr)
			}
		},
		"env_of_the_execution_wins_over_the_env_of_the_sandbox": func(t *testing.T) {
			sb := newSandboxWith(t, map[string]any{"env": map[string]string{"LSB_MODE": "from-sandbox", "LSB_KEPT": "kept"}})

			res := execIn(t, sb.ID, map[string]any{
				"cmd": []string{"sh", "-c", `printf %s "$LSB_MODE/$LSB_KEPT"`},
				"env": map[string]string{"LSB_MODE": "from-exec"},
			})

			if res.Stdout != "from-exec/kept" {
				t.Errorf("stdout = %q, want from-exec/kept", res.Stdout)
			}
		},
		"returns_stdout_and_exit_code_zero": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			res := execIn(t, sb.ID, map[string]any{"cmd": []string{"echo", "hello"}})

			if res.ExitCode != 0 || res.Stdout != "hello\n" {
				t.Errorf("exit=%d stdout=%q, want 0 and %q", res.ExitCode, res.Stdout, "hello\n")
			}
		},
		"nonzero_exit_is_a_result_not_an_error": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			res := sh(t, sb.ID, "exit 7")

			if res.ExitCode != 7 {
				t.Errorf("exit_code = %d, want 7", res.ExitCode)
			}
		},
		"captures_stderr_separately": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			res := sh(t, sb.ID, "echo out; echo err 1>&2")

			if res.Stdout != "out\n" || res.Stderr != "err\n" {
				t.Errorf("stdout=%q stderr=%q, want %q and %q", res.Stdout, res.Stderr, "out\n", "err\n")
			}
		},
		"passes_environment_variables": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			res := execIn(t, sb.ID, map[string]any{
				"cmd": []string{"sh", "-c", "echo $CONFORMANCE_VAR"},
				"env": map[string]string{"CONFORMANCE_VAR": "from-request"},
			})

			if res.Stdout != "from-request\n" {
				t.Errorf("stdout = %q, want %q", res.Stdout, "from-request\n")
			}
		},
		"honours_the_working_directory": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			sh(t, sb.ID, "mkdir -p /workspace/sub")

			res := execIn(t, sb.ID, map[string]any{"cmd": []string{"pwd"}, "workdir": "/workspace/sub"})

			if res.Stdout != "/workspace/sub\n" {
				t.Errorf("stdout = %q, want %q", res.Stdout, "/workspace/sub\n")
			}
		},
		"command_over_its_timeout_answers_504": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			resp := call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/executions",
				map[string]any{"cmd": []string{"sleep", "30"}, "timeout_ms": 500})

			resp.want(t, http.StatusGatewayTimeout, "exec over timeout")
		},
		"working_directory_outside_the_workspace_answers_400": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/executions",
				map[string]any{"cmd": []string{"pwd"}, "workdir": "/etc"}).want(t, http.StatusBadRequest, "exec with workdir /etc")
		},
		"request_without_cmd_answers_400": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/executions", map[string]any{}).
				want(t, http.StatusBadRequest, "exec without cmd")
		},
		"another_tenant_cannot_exec": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			call(t, otherAPIKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/executions",
				map[string]any{"cmd": []string{"echo", "x"}}).want(t, http.StatusNotFound, "exec as other tenant")
		},
	})
}

func TestFiles(t *testing.T) {
	capability(t, "files", nil, map[string]func(t *testing.T){
		"write_creates_missing_parent_directories": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			writeFile(t, sb.ID, "/workspace/new/deep/a.txt", "deep")

			if resp := readFile(t, sb.ID, "/workspace/new/deep/a.txt"); string(resp.body) != "deep" {
				t.Errorf("read = %d %q, want deep", resp.status, resp.body)
			}
		},
		"write_through_a_symlink_cannot_leave_the_workspace": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			sh(t, sb.ID, "ln -s /tmp /workspace/escape")

			resp := call(t, apiKey, http.MethodPut, "/v1/sandboxes/"+sb.ID+"/files/workspace/escape/a.txt", []byte("x"))

			if resp.status != http.StatusBadRequest {
				t.Errorf("write through a symlink = %d, want 400: %s", resp.status, resp.body)
			}
			if res := sh(t, sb.ID, "test -e /tmp/a.txt"); res.ExitCode == 0 {
				t.Error("the write created /tmp/a.txt through a symlink")
			}
		},
		"read_through_a_symlink_cannot_leave_the_workspace": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			sh(t, sb.ID, "ln -s /etc/hostname /workspace/link")

			resp := readFile(t, sb.ID, "/workspace/link")

			if resp.status != http.StatusBadRequest {
				t.Errorf("read through a symlink = %d, want 400: %s", resp.status, resp.body)
			}
		},
		"read_returns_what_was_written": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/a.txt", "content-a")

			resp := readFile(t, sb.ID, "/workspace/a.txt").want(t, http.StatusOK, "read")

			if string(resp.body) != "content-a" {
				t.Errorf("read = %q, want %q", resp.body, "content-a")
			}
		},
		"written_file_is_visible_to_a_command": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/b.txt", "content-b")

			res := execIn(t, sb.ID, map[string]any{"cmd": []string{"cat", "/workspace/b.txt"}})

			if res.Stdout != "content-b" {
				t.Errorf("cat = %q, want %q", res.Stdout, "content-b")
			}
		},
		"file_created_by_a_command_is_readable": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			sh(t, sb.ID, "printf from-command > /workspace/c.txt")

			resp := readFile(t, sb.ID, "/workspace/c.txt").want(t, http.StatusOK, "read")

			if string(resp.body) != "from-command" {
				t.Errorf("read = %q, want %q", resp.body, "from-command")
			}
		},
		"list_includes_the_file_with_its_size": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/d.txt", "12345")

			resp := call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files?path=/workspace", nil).want(t, http.StatusOK, "list")

			var body struct {
				Files []struct {
					Name string `json:"name"`
					Size int64  `json:"size"`
				} `json:"files"`
			}
			resp.json(t, &body)
			for _, f := range body.Files {
				if strings.HasSuffix(f.Name, "d.txt") {
					if f.Size != 5 {
						t.Errorf("size = %d, want 5", f.Size)
					}
					return
				}
			}
			t.Errorf("d.txt not in listing: %s", resp.body)
		},
		"read_after_delete_answers_404": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/e.txt", "x")

			call(t, apiKey, http.MethodDelete, "/v1/sandboxes/"+sb.ID+"/files/workspace/e.txt", nil).want(t, http.StatusNoContent, "delete")

			readFile(t, sb.ID, "/workspace/e.txt").want(t, http.StatusNotFound, "read after delete")
		},
		"reading_a_missing_file_answers_404": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			readFile(t, sb.ID, "/workspace/missing.txt").want(t, http.StatusNotFound, "read missing")
		},
		"another_tenant_cannot_read_the_file": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/secret.txt", "secret")

			call(t, otherAPIKey, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files/workspace/secret.txt", nil).
				want(t, http.StatusNotFound, "read as other tenant")
		},
	})
}

// streamExec sends a streamed exec and returns the raw response without buffering it.
func streamExec(t *testing.T, id, script string) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"cmd": []string{"sh", "-c", script}})
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/sandboxes/"+id+"/executions?stream=1", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("build streamed exec: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("streamed exec: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestExecStream(t *testing.T) {
	probe := func(t *testing.T) *response {
		sb := newSandbox(t, shellImage)
		return call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/executions?stream=1",
			map[string]any{"cmd": []string{"echo", "x"}})
	}
	capability(t, "exec_stream", probe, map[string]func(t *testing.T){
		"delivers_output_before_the_command_ends": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			resp := streamExec(t, sb.ID, "echo first; sleep 2; echo second")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			events := readSSE(t, resp.Body)

			var first, done *sseEvent
			for i := range events {
				if first == nil && events[i].name == "stdout" && strings.Contains(events[i].data, "first") {
					first = &events[i]
				}
				if events[i].name == "done" {
					done = &events[i]
				}
			}
			if first == nil || done == nil {
				t.Fatalf("stream is missing the first stdout event or the done event: %+v", events)
			}
			if gap := done.at.Sub(first.at); gap < time.Second {
				t.Errorf("first output arrived %v before the end; want at least 1s, the output was buffered", gap)
			}
		},
		"done_event_carries_the_exit_code": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			resp := streamExec(t, sb.ID, "echo x; exit 3")
			events := readSSE(t, resp.Body)

			last := events[len(events)-1]
			var done struct {
				ExitCode int `json:"exit_code"`
			}
			if last.name != "done" || json.Unmarshal([]byte(last.data), &done) != nil || done.ExitCode != 3 {
				t.Errorf("last event = %+v, want done with exit_code 3", last)
			}
		},
	})
}

func TestFileOps(t *testing.T) {
	type fileInfo struct {
		Name       string `json:"name"`
		Size       int64  `json:"size"`
		IsDir      bool   `json:"is_dir"`
		Mode       string `json:"mode"`
		ModifiedAt string `json:"modified_at"`
	}
	stat := func(t *testing.T, id, path string) *response {
		t.Helper()
		return call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+id+"/file-info"+path, nil)
	}
	mkdir := func(t *testing.T, id, path string) *response {
		t.Helper()
		return call(t, apiKey, http.MethodPut, "/v1/sandboxes/"+id+"/directories"+path, nil)
	}
	move := func(t *testing.T, id, from, to string, overwrite bool) *response {
		t.Helper()
		return call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+id+"/file-moves",
			map[string]any{"from": from, "to": to, "overwrite": overwrite})
	}
	probe := func(t *testing.T) *response {
		return stat(t, newSandbox(t, shellImage).ID, "/workspace")
	}
	capability(t, "file_ops", probe, map[string]func(t *testing.T){
		"stat_describes_a_file_written_by_a_command": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			sh(t, sb.ID, "printf 12345 > /workspace/a.txt && chmod 640 /workspace/a.txt")

			var info fileInfo
			stat(t, sb.ID, "/workspace/a.txt").want(t, http.StatusOK, "stat").json(t, &info)

			if info.Name != "a.txt" || info.Size != 5 || info.IsDir || info.Mode != "0640" {
				t.Errorf("info = %+v, want a.txt, 5 bytes, not a directory, mode 0640", info)
			}
			modified, err := time.Parse(time.RFC3339, info.ModifiedAt)
			if err != nil || time.Since(modified) > 10*time.Minute || time.Until(modified) > 10*time.Minute {
				t.Errorf("modified_at = %q, want a time close to now", info.ModifiedAt)
			}
		},
		"stat_of_a_missing_path_answers_404": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			stat(t, sb.ID, "/workspace/nope").want(t, http.StatusNotFound, "stat a missing path")
		},
		"stat_outside_the_workspace_answers_400": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			stat(t, sb.ID, "/etc/passwd").want(t, http.StatusBadRequest, "stat outside the workspace")
		},
		"mkdir_creates_parents_and_a_command_sees_the_directory": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			mkdir(t, sb.ID, "/workspace/a/b/c").want(t, http.StatusNoContent, "mkdir")
			mkdir(t, sb.ID, "/workspace/a/b/c").want(t, http.StatusNoContent, "mkdir again")

			if res := sh(t, sb.ID, "test -d /workspace/a/b/c && touch /workspace/a/b/c/f"); res.ExitCode != 0 {
				t.Errorf("the directory is missing or not writable in the sandbox: exit %d, %s", res.ExitCode, res.Stderr)
			}
			var info fileInfo
			stat(t, sb.ID, "/workspace/a/b").want(t, http.StatusOK, "stat the parent").json(t, &info)
			if !info.IsDir {
				t.Errorf("info of the parent = %+v, want a directory", info)
			}
		},
		"mkdir_over_a_file_answers_409": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/a.txt", "keep")

			mkdir(t, sb.ID, "/workspace/a.txt").want(t, http.StatusConflict, "mkdir over a file")

			if resp := readFile(t, sb.ID, "/workspace/a.txt"); string(resp.body) != "keep" {
				t.Errorf("the file became %q", resp.body)
			}
		},
		"move_renames_a_file_into_a_new_directory": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/a.txt", "hello")

			move(t, sb.ID, "/workspace/a.txt", "/workspace/new/dir/b.txt", false).want(t, http.StatusNoContent, "move")

			if resp := readFile(t, sb.ID, "/workspace/new/dir/b.txt"); string(resp.body) != "hello" {
				t.Errorf("destination = %d %q, want hello", resp.status, resp.body)
			}
			readFile(t, sb.ID, "/workspace/a.txt").want(t, http.StatusNotFound, "read the old path")
		},
		"move_of_a_directory_takes_its_content": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/src/deep/a.txt", "deep")

			move(t, sb.ID, "/workspace/src", "/workspace/dst", false).want(t, http.StatusNoContent, "move a directory")

			if resp := readFile(t, sb.ID, "/workspace/dst/deep/a.txt"); string(resp.body) != "deep" {
				t.Errorf("moved file = %d %q, want deep", resp.status, resp.body)
			}
		},
		"move_onto_an_existing_path_answers_409_and_changes_nothing": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/a.txt", "source")
			writeFile(t, sb.ID, "/workspace/b.txt", "destination")

			move(t, sb.ID, "/workspace/a.txt", "/workspace/b.txt", false).want(t, http.StatusConflict, "move onto an existing file")

			a, b := readFile(t, sb.ID, "/workspace/a.txt"), readFile(t, sb.ID, "/workspace/b.txt")
			if string(a.body) != "source" || string(b.body) != "destination" {
				t.Errorf("after the refused move: a=%q b=%q", a.body, b.body)
			}
		},
		"move_with_overwrite_replaces_the_destination": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/src/new.txt", "new")
			writeFile(t, sb.ID, "/workspace/dst/old.txt", "old")

			move(t, sb.ID, "/workspace/src", "/workspace/dst", true).want(t, http.StatusNoContent, "move with overwrite")

			if resp := readFile(t, sb.ID, "/workspace/dst/new.txt"); string(resp.body) != "new" {
				t.Errorf("dst/new.txt = %d %q, want new", resp.status, resp.body)
			}
			readFile(t, sb.ID, "/workspace/dst/old.txt").want(t, http.StatusNotFound, "read a replaced file")
		},
		"move_of_a_missing_source_answers_404": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			move(t, sb.ID, "/workspace/nope", "/workspace/b", false).want(t, http.StatusNotFound, "move a missing path")
		},
		"move_through_a_symlink_cannot_leave_the_workspace": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/a.txt", "x")
			sh(t, sb.ID, "ln -s /tmp /workspace/escape")

			resp := move(t, sb.ID, "/workspace/a.txt", "/workspace/escape/a.txt", false)

			if res := sh(t, sb.ID, "test -e /tmp/a.txt"); res.ExitCode == 0 {
				t.Errorf("the move (status %d) wrote /tmp/a.txt through a symlink", resp.status)
			}
		},
		"another_tenant_cannot_stat_or_move": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/a.txt", "x")

			call(t, otherAPIKey, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/file-info/workspace/a.txt", nil).
				want(t, http.StatusNotFound, "stat as other tenant")
			call(t, otherAPIKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/file-moves", map[string]any{"from": "/workspace/a.txt", "to": "/workspace/b.txt"}).
				want(t, http.StatusNotFound, "move as other tenant")

			readFile(t, sb.ID, "/workspace/a.txt").want(t, http.StatusOK, "the file is still in place")
		},
	})
}

// API keys are part of the control plane, not of a driver: every deployment that
// manages its own keys must pass these. They are recorded under "api_keys".
func TestAPIKeys(t *testing.T) {
	type createdKey struct {
		ID        string   `json:"id"`
		Key       string   `json:"key"`
		Scopes    []string `json:"scopes"`
		RevokedAt string   `json:"revoked_at"`
	}
	create := func(t *testing.T, scopes ...string) createdKey {
		t.Helper()
		body := map[string]any{"name": randomName(t, "conformance-")}
		if len(scopes) > 0 {
			body["scopes"] = scopes
		}
		var key createdKey
		call(t, apiKey, http.MethodPost, "/v1/api-keys", body).want(t, http.StatusCreated, "create key").json(t, &key)
		t.Cleanup(func() { call(t, apiKey, http.MethodDelete, "/v1/api-keys/"+key.ID, nil) })
		return key
	}
	supported["api_keys"] = true
	capability(t, "api_keys", nil, map[string]func(t *testing.T){
		"created_key_authenticates": func(t *testing.T) {
			key := create(t)

			call(t, key.Key, http.MethodGet, "/v1/sandboxes?limit=1", nil).want(t, http.StatusOK, "list with the new key")
		},
		"revoked_key_is_refused_on_the_next_request": func(t *testing.T) {
			key := create(t)
			call(t, key.Key, http.MethodGet, "/v1/sandboxes?limit=1", nil).want(t, http.StatusOK, "before the revoke")

			call(t, apiKey, http.MethodDelete, "/v1/api-keys/"+key.ID, nil).want(t, http.StatusNoContent, "revoke")

			call(t, key.Key, http.MethodGet, "/v1/sandboxes?limit=1", nil).want(t, http.StatusUnauthorized, "after the revoke")
		},
		"list_never_returns_a_secret": func(t *testing.T) {
			key := create(t)

			resp := call(t, apiKey, http.MethodGet, "/v1/api-keys", nil).want(t, http.StatusOK, "list keys")

			if strings.Contains(string(resp.body), key.Key) {
				t.Error("the list of keys contains the secret of a key")
			}
			if !strings.Contains(string(resp.body), key.ID) {
				t.Errorf("the list of keys does not contain the new key %s", key.ID)
			}
		},
		"key_without_the_manage_scope_cannot_create_keys": func(t *testing.T) {
			key := create(t, "sandbox:execute")

			call(t, key.Key, http.MethodPost, "/v1/api-keys", map[string]any{"name": "x"}).
				want(t, http.StatusForbidden, "create a key without api_keys:manage")
		},
		"key_without_the_sandbox_scope_cannot_create_a_sandbox": func(t *testing.T) {
			key := create(t, "api_keys:manage")

			call(t, key.Key, http.MethodPost, "/v1/sandboxes", map[string]any{"image": shellImage}).
				want(t, http.StatusForbidden, "create a sandbox without sandbox:execute")
		},
		"another_tenant_cannot_revoke_the_key": func(t *testing.T) {
			key := create(t)

			call(t, otherAPIKey, http.MethodDelete, "/v1/api-keys/"+key.ID, nil).want(t, http.StatusNotFound, "revoke as other tenant")

			call(t, key.Key, http.MethodGet, "/v1/sandboxes?limit=1", nil).want(t, http.StatusOK, "key still works")
		},
	})
}

func startSession(t *testing.T, id string) *response {
	t.Helper()
	return call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+id+"/sessions", map[string]any{})
}

func runCell(t *testing.T, id, sessionID, code string) execResult {
	t.Helper()
	resp := call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+id+"/sessions/"+sessionID+"/execute", map[string]any{"code": code})
	resp.want(t, http.StatusOK, "run cell")
	var res execResult
	resp.json(t, &res)
	return res
}

func sessionID(t *testing.T, resp *response) string {
	t.Helper()
	resp.want(t, http.StatusCreated, "start session")
	var body struct {
		SessionID string `json:"session_id"`
	}
	resp.json(t, &body)
	if body.SessionID == "" {
		t.Fatalf("start session: empty session_id: %s", resp.body)
	}
	return body.SessionID
}

// A driver without the "network" capability must deny network access to every
// sandbox. The cases read kernel state instead of relying on a tool the image
// may lack. A driver with the capability is covered by TestNetwork.
func TestNetworkDeniedByDefault(t *testing.T) {
	if supported["network"] {
		t.Skip("the driver gives sandboxes a network; TestNetwork covers what a sandbox without rules can reach")
	}
	supported["network_default_deny"] = true
	capability(t, "network_default_deny", nil, map[string]func(t *testing.T){
		"the_sandbox_has_no_interface_but_loopback": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			res := sh(t, sb.ID, "ls /sys/class/net")

			if got := strings.Fields(res.Stdout); res.ExitCode != 0 || len(got) != 1 || got[0] != "lo" {
				t.Errorf("interfaces = %q (exit %d), want only lo", res.Stdout, res.ExitCode)
			}
		},
		"the_sandbox_has_no_route_to_any_host": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			// The first line of /proc/net/route is the header.
			res := sh(t, sb.ID, "tail -n +2 /proc/net/route | wc -l")

			if strings.TrimSpace(res.Stdout) != "0" {
				t.Errorf("routes = %q, want none", res.Stdout)
			}
		},
		"a_connection_to_a_public_address_fails": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			// Each tool that exists must fail; an image with neither proves nothing here
			// and the two cases above carry the assertion.
			res := sh(t, sb.ID, "for tool in 'wget -q -T 3 -O /dev/null http://1.1.1.1' 'nc -w 3 1.1.1.1 80'; do "+
				"command -v ${tool%% *} >/dev/null 2>&1 || continue; $tool </dev/null >/dev/null 2>&1 && echo reached; done; echo checked")

			if strings.Contains(res.Stdout, "reached") || !strings.Contains(res.Stdout, "checked") {
				t.Errorf("output = %q, want no connection to succeed", res.Stdout)
			}
		},
	})
}

// The "network" capability: a sandbox has a network interface, reaches the public
// internet unless its rules say otherwise, and never reaches a private address.
// The cases reach example.com and example.org over HTTP with the wget of the
// shell image, so the suite needs outbound internet where the driver runs.
func TestNetwork(t *testing.T) {
	type denial struct {
		Destination string `json:"destination"`
		Host        string `json:"host"`
		Reason      string `json:"reason"`
		Time        string `json:"time"`
	}
	denials := func(t *testing.T, key, id string) []denial {
		t.Helper()
		var body struct {
			Denials []denial `json:"denials"`
		}
		call(t, key, http.MethodGet, "/v1/sandboxes/"+id+"/network/denials", nil).want(t, http.StatusOK, "list denials").json(t, &body)
		return body.Denials
	}
	fetch := func(t *testing.T, id, url string) bool {
		t.Helper()
		return sh(t, id, "wget -q -T 15 -O /dev/null "+url).ExitCode == 0
	}
	restricted := map[string]any{"network": map[string]any{"allow_out": []string{"example.com"}, "deny_out": []string{"0.0.0.0/0"}}}

	capability(t, "network", func(t *testing.T) *response {
		sb := newSandbox(t, shellImage)
		return call(t, apiKey, http.MethodPut, "/v1/sandboxes/"+sb.ID+"/network", map[string]any{})
	}, map[string]func(t *testing.T){
		"a_sandbox_without_rules_reaches_the_public_internet": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			if !fetch(t, sb.ID, "http://example.com/") {
				t.Error("a sandbox with the default policy could not fetch http://example.com/")
			}
		},
		"a_sandbox_never_reaches_a_private_or_metadata_address": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			for _, url := range []string{"http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/", "http://192.168.0.1/"} {
				if fetch(t, sb.ID, url) {
					t.Errorf("%s answered", url)
				}
			}
			got := denials(t, apiKey, sb.ID)
			seen := map[string]bool{}
			for _, d := range got {
				seen[d.Destination] = true
			}
			for _, want := range []string{"169.254.169.254:80", "10.0.0.1:80", "192.168.0.1:80"} {
				if !seen[want] {
					t.Errorf("no denial recorded for %s: %+v", want, got)
				}
			}
		},
		"rules_given_at_creation_limit_the_sandbox_to_the_allowed_host": func(t *testing.T) {
			sb := newSandboxWith(t, restricted)

			if !fetch(t, sb.ID, "http://example.com/") {
				t.Error("the host in allow_out could not be fetched")
			}
			if fetch(t, sb.ID, "http://example.org/") {
				t.Error("a host outside allow_out was fetched")
			}
			got := denials(t, apiKey, sb.ID)
			if len(got) == 0 || got[len(got)-1].Host != "example.org" || got[len(got)-1].Reason == "" || got[len(got)-1].Time == "" {
				t.Errorf("denials = %+v, want the last one to name example.org with a reason and a time", got)
			}
		},
		"the_sandbox_reports_its_rules": func(t *testing.T) {
			sb := newSandboxWith(t, restricted)

			var got struct {
				Network struct {
					AllowOut []string `json:"allow_out"`
					DenyOut  []string `json:"deny_out"`
				} `json:"network"`
			}
			call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID, nil).want(t, http.StatusOK, "get sandbox").json(t, &got)
			if len(got.Network.AllowOut) != 1 || got.Network.AllowOut[0] != "example.com" || len(got.Network.DenyOut) != 1 {
				t.Errorf("network = %+v", got.Network)
			}
		},
		"new_rules_apply_to_a_running_sandbox": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			if !fetch(t, sb.ID, "http://example.org/") {
				t.Fatal("before the update, http://example.org/ could not be fetched")
			}

			call(t, apiKey, http.MethodPut, "/v1/sandboxes/"+sb.ID+"/network", restricted["network"]).want(t, http.StatusOK, "update network")

			// The stored rules follow the update, so a later read agrees with what is enforced.
			var stored struct {
				Network struct {
					AllowOut []string `json:"allow_out"`
				} `json:"network"`
			}
			call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID, nil).want(t, http.StatusOK, "get sandbox").json(t, &stored)
			if len(stored.Network.AllowOut) != 1 || stored.Network.AllowOut[0] != "example.com" {
				t.Errorf("the sandbox reports %+v after the update", stored.Network)
			}
			if fetch(t, sb.ID, "http://example.org/") {
				t.Error("after the update, a host outside allow_out was fetched")
			}
			if !fetch(t, sb.ID, "http://example.com/") {
				t.Error("after the update, the host in allow_out could not be fetched")
			}

			call(t, apiKey, http.MethodPut, "/v1/sandboxes/"+sb.ID+"/network", map[string]any{}).want(t, http.StatusOK, "clear network")

			if !fetch(t, sb.ID, "http://example.org/") {
				t.Error("after the rules were removed, http://example.org/ could not be fetched")
			}
		},
		"invalid_rules_are_refused_and_change_nothing": func(t *testing.T) {
			sb := newSandboxWith(t, restricted)

			call(t, apiKey, http.MethodPut, "/v1/sandboxes/"+sb.ID+"/network", map[string]any{"deny_out": []string{"example.com"}}).
				want(t, http.StatusBadRequest, "update with a host name in deny_out")
			call(t, apiKey, http.MethodPost, "/v1/sandboxes", map[string]any{"image": shellImage, "network": map[string]any{"allow_out": []string{"10.0.0.0/99"}}}).
				want(t, http.StatusBadRequest, "create with a malformed range")

			if fetch(t, sb.ID, "http://example.org/") {
				t.Error("the rules in force were lost after a refused update")
			}
		},
		"another_tenant_cannot_read_or_change_the_rules": func(t *testing.T) {
			sb := newSandboxWith(t, restricted)

			call(t, otherAPIKey, http.MethodPut, "/v1/sandboxes/"+sb.ID+"/network", map[string]any{}).want(t, http.StatusNotFound, "update by another tenant")
			call(t, otherAPIKey, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/network/denials", nil).want(t, http.StatusNotFound, "denials by another tenant")

			if fetch(t, sb.ID, "http://example.org/") {
				t.Error("another tenant opened the sandbox's network")
			}
		},
	})
}

// Signed file URLs belong to the deployment, not to the driver: they are on when
// the API has a signing key. The suite asks once and reports them as
// unsupported when the route answers 501.
func TestSignedURLs(t *testing.T) {
	type fileURL struct {
		URL    string `json:"url"`
		Method string `json:"method"`
	}
	issueRaw := func(t *testing.T, key, id string, body map[string]any) *response {
		t.Helper()
		return call(t, key, http.MethodPost, "/v1/sandboxes/"+id+"/file-urls", body)
	}
	issue := func(t *testing.T, id, path, operation string, seconds int) fileURL {
		t.Helper()
		var u fileURL
		issueRaw(t, apiKey, id, map[string]any{"path": path, "operation": operation, "expires_in_seconds": seconds}).
			want(t, http.StatusCreated, "create file url").json(t, &u)
		return u
	}
	// use sends a request to a signed URL with no API key.
	use := func(t *testing.T, method, url, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request to the signed url: %v", err)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("request to the signed url: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	probe := func(t *testing.T) *response {
		return issueRaw(t, apiKey, newSandbox(t, shellImage).ID, map[string]any{"path": "/workspace/probe", "operation": "upload"})
	}
	supported["signed_urls"] = probe(t).status != http.StatusNotImplemented
	capability(t, "signed_urls", probe, map[string]func(t *testing.T){
		"a_file_uploaded_by_signed_url_is_listed_and_visible_to_a_command": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			u := issue(t, sb.ID, "/workspace/in/data.csv", "upload", 60)

			status, body := use(t, u.Method, u.URL, "a,b\n1,2\n")

			if status != http.StatusNoContent {
				t.Fatalf("upload = %d %s, want 204", status, body)
			}
			listing := call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files?path=/workspace/in", nil).want(t, http.StatusOK, "list")
			if !strings.Contains(string(listing.body), "data.csv") {
				t.Errorf("listing = %s, want data.csv in it", listing.body)
			}
			if res := sh(t, sb.ID, "cat /workspace/in/data.csv"); res.Stdout != "a,b\n1,2\n" {
				t.Errorf("cat = %q, want the uploaded bytes", res.Stdout)
			}
		},
		"a_signed_url_downloads_a_file_written_by_a_command": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			sh(t, sb.ID, "printf result > /workspace/out.txt")
			u := issue(t, sb.ID, "/workspace/out.txt", "download", 60)

			status, body := use(t, u.Method, u.URL, "")

			if status != http.StatusOK || body != "result" {
				t.Errorf("download = %d %q, want 200 result", status, body)
			}
		},
		"an_expired_url_answers_403": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/a.txt", "x")
			u := issue(t, sb.ID, "/workspace/a.txt", "download", 1)
			time.Sleep(2500 * time.Millisecond)

			status, _ := use(t, u.Method, u.URL, "")

			if status != http.StatusForbidden {
				t.Errorf("download with an expired url = %d, want 403", status)
			}
		},
		"an_altered_url_answers_403_and_changes_nothing": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/a.txt", "keep")
			u := issue(t, sb.ID, "/workspace/a.txt", "upload", 60)
			last := u.URL[len(u.URL)-1:]
			swapped := "A"
			if last == "A" {
				swapped = "B"
			}

			status, _ := use(t, http.MethodPut, u.URL[:len(u.URL)-1]+swapped, "overwritten")

			if status != http.StatusForbidden {
				t.Errorf("upload with an altered url = %d, want 403", status)
			}
			if resp := readFile(t, sb.ID, "/workspace/a.txt"); string(resp.body) != "keep" {
				t.Errorf("the file became %q", resp.body)
			}
		},
		"an_upload_url_cannot_download": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/a.txt", "secret")
			u := issue(t, sb.ID, "/workspace/a.txt", "upload", 60)

			status, body := use(t, http.MethodGet, u.URL, "")

			if status != http.StatusForbidden || strings.Contains(body, "secret") {
				t.Errorf("GET on an upload url = %d %q, want 403 without the content", status, body)
			}
		},
		"another_tenant_cannot_get_a_url_for_the_sandbox": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			issueRaw(t, otherAPIKey, sb.ID, map[string]any{"path": "/workspace/a.txt", "operation": "download"}).
				want(t, http.StatusNotFound, "create file url as other tenant")
		},
		"a_url_for_a_path_outside_the_workspace_is_refused": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			issueRaw(t, apiKey, sb.ID, map[string]any{"path": "/etc/passwd", "operation": "download"}).
				want(t, http.StatusBadRequest, "create file url outside the workspace")
		},
	})
}

func TestLogs(t *testing.T) {
	type entry struct {
		PID    int    `json:"pid"`
		Stream string `json:"stream"`
		Data   string `json:"data"`
		At     string `json:"at"`
	}
	type logs struct {
		Entries   []entry `json:"entries"`
		Next      int64   `json:"next"`
		Truncated bool    `json:"truncated"`
	}
	read := func(t *testing.T, id string, after int64, waitMS int) logs {
		t.Helper()
		var out logs
		call(t, apiKey, http.MethodGet, fmt.Sprintf("/v1/sandboxes/%s/logs?after=%d&wait_ms=%d", id, after, waitMS), nil).
			want(t, http.StatusOK, "read logs").json(t, &out)
		return out
	}
	start := func(t *testing.T, id, script string) int {
		t.Helper()
		var p struct {
			PID int `json:"pid"`
		}
		call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+id+"/processes", map[string]any{"cmd": []string{"sh", "-c", script}}).
			want(t, http.StatusCreated, "start process").json(t, &p)
		return p.PID
	}
	// until reads from cursor after until the log holds want as the data of one stream of pid.
	until := func(t *testing.T, id string, after int64, pid int, stream, want string) logs {
		t.Helper()
		var got strings.Builder
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
			out := read(t, id, after, 2000)
			after = out.Next
			for _, e := range out.Entries {
				if e.PID == pid && e.Stream == stream {
					got.WriteString(e.Data)
				}
			}
			if got.String() == want {
				return out
			}
		}
		t.Fatalf("the %s of process %d in the log = %q, want %q", stream, pid, got.String(), want)
		return logs{}
	}
	probe := func(t *testing.T) *response {
		return call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+newSandbox(t, shellImage).ID+"/logs", nil)
	}
	capability(t, "logs", probe, map[string]func(t *testing.T){
		"the_log_has_the_output_of_each_process_with_its_pid_and_stream": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			first := start(t, sb.ID, "echo from-first")
			second := start(t, sb.ID, "echo from-second >&2")

			until(t, sb.ID, 0, first, "stdout", "from-first\n")
			until(t, sb.ID, 0, second, "stderr", "from-second\n")
		},
		"a_reader_resumes_from_its_cursor": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			first := start(t, sb.ID, "echo one")
			seen := until(t, sb.ID, 0, first, "stdout", "one\n")

			second := start(t, sb.ID, "echo two")
			later := until(t, sb.ID, seen.Next, second, "stdout", "two\n")

			for _, e := range later.Entries {
				if e.PID == first {
					t.Errorf("an entry before the cursor came back: %+v", e)
				}
			}
		},
		"a_new_sandbox_has_an_empty_log": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			out := read(t, sb.ID, 0, 0)

			if len(out.Entries) != 0 || out.Next != 0 || out.Truncated {
				t.Errorf("log of a new sandbox = %+v, want empty", out)
			}
		},
		"another_tenant_cannot_read_the_log": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			call(t, otherAPIKey, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/logs", nil).want(t, http.StatusNotFound, "logs as other tenant")
		},
	})
}

func TestMetrics(t *testing.T) {
	type metrics struct {
		CPU   uint64 `json:"cpu_usage_ns"`
		Used  uint64 `json:"memory_used_bytes"`
		Limit uint64 `json:"memory_limit_bytes"`
		At    string `json:"at"`
	}
	read := func(t *testing.T, id string) metrics {
		t.Helper()
		var m metrics
		call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+id+"/metrics", nil).want(t, http.StatusOK, "read metrics").json(t, &m)
		return m
	}
	probe := func(t *testing.T) *response {
		return call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+newSandbox(t, shellImage).ID+"/metrics", nil)
	}
	capability(t, "metrics", probe, map[string]func(t *testing.T){
		"memory_in_use_is_positive_and_within_the_limit": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			m := read(t, sb.ID)

			if m.Used == 0 || m.Limit == 0 || m.Used > m.Limit {
				t.Errorf("metrics = %+v, want used memory above 0 and within the limit", m)
			}
			if _, err := time.Parse(time.RFC3339Nano, m.At); err != nil {
				t.Errorf("at = %q is not a timestamp: %v", m.At, err)
			}
		},
		"cpu_time_grows_when_the_sandbox_burns_cpu": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			before := read(t, sb.ID)

			sh(t, sb.ID, "i=0; while [ $i -lt 300000 ]; do i=$((i+1)); done")
			after := read(t, sb.ID)

			if grew := int64(after.CPU) - int64(before.CPU); grew < int64(50*time.Millisecond) {
				t.Errorf("cpu_usage_ns went from %d to %d (%dns); want at least 50ms more after a busy loop", before.CPU, after.CPU, grew)
			}
		},
		"another_tenant_cannot_read_the_metrics": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			call(t, otherAPIKey, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/metrics", nil).want(t, http.StatusNotFound, "metrics as other tenant")
		},
	})
}

func TestFileWatch(t *testing.T) {
	type event struct {
		Type  string `json:"type"`
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
	}
	type events struct {
		Events    []event `json:"events"`
		Next      int64   `json:"next"`
		Truncated bool    `json:"truncated"`
		Closed    bool    `json:"closed"`
	}
	base := func(id string) string { return "/v1/sandboxes/" + id + "/watches" }
	startRaw := func(t *testing.T, id, path string, recursive bool) *response {
		t.Helper()
		return call(t, apiKey, http.MethodPost, base(id), map[string]any{"path": path, "recursive": recursive})
	}
	start := func(t *testing.T, id, path string, recursive bool) string {
		t.Helper()
		var body struct {
			WatchID string `json:"watch_id"`
		}
		startRaw(t, id, path, recursive).want(t, http.StatusCreated, "start watch").json(t, &body)
		if body.WatchID == "" {
			t.Fatal("the watch has no id")
		}
		return body.WatchID
	}
	read := func(t *testing.T, id, wid string, after int64, waitMS int) events {
		t.Helper()
		var out events
		call(t, apiKey, http.MethodGet, fmt.Sprintf("%s/%s/events?after=%d&wait_ms=%d", base(id), wid, after, waitMS), nil).
			want(t, http.StatusOK, "read events").json(t, &out)
		return out
	}
	// until reads from the start until an event of that type and path arrives.
	until := func(t *testing.T, id, wid, kind, path string) ([]event, events) {
		t.Helper()
		var all []event
		var after int64
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
			out := read(t, id, wid, after, 2000)
			all, after = append(all, out.Events...), out.Next
			for _, ev := range all {
				if ev.Type == kind && ev.Path == path {
					return all, out
				}
			}
		}
		t.Fatalf("no %s event for %s; got %+v", kind, path, all)
		return nil, events{}
	}
	probe := func(t *testing.T) *response {
		return startRaw(t, newSandbox(t, shellImage).ID, "/workspace", false)
	}
	capability(t, "file_watch", probe, map[string]func(t *testing.T){
		"an_event_arrives_when_a_file_is_created": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			wid := start(t, sb.ID, "/workspace", false)

			writeFile(t, sb.ID, "/workspace/a.txt", "x")

			until(t, sb.ID, wid, "create", "/workspace/a.txt")
		},
		"a_change_made_by_a_command_is_reported_too": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/a.txt", "x")
			wid := start(t, sb.ID, "/workspace", false)

			sh(t, sb.ID, "rm /workspace/a.txt")

			until(t, sb.ID, wid, "remove", "/workspace/a.txt")
		},
		"a_recursive_watch_reports_a_file_in_a_new_subdirectory": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			wid := start(t, sb.ID, "/workspace", true)

			sh(t, sb.ID, "mkdir -p /workspace/new/deep && echo x > /workspace/new/deep/b.txt")

			until(t, sb.ID, wid, "create", "/workspace/new/deep/b.txt")
		},
		"a_reader_that_reconnects_resumes_from_its_cursor": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			wid := start(t, sb.ID, "/workspace", false)
			sh(t, sb.ID, "touch /workspace/a.txt")
			_, seen := until(t, sb.ID, wid, "create", "/workspace/a.txt")

			sh(t, sb.ID, "touch /workspace/b.txt")
			var later events
			for deadline := time.Now().Add(30 * time.Second); len(later.Events) == 0 && time.Now().Before(deadline); {
				later = read(t, sb.ID, wid, seen.Next, 2000)
			}

			if len(later.Events) == 0 || later.Events[0].Path != "/workspace/b.txt" {
				t.Errorf("events after cursor %d = %+v, want them to start at b.txt", seen.Next, later.Events)
			}
		},
		"watching_a_missing_directory_answers_404": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			startRaw(t, sb.ID, "/workspace/nope", false).want(t, http.StatusNotFound, "watch a missing directory")
		},
		"watching_a_file_answers_400": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/a.txt", "x")

			startRaw(t, sb.ID, "/workspace/a.txt", false).want(t, http.StatusBadRequest, "watch a file")
		},
		"watching_outside_the_workspace_answers_400": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			sh(t, sb.ID, "ln -s /tmp /workspace/escape")

			startRaw(t, sb.ID, "/etc", false).want(t, http.StatusBadRequest, "watch /etc")
			startRaw(t, sb.ID, "/workspace/escape", false).want(t, http.StatusBadRequest, "watch through a symlink")
		},
		"a_stopped_watch_answers_404": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			wid := start(t, sb.ID, "/workspace", false)

			call(t, apiKey, http.MethodDelete, base(sb.ID)+"/"+wid, nil).want(t, http.StatusNoContent, "stop watch")

			call(t, apiKey, http.MethodGet, base(sb.ID)+"/"+wid+"/events", nil).want(t, http.StatusNotFound, "events of a stopped watch")
		},
		"another_tenant_cannot_read_or_stop_a_watch": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			wid := start(t, sb.ID, "/workspace", false)

			call(t, otherAPIKey, http.MethodGet, base(sb.ID)+"/"+wid+"/events", nil).want(t, http.StatusNotFound, "events as other tenant")
			call(t, otherAPIKey, http.MethodDelete, base(sb.ID)+"/"+wid, nil).want(t, http.StatusNotFound, "stop as other tenant")

			read(t, sb.ID, wid, 0, 0)
		},
	})
}

func TestProcesses(t *testing.T) {
	type process struct {
		PID      int      `json:"pid"`
		Cmd      []string `json:"cmd"`
		Running  bool     `json:"running"`
		ExitCode *int     `json:"exit_code"`
	}
	type output struct {
		Stdout    string  `json:"stdout"`
		Stderr    string  `json:"stderr"`
		Next      int64   `json:"next"`
		Truncated bool    `json:"truncated"`
		Process   process `json:"process"`
	}
	base := func(id string) string { return "/v1/sandboxes/" + id + "/processes" }
	startRaw := func(t *testing.T, id string, body map[string]any) *response {
		t.Helper()
		return call(t, apiKey, http.MethodPost, base(id), body)
	}
	start := func(t *testing.T, id, script string) process {
		t.Helper()
		var p process
		startRaw(t, id, map[string]any{"cmd": []string{"sh", "-c", script}}).want(t, http.StatusCreated, "start process").json(t, &p)
		if p.PID <= 0 || !p.Running {
			t.Fatalf("started process = %+v, want a pid and running", p)
		}
		return p
	}
	read := func(t *testing.T, id string, pid int, after int64, waitMS int) output {
		t.Helper()
		var out output
		call(t, apiKey, http.MethodGet, fmt.Sprintf("%s/%d/output?after=%d&wait_ms=%d", base(id), pid, after, waitMS), nil).
			want(t, http.StatusOK, "read output").json(t, &out)
		return out
	}
	// drain reads from the start until the process ends.
	drain := func(t *testing.T, id string, pid int) (stdout, stderr string, last output) {
		t.Helper()
		var after int64
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
			out := read(t, id, pid, after, 2000)
			stdout, stderr, after = stdout+out.Stdout, stderr+out.Stderr, out.Next
			if !out.Process.Running {
				return stdout, stderr, out
			}
		}
		t.Fatalf("process %d did not end; stdout so far: %q", pid, stdout)
		return "", "", output{}
	}
	signal := func(t *testing.T, id string, pid int, name string) *response {
		t.Helper()
		return call(t, apiKey, http.MethodPost, fmt.Sprintf("%s/%d/signal", base(id), pid), map[string]any{"signal": name})
	}
	probe := func(t *testing.T) *response {
		return call(t, apiKey, http.MethodGet, base(newSandbox(t, shellImage).ID), nil)
	}
	capability(t, "processes", probe, map[string]func(t *testing.T){
		"start_returns_while_the_command_still_runs": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			begin := time.Now()

			p := start(t, sb.ID, "sleep 60")

			if took := time.Since(begin); took > 20*time.Second {
				t.Errorf("start took %s, want it to return without waiting for the command", took)
			}
			if res := sh(t, sb.ID, fmt.Sprintf("kill -0 %d", p.PID)); res.ExitCode != 0 {
				t.Errorf("pid %d is not a live process in the sandbox", p.PID)
			}
		},
		"output_arrives_before_the_process_ends": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			p := start(t, sb.ID, "echo first; sleep 60")

			out := read(t, sb.ID, p.PID, 0, 10000)

			if out.Stdout != "first\n" || !out.Process.Running || out.Process.ExitCode != nil {
				t.Errorf("output = %+v, want first from a running process", out)
			}
		},
		"output_has_both_streams_and_the_exit_code": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			p := start(t, sb.ID, "echo out; echo err >&2; exit 7")

			stdout, stderr, last := drain(t, sb.ID, p.PID)

			if stdout != "out\n" || stderr != "err\n" || last.Process.ExitCode == nil || *last.Process.ExitCode != 7 {
				t.Errorf("stdout=%q stderr=%q process=%+v, want out, err and exit 7", stdout, stderr, last.Process)
			}
		},
		"a_reader_that_reconnects_resumes_from_its_cursor": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			p := start(t, sb.ID, "echo one; read line; echo two")
			first := read(t, sb.ID, p.PID, 0, 10000)
			call(t, apiKey, http.MethodPost, fmt.Sprintf("%s/%d/stdin", base(sb.ID), p.PID), []byte("go\n")).want(t, http.StatusNoContent, "write stdin")
			drain(t, sb.ID, p.PID)

			resumed := read(t, sb.ID, p.PID, first.Next, 0)
			fromStart := read(t, sb.ID, p.PID, 0, 0)

			if first.Stdout != "one\n" || resumed.Stdout != "two\n" || fromStart.Stdout != "one\ntwo\n" {
				t.Errorf("first=%q resumed=%q from start=%q, want one, two and both", first.Stdout, resumed.Stdout, fromStart.Stdout)
			}
		},
		"stdin_reaches_the_process_and_closing_it_ends_the_input": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			p := start(t, sb.ID, "cat; echo done")
			stdin := fmt.Sprintf("%s/%d/stdin", base(sb.ID), p.PID)
			call(t, apiKey, http.MethodPost, stdin, []byte("hello ")).want(t, http.StatusNoContent, "write stdin")
			call(t, apiKey, http.MethodPost, stdin, []byte("world\n")).want(t, http.StatusNoContent, "write stdin again")

			call(t, apiKey, http.MethodDelete, stdin, nil).want(t, http.StatusNoContent, "close stdin")
			stdout, _, last := drain(t, sb.ID, p.PID)

			if stdout != "hello world\ndone\n" || last.Process.ExitCode == nil || *last.Process.ExitCode != 0 {
				t.Errorf("stdout=%q process=%+v, want the input echoed, done and exit 0", stdout, last.Process)
			}
			call(t, apiKey, http.MethodPost, stdin, []byte("late")).want(t, http.StatusConflict, "write stdin of an ended process")
		},
		"signal_ends_the_process_and_its_children": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			p := start(t, sb.ID, "sleep 4141 & echo child=$!; wait")
			child := strings.TrimSpace(strings.TrimPrefix(read(t, sb.ID, p.PID, 0, 10000).Stdout, "child="))

			signal(t, sb.ID, p.PID, "TERM").want(t, http.StatusNoContent, "signal")
			_, _, last := drain(t, sb.ID, p.PID)

			if last.Process.ExitCode == nil || *last.Process.ExitCode != 143 {
				t.Errorf("process after TERM = %+v, want exit code 143", last.Process)
			}
			if res := sh(t, sb.ID, "kill -0 "+child); res.ExitCode == 0 {
				t.Errorf("the child %s survived the signal", child)
			}
			signal(t, sb.ID, p.PID, "TERM").want(t, http.StatusConflict, "signal an ended process")
		},
		"list_shows_running_and_ended_processes": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			ended := start(t, sb.ID, "exit 3")
			drain(t, sb.ID, ended.PID)
			running := start(t, sb.ID, "sleep 60")

			var list struct {
				Processes []process `json:"processes"`
			}
			call(t, apiKey, http.MethodGet, base(sb.ID), nil).want(t, http.StatusOK, "list").json(t, &list)

			byPID := map[int]process{}
			for _, p := range list.Processes {
				byPID[p.PID] = p
			}
			if e := byPID[ended.PID]; len(byPID) != 2 || e.Running || e.ExitCode == nil || *e.ExitCode != 3 || !byPID[running.PID].Running {
				t.Errorf("processes = %+v, want %d ended with 3 and %d running", list.Processes, ended.PID, running.PID)
			}
		},
		"the_process_sees_the_sandbox_env_and_its_workdir": func(t *testing.T) {
			sb := newSandboxWith(t, map[string]any{"env": map[string]string{"FROM_SANDBOX": "yes"}})
			sh(t, sb.ID, "mkdir -p /workspace/app")
			var p process
			startRaw(t, sb.ID, map[string]any{"cmd": []string{"sh", "-c", "echo $FROM_SANDBOX $OWN; pwd"}, "env": map[string]string{"OWN": "mine"}, "workdir": "/workspace/app"}).
				want(t, http.StatusCreated, "start").json(t, &p)

			stdout, _, _ := drain(t, sb.ID, p.PID)

			if stdout != "yes mine\n/workspace/app\n" {
				t.Errorf("stdout = %q, want the two variables and the workdir", stdout)
			}
		},
		"a_command_that_does_not_exist_answers_400": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			startRaw(t, sb.ID, map[string]any{"cmd": []string{"/no/such/binary"}}).want(t, http.StatusBadRequest, "start a missing command")
		},
		"an_unknown_process_answers_404": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			call(t, apiKey, http.MethodGet, base(sb.ID)+"/999999/output", nil).want(t, http.StatusNotFound, "output of an unknown process")
			signal(t, sb.ID, 999999, "TERM").want(t, http.StatusNotFound, "signal an unknown process")
		},
		"another_tenant_cannot_see_or_signal_a_process": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			p := start(t, sb.ID, "sleep 60")

			call(t, otherAPIKey, http.MethodGet, base(sb.ID), nil).want(t, http.StatusNotFound, "list as other tenant")
			call(t, otherAPIKey, http.MethodPost, fmt.Sprintf("%s/%d/signal", base(sb.ID), p.PID), map[string]any{"signal": "KILL"}).
				want(t, http.StatusNotFound, "signal as other tenant")

			if out := read(t, sb.ID, p.PID, 0, 0); !out.Process.Running {
				t.Error("the process was ended by another tenant")
			}
		},
		"processes_end_when_the_sandbox_is_destroyed": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			start(t, sb.ID, "sleep 600")

			call(t, apiKey, http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil).want(t, http.StatusNoContent, "destroy")

			call(t, apiKey, http.MethodGet, base(sb.ID), nil).want(t, http.StatusNotFound, "list after destroy")
		},
	})
}

func TestSessions(t *testing.T) {
	probe := func(t *testing.T) *response {
		return startSession(t, newSandbox(t, shellImage).ID)
	}
	capability(t, "sessions", probe, map[string]func(t *testing.T){
		"state_from_one_cell_is_visible_in_the_next": func(t *testing.T) {
			sb := newSandbox(t, pythonImage)
			sid := sessionID(t, startSession(t, sb.ID))
			runCell(t, sb.ID, sid, "x = 41")

			res := runCell(t, sb.ID, sid, "print(x + 1)")

			if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "42" {
				t.Errorf("exit=%d stdout=%q stderr=%q, want 0 and 42", res.ExitCode, res.Stdout, res.Stderr)
			}
		},
		"a_cell_that_raises_is_a_result_not_an_error": func(t *testing.T) {
			sb := newSandbox(t, pythonImage)
			sid := sessionID(t, startSession(t, sb.ID))

			res := runCell(t, sb.ID, sid, "raise ValueError('boom')")

			if res.ExitCode == 0 || !strings.Contains(res.Stderr, "ValueError") {
				t.Errorf("exit=%d stderr=%q, want a nonzero exit and a ValueError traceback", res.ExitCode, res.Stderr)
			}
		},
		"sessions_do_not_share_state": func(t *testing.T) {
			sb := newSandbox(t, pythonImage)
			a := sessionID(t, startSession(t, sb.ID))
			b := sessionID(t, startSession(t, sb.ID))
			runCell(t, sb.ID, a, "only_in_a = 1")

			res := runCell(t, sb.ID, b, "print(only_in_a)")

			if res.ExitCode == 0 {
				t.Errorf("session b saw a variable defined in session a: stdout=%q", res.Stdout)
			}
		},
	})
}

func TestSessionStream(t *testing.T) {
	streamCell := func(t *testing.T, id, sid, code string) *http.Response {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"code": code})
		req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/sandboxes/"+id+"/sessions/"+sid+"/execute?stream=1", strings.NewReader(string(raw)))
		if err != nil {
			t.Fatalf("build streamed cell: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("streamed cell: %v", err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	probe := func(t *testing.T) *response {
		sb := newSandbox(t, pythonImage)
		start := startSession(t, sb.ID)
		if start.status != http.StatusCreated {
			return start // sessions themselves are unsupported: 501 from the start route
		}
		var body struct {
			SessionID string `json:"session_id"`
		}
		start.json(t, &body)
		return call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/sessions/"+body.SessionID+"/execute?stream=1",
			map[string]any{"code": "print(1)"})
	}
	capability(t, "session_stream", probe, map[string]func(t *testing.T){
		"delivers_cell_output_before_the_cell_ends": func(t *testing.T) {
			sb := newSandbox(t, pythonImage)
			sid := sessionID(t, startSession(t, sb.ID))

			resp := streamCell(t, sb.ID, sid, "import time\nprint('first', flush=True)\ntime.sleep(2)\nprint('second', flush=True)")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			events := readSSE(t, resp.Body)

			var first, done *sseEvent
			for i := range events {
				if first == nil && events[i].name == "stdout" && strings.Contains(events[i].data, "first") {
					first = &events[i]
				}
				if events[i].name == "done" {
					done = &events[i]
				}
			}
			if first == nil || done == nil {
				t.Fatalf("stream is missing the first stdout event or the done event: %+v", events)
			}
			if gap := done.at.Sub(first.at); gap < time.Second {
				t.Errorf("first output arrived %v before the end; want at least 1s, the output was buffered", gap)
			}
		},
	})
}

func TestPTY(t *testing.T) {
	probe := func(t *testing.T) *response {
		return call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+newSandbox(t, shellImage).ID+"/pty", nil)
	}
	capability(t, "pty", probe, map[string]func(t *testing.T){
		"resize_changes_the_terminal_size_the_shell_sees": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/v1/sandboxes/" + sb.ID + "/pty"
			conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
				HTTPHeader: http.Header{"Authorization": []string{"Bearer " + apiKey}},
			})
			if err != nil {
				t.Fatalf("open pty: %v", err)
			}
			defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","rows":40,"cols":100}`)); err != nil {
				t.Fatalf("send resize: %v", err)
			}
			if err := conn.Write(ctx, websocket.MessageBinary, []byte("stty size\n")); err != nil {
				t.Fatalf("send command: %v", err)
			}

			var seen strings.Builder
			for !strings.Contains(seen.String(), "40 100") {
				_, data, rerr := conn.Read(ctx)
				if rerr != nil {
					t.Fatalf("terminal never reported size 40 100; output so far: %q (%v)", seen.String(), rerr)
				}
				seen.Write(data)
			}
		},
	})
}

func TestFork(t *testing.T) {
	fork := func(t *testing.T, id string) *response {
		t.Helper()
		return call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+id+"/fork", nil)
	}
	forked := func(t *testing.T, parentID string) sandbox {
		t.Helper()
		resp := fork(t, parentID).want(t, http.StatusCreated, "fork")
		var child sandbox
		resp.json(t, &child)
		t.Cleanup(func() { destroy(t, child.ID) })
		return child
	}
	probe := func(t *testing.T) *response { return fork(t, newSandbox(t, shellImage).ID) }
	capability(t, "fork", probe, map[string]func(t *testing.T){
		"fork_starts_with_the_files_of_the_parent": func(t *testing.T) {
			parent := newSandbox(t, shellImage)
			writeFile(t, parent.ID, "/workspace/base.txt", "from-parent")

			child := forked(t, parent.ID)

			resp := readFile(t, child.ID, "/workspace/base.txt").want(t, http.StatusOK, "read in fork")
			if string(resp.body) != "from-parent" {
				t.Errorf("fork read %q, want %q", resp.body, "from-parent")
			}
		},
		"writes_in_the_fork_do_not_reach_the_parent": func(t *testing.T) {
			parent := newSandbox(t, shellImage)
			child := forked(t, parent.ID)

			writeFile(t, child.ID, "/workspace/only-in-fork.txt", "x")

			readFile(t, parent.ID, "/workspace/only-in-fork.txt").want(t, http.StatusNotFound, "read in parent")
		},
		"writes_in_the_parent_after_the_fork_do_not_reach_the_fork": func(t *testing.T) {
			parent := newSandbox(t, shellImage)
			child := forked(t, parent.ID)

			writeFile(t, parent.ID, "/workspace/only-in-parent.txt", "x")

			readFile(t, child.ID, "/workspace/only-in-parent.txt").want(t, http.StatusNotFound, "read in fork")
		},
		"fork_with_count_returns_that_many_independent_sandboxes": func(t *testing.T) {
			parent := newSandbox(t, shellImage)
			writeFile(t, parent.ID, "/workspace/base.txt", "from-parent")
			var batch struct {
				Sandboxes []sandbox `json:"sandboxes"`
			}

			call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+parent.ID+"/fork", map[string]any{"count": 3}).
				want(t, http.StatusCreated, "fork with count").json(t, &batch)

			for _, child := range batch.Sandboxes {
				t.Cleanup(func() { destroy(t, child.ID) })
			}
			if len(batch.Sandboxes) != 3 {
				t.Fatalf("got %d forks, want 3", len(batch.Sandboxes))
			}
			for i, child := range batch.Sandboxes {
				writeFile(t, child.ID, "/workspace/mine.txt", fmt.Sprintf("fork-%d", i))
			}
			for i, child := range batch.Sandboxes {
				base := readFile(t, child.ID, "/workspace/base.txt").want(t, http.StatusOK, "read base in fork")
				mine := readFile(t, child.ID, "/workspace/mine.txt").want(t, http.StatusOK, "read own file in fork")
				if string(base.body) != "from-parent" || string(mine.body) != fmt.Sprintf("fork-%d", i) {
					t.Errorf("fork %d has base %q and mine %q", i, base.body, mine.body)
				}
			}
			readFile(t, parent.ID, "/workspace/mine.txt").want(t, http.StatusNotFound, "read a fork's file in the parent")
		},
		"fork_has_its_own_id_and_is_running": func(t *testing.T) {
			parent := newSandbox(t, shellImage)

			child := forked(t, parent.ID)

			if child.ID == parent.ID || child.State != "running" {
				t.Errorf("fork id=%q state=%q, want a new id and running", child.ID, child.State)
			}
		},
	})
}

func TestPause(t *testing.T) {
	pause := func(t *testing.T, id string) *response {
		t.Helper()
		return call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+id+"/pause", nil)
	}
	resume := func(t *testing.T, id string) *response {
		t.Helper()
		return call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+id+"/resume", nil)
	}
	probe := func(t *testing.T) *response { return pause(t, newSandbox(t, shellImage).ID) }
	capability(t, "pause", probe, map[string]func(t *testing.T){
		"paused_sandbox_reports_state_paused": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			pause(t, sb.ID).want(t, http.StatusOK, "pause")

			var got sandbox
			call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID, nil).want(t, http.StatusOK, "get").json(t, &got)
			if got.State != "paused" {
				t.Errorf("state = %q, want paused", got.State)
			}
		},
		"files_survive_pause_and_resume": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			writeFile(t, sb.ID, "/workspace/kept.txt", "kept")
			pause(t, sb.ID).want(t, http.StatusOK, "pause")

			resume(t, sb.ID).want(t, http.StatusOK, "resume")

			resp := readFile(t, sb.ID, "/workspace/kept.txt").want(t, http.StatusOK, "read after resume")
			if string(resp.body) != "kept" {
				t.Errorf("read %q after resume, want %q", resp.body, "kept")
			}
		},
		"a_running_process_survives_pause_and_resume": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			sh(t, sb.ID, "nohup sleep 300 >/dev/null 2>&1 & echo $! > /workspace/pid")
			pause(t, sb.ID).want(t, http.StatusOK, "pause")

			resume(t, sb.ID).want(t, http.StatusOK, "resume")

			res := sh(t, sb.ID, "kill -0 $(cat /workspace/pid)")
			if res.ExitCode != 0 {
				t.Errorf("the process started before the pause is gone after the resume: %q", res.Stderr)
			}
		},
		"pausing_a_paused_sandbox_answers_409": func(t *testing.T) {
			sb := newSandbox(t, shellImage)
			pause(t, sb.ID).want(t, http.StatusOK, "pause")

			pause(t, sb.ID).want(t, http.StatusConflict, "second pause")
		},
	})
}

func TestVolumes(t *testing.T) {
	attach := func(t *testing.T, sandboxID, name string) *response {
		t.Helper()
		return call(t, apiKey, http.MethodPost, "/v1/volumes", map[string]any{"sandbox_id": sandboxID, "name": name, "size_gb": 1})
	}
	volumeID := func(t *testing.T, resp *response) string {
		t.Helper()
		resp.want(t, http.StatusCreated, "attach volume")
		var v struct {
			ID string `json:"id"`
		}
		resp.json(t, &v)
		return v.ID
	}
	probe := func(t *testing.T) *response {
		return attach(t, newSandbox(t, shellImage).ID, randomName(t, "probe"))
	}
	capability(t, "volumes", probe, map[string]func(t *testing.T){
		"data_survives_the_sandbox_and_reattaches_to_another": func(t *testing.T) {
			name := randomName(t, "vol")
			first := newSandbox(t, shellImage)
			id := volumeID(t, attach(t, first.ID, name))
			t.Cleanup(func() { call(t, apiKey, http.MethodDelete, "/v1/volumes/"+id, nil) })
			if res := sh(t, first.ID, "printf durable > /workspace/"+name+"/data.txt"); res.ExitCode != 0 {
				t.Fatalf("write to the volume: exit %d: %s", res.ExitCode, res.Stderr)
			}
			call(t, apiKey, http.MethodDelete, "/v1/sandboxes/"+first.ID, nil).want(t, http.StatusNoContent, "destroy first sandbox")

			second := newSandbox(t, shellImage)
			volumeID(t, attach(t, second.ID, name))

			res := execIn(t, second.ID, map[string]any{"cmd": []string{"cat", "/workspace/" + name + "/data.txt"}})
			if res.Stdout != "durable" {
				t.Errorf("the second sandbox read %q (exit %d, stderr %q), want %q", res.Stdout, res.ExitCode, res.Stderr, "durable")
			}
		},
		"another_tenant_cannot_attach_to_the_sandbox": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			resp := call(t, otherAPIKey, http.MethodPost, "/v1/volumes",
				map[string]any{"sandbox_id": sb.ID, "name": randomName(t, "vol"), "size_gb": 1})

			resp.want(t, http.StatusNotFound, "attach as other tenant")
		},
	})
}

func TestBrowser(t *testing.T) {
	navigate := func(t *testing.T, id, url string) *response {
		t.Helper()
		return call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+id+"/browser/navigate", map[string]any{"url": url})
	}
	const page = "data:text/html,<h1 id=%22marker%22>conformance-page</h1>"
	probe := func(t *testing.T) *response { return navigate(t, newSandbox(t, shellImage).ID, page) }
	capability(t, "browser", probe, map[string]func(t *testing.T){
		"dom_contains_the_page_that_was_opened": func(t *testing.T) {
			sb := newSandbox(t, browserImage)
			navigate(t, sb.ID, page).want(t, http.StatusNoContent, "navigate")

			resp := call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/browser/dom", nil).want(t, http.StatusOK, "dom")

			var body struct {
				DOM string `json:"dom"`
			}
			resp.json(t, &body)
			if !strings.Contains(body.DOM, "conformance-page") {
				t.Errorf("dom does not contain the page text: %q", body.DOM)
			}
		},
		"screenshot_is_a_png": func(t *testing.T) {
			sb := newSandbox(t, browserImage)
			navigate(t, sb.ID, page).want(t, http.StatusNoContent, "navigate")

			resp := call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/browser/screenshot", nil).want(t, http.StatusOK, "screenshot")

			if !strings.HasPrefix(string(resp.body), "\x89PNG") {
				t.Errorf("screenshot does not start with the PNG signature (%d bytes)", len(resp.body))
			}
		},
		"navigating_to_the_metadata_address_is_refused": func(t *testing.T) {
			sb := newSandbox(t, browserImage)

			navigate(t, sb.ID, "http://169.254.169.254/latest/meta-data/").want(t, http.StatusBadRequest, "navigate to IMDS")
		},
	})
}
