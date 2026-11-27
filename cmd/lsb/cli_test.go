package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/ratelimit"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

type keyAuth struct{}

// Authenticate accepts "key-a" and "key-b", one tenant each.
func (keyAuth) Authenticate(_ context.Context, raw string) (*auth.Principal, error) {
	switch strings.TrimPrefix(raw, "Bearer ") {
	case "key-a":
		return &auth.Principal{TenantID: "tenant-a", KeyID: "ka", Scopes: []string{"sandbox:execute"}}, nil
	case "key-b":
		return &auth.Principal{TenantID: "tenant-b", KeyID: "kb", Scopes: []string{"sandbox:execute"}}, nil
	}
	return nil, errors.New("unknown key")
}

type noAudit struct{}

func (noAudit) Record(_, _, _, _, _, _ string, _ time.Time) {}

// harness runs the CLI against the real API router over HTTP, with the fake
// runtime driver.
type harness struct {
	t      *testing.T
	url    string
	driver *fake.Driver
}

func newHarness(t *testing.T, mw ...api.Middleware) *harness {
	t.Helper()
	driver := fake.NewWithSessions()
	svc := app.NewService(memory.New(), driver, clock.Fixed{T: time.Unix(1_700_000_000, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	chain := auth.Middleware(keyAuth{})
	for _, m := range mw {
		chain = api.Chain(chain, m)
	}
	srv := httptest.NewServer(api.NewRouter(api.NewHandler(svc, nil), chain, nil))
	t.Cleanup(srv.Close)
	return &harness{t: t, url: srv.URL, driver: driver}
}

type outcome struct {
	code           int
	stdout, stderr string
}

// lsbAs runs the CLI with the given API key and stdin.
func (h *harness) lsbAs(key, stdin string, args ...string) outcome {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	env := map[string]string{"LOOPSANDBOX_URL": h.url, "LOOPSANDBOX_API_KEY": key}
	c := &cli{stdin: strings.NewReader(stdin), stdout: &stdout, stderr: &stderr, getenv: func(k string) string { return env[k] }}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := c.run(ctx, args)
	return outcome{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func (h *harness) lsb(args ...string) outcome { return h.lsbAs("key-a", "", args...) }

// create makes a sandbox and returns its id.
func (h *harness) create(args ...string) string {
	h.t.Helper()
	out := h.lsb(append([]string{"sandbox", "create"}, args...)...)
	if out.code != 0 {
		h.t.Fatalf("sandbox create exited %d: %s", out.code, out.stderr)
	}
	return strings.TrimSpace(out.stdout)
}

func TestCreatePrintsTheIDOfARunningSandbox(t *testing.T) {
	h := newHarness(t)

	id := h.create("--image", "busybox:latest", "--ttl", "10m", "--label", "team=search")

	var listed []sandbox
	out := h.lsb("sandbox", "list", "--json")
	if err := json.Unmarshal([]byte(out.stdout), &listed); err != nil {
		t.Fatalf("list --json printed %q: %v", out.stdout, err)
	}
	if len(listed) != 1 || listed[0].ID != id || listed[0].State != "running" || listed[0].Metadata["team"] != "search" ||
		listed[0].ExpiresAt != "2023-11-14T22:23:20Z" {
		t.Errorf("listed = %+v, want the running sandbox %s with its label and a 10 minute lifetime", listed, id)
	}
}

func TestListPrintsATableWithOneRowPerSandbox(t *testing.T) {
	h := newHarness(t)
	first, second := h.create(), h.create()

	out := h.lsb("sandbox", "list")

	lines := strings.Split(strings.TrimSpace(out.stdout), "\n")
	if out.code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") ||
		!strings.Contains(out.stdout, first) || !strings.Contains(out.stdout, second) {
		t.Errorf("exit %d, output:\n%s\nwant a header and the rows of %s and %s", out.code, out.stdout, first, second)
	}
}

func TestListShowsOnlyTheSandboxesOfTheKey(t *testing.T) {
	h := newHarness(t)
	id := h.create()

	out := h.lsbAs("key-b", "", "sandbox", "list")

	if out.code != 0 || strings.Contains(out.stdout, id) {
		t.Errorf("another tenant's list (exit %d) shows %s:\n%s", out.code, id, out.stdout)
	}
}

func TestExecPrintsTheOutputAndReturnsTheExitCodeOfTheCommand(t *testing.T) {
	h := newHarness(t)
	id := h.create()

	// The fake runtime knows echo, and cat of a missing file fails with exit 1.
	ok := h.lsb("sandbox", "exec", id, "--", "echo", "out", "put")
	failed := h.lsb("sandbox", "exec", "--timeout", "5s", id, "--", "cat", "/workspace/missing")

	if ok.code != 0 || ok.stdout != "out put\n" || ok.stderr != "" {
		t.Errorf("exit %d stdout %q stderr %q, want 0 and the echoed words", ok.code, ok.stdout, ok.stderr)
	}
	if failed.code != 1 || failed.stdout != "" || failed.stderr != "cat: no such file" {
		t.Errorf("exit %d stdout %q stderr %q, want the command's exit 1 and its stderr", failed.code, failed.stdout, failed.stderr)
	}
}

func TestKillDestroysEverySandboxNamedAndReportsTheOnesItCouldNot(t *testing.T) {
	h := newHarness(t)
	first, second := h.create(), h.create()

	out := h.lsb("sandbox", "kill", first, "sbx_missing", second)

	left := h.lsb("sandbox", "list", "--state", "running")
	if out.code != exitError || !strings.Contains(out.stderr, "sbx_missing") || out.stdout != first+"\n"+second+"\n" {
		t.Errorf("exit %d stdout %q stderr %q, want 1, both ids printed and the missing one reported", out.code, out.stdout, out.stderr)
	}
	if strings.Contains(left.stdout, first) || strings.Contains(left.stdout, second) {
		t.Errorf("a killed sandbox is still running:\n%s", left.stdout)
	}
}

func TestConnectRunsPipedInputInAShellOfTheSandbox(t *testing.T) {
	h := newHarness(t)
	id := h.create()

	out := h.lsbAs("key-a", "echo marker-$((6*7))\nexit\n", "sandbox", "connect", id)

	if out.code != 0 || !strings.Contains(out.stdout, "marker-42") {
		t.Errorf("exit %d, output %q (stderr %q); want the shell to print marker-42", out.code, out.stdout, out.stderr)
	}
}

func TestConnectReportsTheErrorOfTheAPI(t *testing.T) {
	h := newHarness(t)

	out := h.lsb("sandbox", "connect", "sbx_missing")

	if out.code != exitError || !strings.Contains(out.stderr, "404") {
		t.Errorf("exit %d stderr %q, want 1 and the 404 of the API", out.code, out.stderr)
	}
}

func TestAnAPIErrorIsPrintedWithItsCode(t *testing.T) {
	h := newHarness(t)

	out := h.lsb("sandbox", "exec", "sbx_missing", "--", "true")

	if out.code != exitError || !strings.Contains(out.stderr, "not_found") {
		t.Errorf("exit %d stderr %q, want 1 and the code not_found", out.code, out.stderr)
	}
}

func TestARateLimitedRequestSaysWhenToRetry(t *testing.T) {
	frozen := time.Unix(1_700_000_000, 0)
	h := newHarness(t, api.RateLimit(ratelimit.New(1, 1), func() time.Time { return frozen }))
	h.lsb("sandbox", "list")

	out := h.lsb("sandbox", "list")

	if out.code != exitError || !strings.Contains(out.stderr, "rate_limited") || !strings.Contains(out.stderr, "retry after 1s") {
		t.Errorf("exit %d stderr %q, want 1, rate_limited and the wait", out.code, out.stderr)
	}
}

func TestAnInvalidKeyIsReportedAsUnauthorized(t *testing.T) {
	h := newHarness(t)

	out := h.lsbAs("wrong", "", "sandbox", "list")

	if out.code != exitError || !strings.Contains(out.stderr, "401") {
		t.Errorf("exit %d stderr %q, want 1 and HTTP 401", out.code, out.stderr)
	}
}

func TestMistakesInTheCommandLineExitWithTwoAndShowTheUsage(t *testing.T) {
	h := newHarness(t)
	cases := map[string][]string{
		"no command":                 {},
		"unknown command":            {"volume", "list"},
		"sandbox without subcommand": {"sandbox"},
		"unknown subcommand":         {"sandbox", "frobnicate"},
		"unknown flag":               {"sandbox", "list", "--nope"},
		"image and template":         {"sandbox", "create", "--image", "a", "--template", "b"},
		"env without a value":        {"sandbox", "create", "--env", "NOEQUALS"},
		"exec without a command":     {"sandbox", "exec", "sbx_1"},
		"exec with only the dashes":  {"sandbox", "exec", "sbx_1", "--"},
		"logs without a sandbox":     {"sandbox", "logs"},
		"kill without a sandbox":     {"sandbox", "kill"},
		"connect with two sandboxes": {"sandbox", "connect", "a", "b"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			out := h.lsb(args...)

			if out.code != exitUsage || !strings.Contains(out.stderr, "Usage:") {
				t.Errorf("exit %d stderr %q, want 2 and the usage", out.code, out.stderr)
			}
		})
	}
}

func TestAMissingAPIKeyIsNamedInTheError(t *testing.T) {
	h := newHarness(t)

	out := h.lsbAs("", "", "sandbox", "list")

	if out.code != exitError || !strings.Contains(out.stderr, "LOOPSANDBOX_API_KEY") {
		t.Errorf("exit %d stderr %q, want 1 and the name of the variable", out.code, out.stderr)
	}
}

func TestAnAddressThatIsNotHTTPIsRefused(t *testing.T) {
	var stderr bytes.Buffer
	env := map[string]string{"LOOPSANDBOX_URL": "localhost:8080", "LOOPSANDBOX_API_KEY": "k"}
	c := &cli{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &stderr, getenv: func(k string) string { return env[k] }}

	code := c.run(context.Background(), []string{"sandbox", "list"})

	if code != exitError || !strings.Contains(stderr.String(), "LOOPSANDBOX_URL") {
		t.Errorf("exit %d stderr %q, want 1 and the name of the variable", code, stderr.String())
	}
}

func TestHelpPrintsTheUsageAndExitsZero(t *testing.T) {
	h := newHarness(t)

	out := h.lsb("help")

	if out.code != 0 || !strings.Contains(out.stdout, "lsb sandbox create") {
		t.Errorf("exit %d stdout %q, want 0 and the usage", out.code, out.stdout)
	}
}

func TestLogsOfAnUnknownSandboxFail(t *testing.T) {
	h := newHarness(t)

	out := h.lsb("sandbox", "logs", "sbx_missing")

	if out.code != exitError || !strings.Contains(out.stderr, "404") {
		t.Errorf("exit %d stderr %q, want 1 and HTTP 404", out.code, out.stderr)
	}
}
