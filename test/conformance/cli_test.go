//go:build conformance

package conformance

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	lsbOnce sync.Once
	lsbPath string
	lsbErr  error
)

// lsbBinary builds the user CLI once for the whole run.
func lsbBinary(t *testing.T) string {
	t.Helper()
	lsbOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lsb-conformance-")
		if err != nil {
			lsbErr = err
			return
		}
		lsbPath = filepath.Join(dir, "lsb")
		out, err := exec.Command("go", "build", "-o", lsbPath, "github.com/getloopscale/loop-sandbox/cmd/lsb").CombinedOutput()
		if err != nil {
			lsbErr = errors.New("build lsb: " + err.Error() + ": " + string(out))
		}
	})
	if lsbErr != nil {
		t.Fatal(lsbErr)
	}
	return lsbPath
}

type cliResult struct {
	code           int
	stdout, stderr string
}

// lsb runs the CLI against the deployment under test with the suite's API key.
func lsb(t *testing.T, stdin string, args ...string) cliResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, lsbBinary(t), args...)
	cmd.Env = append(os.Environ(), "LOOPSANDBOX_URL="+baseURL, "LOOPSANDBOX_API_KEY="+apiKey)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("run lsb %v: %v", args, err)
	}
	return cliResult{code: cmd.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// The user CLI works with nothing but the address of the API and an API key.
func TestCLI(t *testing.T) {
	supported["cli"] = true
	// created makes a sandbox with the CLI and destroys it when the case ends.
	created := func(t *testing.T) string {
		t.Helper()
		res := lsb(t, "", "sandbox", "create", "--image", shellImage, "--ttl", "5m")
		id := strings.TrimSpace(res.stdout)
		if res.code != 0 || id == "" {
			t.Fatalf("lsb sandbox create exited %d: %s", res.code, res.stderr)
		}
		t.Cleanup(func() { destroy(t, id) })
		return id
	}
	cases := map[string]func(t *testing.T){
		"create_makes_a_running_sandbox_that_list_shows": func(t *testing.T) {
			id := created(t)

			res := lsb(t, "", "sandbox", "list", "--state", "running")

			if res.code != 0 || !strings.Contains(res.stdout, id) {
				t.Errorf("list exited %d and does not show %s:\n%s%s", res.code, id, res.stdout, res.stderr)
			}
		},
		"exec_prints_both_streams_and_returns_the_exit_code": func(t *testing.T) {
			id := created(t)

			res := lsb(t, "", "sandbox", "exec", id, "--", "sh", "-c", "echo out; echo err >&2; exit 7")

			if res.code != 7 || res.stdout != "out\n" || res.stderr != "err\n" {
				t.Errorf("exit %d stdout %q stderr %q, want 7, out and err", res.code, res.stdout, res.stderr)
			}
		},
		"kill_destroys_the_sandbox": func(t *testing.T) {
			id := created(t)

			res := lsb(t, "", "sandbox", "kill", id)

			if res.code != 0 || strings.TrimSpace(res.stdout) != id {
				t.Fatalf("kill exited %d: %s%s", res.code, res.stdout, res.stderr)
			}
			var sb sandbox
			got := call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+id, nil)
			if got.status == http.StatusOK {
				got.json(t, &sb)
			}
			if got.status != http.StatusNotFound && sb.State != "destroyed" {
				t.Errorf("after kill the sandbox answers %d in state %q", got.status, sb.State)
			}
		},
	}
	if supported["logs"] {
		cases["logs_prints_the_output_of_a_background_process"] = func(t *testing.T) {
			id := created(t)
			call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+id+"/processes",
				map[string]any{"cmd": []string{"sh", "-c", "echo from-the-process"}}).want(t, http.StatusCreated, "start process")
			var res cliResult

			// The process writes right after it starts; poll instead of sleeping a fixed time.
			for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
				if res = lsb(t, "", "sandbox", "logs", id); strings.Contains(res.stdout, "from-the-process") {
					break
				}
			}

			if res.code != 0 || !strings.Contains(res.stdout, "from-the-process") {
				t.Errorf("logs exited %d with %q (stderr %q), want the line of the process", res.code, res.stdout, res.stderr)
			}
		}
	}
	if supported["pty"] {
		cases["connect_runs_piped_input_in_a_shell"] = func(t *testing.T) {
			id := created(t)

			res := lsb(t, "echo marker-$((6*7))\nexit\n", "sandbox", "connect", id)

			if res.code != 0 || !strings.Contains(res.stdout, "marker-42") {
				t.Errorf("connect exited %d with %q (stderr %q), want marker-42", res.code, res.stdout, res.stderr)
			}
		}
	}
	capability(t, "cli", nil, cases)
}
