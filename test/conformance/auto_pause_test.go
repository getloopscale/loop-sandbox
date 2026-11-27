//go:build conformance

package conformance

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// idleLimitSeconds is the shortest idle limit the API accepts.
const idleLimitSeconds = 10

// stateOf reads the state of a sandbox. A GET of the record is not use, so it
// does not keep an idle sandbox awake.
func stateOf(t *testing.T, id string) string {
	t.Helper()
	var got sandbox
	call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+id, nil).want(t, http.StatusOK, "get sandbox").json(t, &got)
	return got.State
}

// waitForState polls until the sandbox reaches the state or the deadline passes.
func waitForState(t *testing.T, id, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := stateOf(t, id)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox %s is %s after %s, want %s", id, got, within, want)
		}
		time.Sleep(time.Second)
	}
}

func TestAutoPause(t *testing.T) {
	probe := func(t *testing.T) *response {
		return call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+newSandbox(t, shellImage).ID+"/pause", nil)
	}
	idleSandbox := func(t *testing.T) sandbox {
		t.Helper()
		return newSandboxWith(t, map[string]any{"auto_pause_seconds": idleLimitSeconds})
	}
	capability(t, "pause", probe, map[string]func(t *testing.T){
		"an_idle_sandbox_pauses_by_itself": func(t *testing.T) {
			sb := idleSandbox(t)

			waitForState(t, sb.ID, "paused", 3*idleLimitSeconds*time.Second)
		},
		"a_request_to_an_idle_paused_sandbox_resumes_it_and_is_served": func(t *testing.T) {
			sb := idleSandbox(t)
			writeFile(t, sb.ID, "/workspace/a.txt", "kept")
			pid := strings.TrimSpace(sh(t, sb.ID, "nohup sleep 600 >/dev/null 2>&1 & echo $!").Stdout)
			waitForState(t, sb.ID, "paused", 3*idleLimitSeconds*time.Second)

			out := sh(t, sb.ID, "cat /workspace/a.txt && kill -0 "+pid)

			if out.ExitCode != 0 || out.Stdout != "kept" || stateOf(t, sb.ID) != "running" {
				t.Errorf("exec gave exit %d stdout %q stderr %q and the sandbox is %s; want 0, kept and running",
					out.ExitCode, out.Stdout, out.Stderr, stateOf(t, sb.ID))
			}
		},
		"a_command_longer_than_the_idle_limit_runs_to_the_end": func(t *testing.T) {
			sb := idleSandbox(t)

			out := sh(t, sb.ID, "sleep 15 && echo done")

			if out.ExitCode != 0 || out.Stdout != "done\n" {
				t.Errorf("exit %d stdout %q stderr %q; want 0 and done", out.ExitCode, out.Stdout, out.Stderr)
			}
		},
		"a_sandbox_without_an_idle_limit_stays_running": func(t *testing.T) {
			sb := newSandbox(t, shellImage)

			time.Sleep((idleLimitSeconds + 5) * time.Second)

			if got := stateOf(t, sb.ID); got != "running" {
				t.Errorf("the sandbox is %s after %d idle seconds, want running", got, idleLimitSeconds+5)
			}
		},
	})
}
