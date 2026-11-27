//go:build conformance

// Package conformance is the black-box conformance suite of LoopSandbox. It talks
// only to the HTTP API of a running deployment, so the same cases run against any
// runtime driver.
//
// For every capability the suite first asks GET /v1/capabilities. A capability the
// driver declares must pass its cases. A capability the driver does not declare is
// reported as "unsupported", after checking that its route answers 501: an
// undeclared capability that half-works is a failure, and so is a declared one
// that answers 501.
//
// Configuration (environment):
//
//	LSB_BASE_URL       API base URL (default http://localhost:8080)
//	LSB_API_KEY        API key of the tenant the cases run as (required). It needs the
//	                   scopes sandbox:execute and api_keys:manage.
//	LSB_API_KEY_OTHER  API key of a second tenant, for the isolation cases (required)
//	LSB_IMAGE          image for shell cases (default busybox:latest)
//	LSB_PYTHON_IMAGE   image for code-session cases (default python:3.12-slim)
//	LSB_BROWSER_IMAGE  image for browser cases (default browserless/chrome:latest)
//	LSB_TTL_GRACE      how long after its TTL a sandbox may still exist (default 60s);
//	                   set it above the deployment's reconcile interval
//	LSB_SUMMARY_FILE   also write the per-capability summary to this file
package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	baseURL      = envOr("LSB_BASE_URL", "http://localhost:8080")
	apiKey       = os.Getenv("LSB_API_KEY")
	otherAPIKey  = os.Getenv("LSB_API_KEY_OTHER")
	shellImage   = envOr("LSB_IMAGE", "busybox:latest")
	pythonImage  = envOr("LSB_PYTHON_IMAGE", "python:3.12-slim")
	browserImage = envOr("LSB_BROWSER_IMAGE", "browserless/chrome:latest")

	ttlGrace = durationOr("LSB_TTL_GRACE", 60*time.Second)

	httpClient = &http.Client{Timeout: 120 * time.Second}

	driverName string
	supported  = map[string]bool{}
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "conformance: %s=%q is not a positive duration\n", key, v)
		os.Exit(2)
	}
	return d
}

// outcome of one capability across all of its cases.
type outcome struct {
	passed, failed int
	unsupported    bool
}

var (
	resultsMu sync.Mutex
	results   = map[string]*outcome{}
)

func record(capability string, fn func(o *outcome)) {
	resultsMu.Lock()
	defer resultsMu.Unlock()
	o, ok := results[capability]
	if !ok {
		o = &outcome{}
		results[capability] = o
	}
	fn(o)
}

func TestMain(m *testing.M) {
	if apiKey == "" || otherAPIKey == "" {
		fmt.Fprintln(os.Stderr, "conformance: LSB_API_KEY and LSB_API_KEY_OTHER are required")
		os.Exit(2)
	}
	if err := loadCapabilities(); err != nil {
		fmt.Fprintf(os.Stderr, "conformance: cannot read capabilities from %s: %v\n", baseURL, err)
		os.Exit(2)
	}
	code := m.Run()
	printSummary()
	os.Exit(code)
}

func loadCapabilities() error {
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/capabilities", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /v1/capabilities: status %d: %s", resp.StatusCode, raw)
	}
	var body struct {
		Driver       string `json:"driver"`
		Capabilities []struct {
			Name      string `json:"name"`
			Supported bool   `json:"supported"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("decode capabilities: %w", err)
	}
	driverName = body.Driver
	for _, c := range body.Capabilities {
		supported[c.Name] = c.Supported
	}
	return nil
}

// printSummary writes the per-capability table to stdout and, when
// LSB_SUMMARY_FILE is set, to that file: go test hides the stdout of a passing
// package, and the table matters most when everything passes.
func printSummary() {
	var out strings.Builder
	writeSummary(&out)
	fmt.Print(out.String())
	if path := os.Getenv("LSB_SUMMARY_FILE"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "conformance: write summary to %s: %v\n", path, err)
		}
	}
}

func writeSummary(w *strings.Builder) {
	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "\nConformance summary (driver: %s)\n", driverName)  //nolint:errcheck // a strings.Builder never fails
	fmt.Fprintf(w, "%-16s %-12s %s\n", "CAPABILITY", "RESULT", "CASES") //nolint:errcheck // a strings.Builder never fails
	for _, name := range names {
		o := results[name]
		result := "pass"
		switch {
		case o.failed > 0:
			result = "FAIL"
		case o.unsupported:
			result = "unsupported"
		}
		fmt.Fprintf(w, "%-16s %-12s %d passed, %d failed\n", name, result, o.passed, o.failed) //nolint:errcheck // a strings.Builder never fails
	}
}

// capability runs the cases of one capability. When the driver does not declare
// it, probe must show the route answers 501, and the cases are skipped.
func capability(t *testing.T, name string, probe func(t *testing.T) *response, cases map[string]func(t *testing.T)) {
	t.Helper()
	declared, known := supported[name]
	if !known {
		record(name, func(o *outcome) { o.failed++ })
		t.Fatalf("capability %q is not listed by GET /v1/capabilities", name)
	}
	if !declared {
		t.Run("unsupported_route_answers_501", func(t *testing.T) {
			resp := probe(t)
			if resp.status != http.StatusNotImplemented {
				record(name, func(o *outcome) { o.failed++ })
				t.Fatalf("driver %q does not declare %q, but its route answered %d instead of 501: %s",
					driverName, name, resp.status, resp.body)
			}
			record(name, func(o *outcome) { o.unsupported = true })
		})
		return
	}
	caseNames := make([]string, 0, len(cases))
	for caseName := range cases {
		caseNames = append(caseNames, caseName)
	}
	sort.Strings(caseNames)
	for _, caseName := range caseNames {
		fn := cases[caseName]
		t.Run(caseName, func(t *testing.T) {
			t.Cleanup(func() {
				if t.Failed() {
					record(name, func(o *outcome) { o.failed++ })
				} else {
					record(name, func(o *outcome) { o.passed++ })
				}
			})
			fn(t)
		})
	}
}

// response is a fully-read HTTP response.
type response struct {
	status int
	header http.Header
	body   []byte
}

func (r *response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode response (status %d): %v: %s", r.status, err, r.body)
	}
}

// call sends one request as the given key. body may be nil, []byte (sent raw) or
// any value (sent as JSON).
func call(t *testing.T, key, method, path string, body any, headers ...string) *response {
	t.Helper()
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
		contentType = "application/octet-stream"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(raw)
		contentType = "application/json"
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: raw}
}

func (r *response) want(t *testing.T, status int, what string) *response {
	t.Helper()
	if r.status != status {
		t.Fatalf("%s: status = %d, want %d: %s", what, r.status, status, r.body)
	}
	return r
}

type sandbox struct {
	ID        string            `json:"id"`
	TenantID  string            `json:"tenant_id"`
	Image     string            `json:"image"`
	State     string            `json:"state"`
	CreatedAt string            `json:"created_at"`
	ExpiresAt string            `json:"expires_at"`
	Metadata  map[string]string `json:"metadata"`
	Env       map[string]string `json:"env"`
}

type sandboxList struct {
	Sandboxes  []sandbox `json:"sandboxes"`
	NextCursor string    `json:"next_cursor"`
}

// newSandboxWith creates a sandbox from a request body and destroys it when the
// test ends. The body's image and ttl_seconds default to shellImage and 600.
func newSandboxWith(t *testing.T, body map[string]any) sandbox {
	t.Helper()
	if _, ok := body["image"]; !ok {
		body["image"] = shellImage
	}
	if _, ok := body["ttl_seconds"]; !ok {
		body["ttl_seconds"] = 600
	}
	resp := call(t, apiKey, http.MethodPost, "/v1/sandboxes", body)
	resp.want(t, http.StatusCreated, "create sandbox")
	var sb sandbox
	resp.json(t, &sb)
	if sb.ID == "" {
		t.Fatalf("create sandbox: empty id: %s", resp.body)
	}
	t.Cleanup(func() { destroy(t, sb.ID) })
	return sb
}

// newSandbox creates a sandbox from image and destroys it when the test ends.
func newSandbox(t *testing.T, image string) sandbox {
	t.Helper()
	resp := call(t, apiKey, http.MethodPost, "/v1/sandboxes", map[string]any{"image": image, "ttl_seconds": 600})
	resp.want(t, http.StatusCreated, "create sandbox")
	var sb sandbox
	resp.json(t, &sb)
	if sb.ID == "" {
		t.Fatalf("create sandbox: empty id: %s", resp.body)
	}
	t.Cleanup(func() { destroy(t, sb.ID) })
	return sb
}

func destroy(t *testing.T, id string) {
	t.Helper()
	resp := call(t, apiKey, http.MethodDelete, "/v1/sandboxes/"+id, nil)
	if resp.status != http.StatusNoContent && resp.status != http.StatusNotFound {
		t.Errorf("destroy sandbox %s: status = %d: %s", id, resp.status, resp.body)
	}
}

type execResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func execIn(t *testing.T, id string, request map[string]any) execResult {
	t.Helper()
	resp := call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+id+"/executions", request)
	resp.want(t, http.StatusOK, "exec")
	var res execResult
	resp.json(t, &res)
	return res
}

func sh(t *testing.T, id, script string) execResult {
	t.Helper()
	return execIn(t, id, map[string]any{"cmd": []string{"sh", "-c", script}})
}

func writeFile(t *testing.T, id, path, content string) {
	t.Helper()
	call(t, apiKey, http.MethodPut, "/v1/sandboxes/"+id+"/files"+path, []byte(content)).
		want(t, http.StatusNoContent, "write "+path)
}

func readFile(t *testing.T, id, path string) *response {
	t.Helper()
	return call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+id+"/files"+path, nil)
}

// sseEvent is one Server-Sent Event with the time it arrived.
type sseEvent struct {
	name string
	data string
	at   time.Time
}

// readSSE reads a text/event-stream body to its end.
func readSSE(t *testing.T, body io.Reader) []sseEvent {
	t.Helper()
	var events []sseEvent
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 1024)
	for {
		n, err := body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			end := bytes.Index(buf, []byte("\n\n"))
			if end < 0 {
				break
			}
			ev := sseEvent{at: time.Now()}
			for _, line := range strings.Split(string(buf[:end]), "\n") {
				switch {
				case strings.HasPrefix(line, "event: "):
					ev.name = strings.TrimPrefix(line, "event: ")
				case strings.HasPrefix(line, "data: "):
					ev.data = strings.TrimPrefix(line, "data: ")
				}
			}
			events = append(events, ev)
			buf = buf[end+2:]
		}
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("read event stream: %v", err)
		}
	}
}
