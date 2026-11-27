package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	gomcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/mcp"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// stubAuth: "key-a" → tenant-a WITH sandbox:execute; "key-noscope" → tenant-c with
// NO scope; anything else → unauthorized.
type stubAuth struct{}

func (stubAuth) Authenticate(_ context.Context, raw string) (*auth.Principal, error) {
	switch strings.TrimPrefix(strings.TrimSpace(raw), "Bearer ") {
	case "key-a":
		return &auth.Principal{TenantID: "tenant-a", KeyID: "ka", Scopes: []string{mcp.ScopeExecute}}, nil
	case "key-b":
		return &auth.Principal{TenantID: "tenant-b", KeyID: "kb", Scopes: []string{mcp.ScopeExecute}}, nil
	case "key-noscope":
		return &auth.Principal{TenantID: "tenant-c", KeyID: "kc", Scopes: nil}, nil
	default:
		return nil, auth.ErrUnauthorized
	}
}

type noAudit struct{}

func (noAudit) Record(_, _, _, _, _, _ string, _ time.Time) {}

type seqID struct{ n int }

func (s *seqID) New() string { s.n++; return "sbx-" + string(rune('a'+s.n)) }

func newSvc() *app.Service {
	return app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqID{}, noAudit{}, nil, nil)
}

func mcpServerFor(t *testing.T, svc *app.Service) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(mcp.NewHTTPHandler(mcp.NewServer(svc), stubAuth{}))
	t.Cleanup(srv.Close)
	return srv
}

func mcpServer(t *testing.T) *httptest.Server {
	t.Helper()
	return mcpServerFor(t, newSvc())
}

func newClient(t *testing.T, url, key string) *mcpclient.Client {
	t.Helper()
	c, err := mcpclient.NewStreamableHttpClient(url, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + key}))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	var initReq gomcp.InitializeRequest
	initReq.Params.ProtocolVersion = gomcp.LATEST_PROTOCOL_VERSION
	if _, err := c.Initialize(context.Background(), initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c
}

func TestHTTP_auth_fail_closed(t *testing.T) {
	srv := mcpServer(t)
	c, err := mcpclient.NewStreamableHttpClient(srv.URL, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer nope"}))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	_ = c.Start(context.Background())
	defer func() { _ = c.Close() }()
	var initReq gomcp.InitializeRequest
	initReq.Params.ProtocolVersion = gomcp.LATEST_PROTOCOL_VERSION
	if _, err := c.Initialize(context.Background(), initReq); err == nil {
		t.Error("initialize with bad credential succeeded, want 401 fail-closed")
	}
}

func TestHTTP_tools_list_filtered_by_scope(t *testing.T) {
	srv := mcpServer(t)

	withScope := newClient(t, srv.URL, "key-a")
	res, err := withScope.ListTools(context.Background(), gomcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools(scoped): %v", err)
	}
	// Every tool of the coverage table is registered on the server and listed.
	if want := len(mcp.Coverage()); len(res.Tools) != want {
		t.Errorf("scoped tools = %d, want %d", len(res.Tools), want)
	}

	noScope := newClient(t, srv.URL, "key-noscope")
	res2, err := noScope.ListTools(context.Background(), gomcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools(no scope): %v", err)
	}
	if len(res2.Tools) != 0 {
		t.Errorf("unscoped tools = %d, want 0 (allowlist gates visibility)", len(res2.Tools))
	}
}

func TestHTTP_full_flow_create_via_mcp(t *testing.T) {
	srv := mcpServer(t)
	c := newClient(t, srv.URL, "key-a")

	var call gomcp.CallToolRequest
	call.Params.Name = mcp.ToolCreateSandbox
	call.Params.Arguments = map[string]any{"image": "busybox:latest", "ttl_seconds": 300}
	res, err := c.CallTool(context.Background(), call)
	if err != nil {
		t.Fatalf("CallTool create: %v", err)
	}
	if res.IsError {
		t.Fatalf("create tool errored: %+v", res.Content)
	}
	// The result carries the created sandbox id + tenant.
	txt := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(gomcp.TextContent); ok {
			txt = tc.Text
		}
	}
	if !strings.Contains(txt, "tenant-a") || !strings.Contains(txt, "sbx-") {
		t.Errorf("create result = %q, want tenant-a + sandbox id", txt)
	}

}

// A no-scope client must be BLOCKED from tools/call exec over the wire — the library
// filter (ErrToolNotFound) or the actorFor gate must fire. Strong assertion: the
// call must NOT succeed as a normal exec result.
func TestHTTP_call_ungranted_tool_is_blocked(t *testing.T) {
	srv := mcpServer(t)
	nc := newClient(t, srv.URL, "key-noscope")
	var exec gomcp.CallToolRequest
	exec.Params.Name = mcp.ToolExec
	exec.Params.Arguments = map[string]any{"sandbox_id": "x", "cmd": []any{"echo"}}
	er, err := nc.CallTool(context.Background(), exec)
	if err != nil {
		return // transport-level rejection (ErrToolNotFound) — blocked, acceptable.
	}
	if er == nil || !er.IsError {
		t.Fatalf("no-scope exec was NOT blocked (result=%+v) — allowlist must gate execution", er)
	}
}

// Cross-tenant spoof over HTTP: key-a creates a sandbox; key-b (a DIFFERENT real
// tenant, scoped) calls exec with that sandbox_id → not found, no data leak. The
// tenant comes from the credential, never from the argument (top risk #2).
func TestHTTP_cross_tenant_sandbox_id_spoof_is_not_found(t *testing.T) {
	srv := mcpServer(t)
	id := createViaMCP(t, srv.URL, "key-a")

	cb := newClient(t, srv.URL, "key-b")
	var exec gomcp.CallToolRequest
	exec.Params.Name = mcp.ToolExec
	exec.Params.Arguments = map[string]any{"sandbox_id": id, "cmd": []any{"echo", "pwn"}}
	res, err := cb.CallTool(context.Background(), exec)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	txt := resultText(res)
	if !res.IsError || !strings.Contains(txt, "not found") {
		t.Errorf("cross-tenant exec = %q (isErr %v), want not found", txt, res.IsError)
	}
	if strings.Contains(txt, "tenant-a") {
		t.Errorf("cross-tenant error leaked tenant-a detail: %q", txt)
	}
}

// Cross-surface over TWO real HTTP surfaces: a sandbox created via the REST HTTP API
// is exec'able via the MCP HTTP surface with the SAME key (same tenant/data), and a
// different tenant's key gets not-found on the MCP surface.
func TestHTTP_cross_surface_rest_create_mcp_exec(t *testing.T) {
	svc := newSvc()
	restH := api.NewHandler(svc, memory.NewIdempotency())
	restSrv := httptest.NewServer(api.NewRouter(restH, auth.Middleware(stubAuth{}), nil))
	t.Cleanup(restSrv.Close)
	mcpSrv := mcpServerFor(t, svc)

	// REST create (key-a).
	id := createViaREST(t, restSrv.URL, "key-a")

	// MCP exec on that id with key-a → sees the REST-created sandbox.
	ca := newClient(t, mcpSrv.URL, "key-a")
	var exec gomcp.CallToolRequest
	exec.Params.Name = mcp.ToolExec
	exec.Params.Arguments = map[string]any{"sandbox_id": id, "cmd": []any{"echo", "hi"}}
	res, err := ca.CallTool(context.Background(), exec)
	if err != nil || res.IsError {
		t.Fatalf("MCP exec on REST-created sandbox: err=%v result=%q", err, resultText(res))
	}

	// key-b (different tenant) → not found on MCP.
	cb := newClient(t, mcpSrv.URL, "key-b")
	var exec2 gomcp.CallToolRequest
	exec2.Params.Name = mcp.ToolExec
	exec2.Params.Arguments = map[string]any{"sandbox_id": id, "cmd": []any{"echo"}}
	res2, _ := cb.CallTool(context.Background(), exec2)
	if res2 == nil || !res2.IsError {
		t.Errorf("cross-surface cross-tenant exec was not isolated: %+v", res2)
	}
}

func resultText(r *gomcp.CallToolResult) string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	if tc, ok := r.Content[0].(gomcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

func createViaMCP(t *testing.T, url, key string) string {
	t.Helper()
	c := newClient(t, url, key)
	var call gomcp.CallToolRequest
	call.Params.Name = mcp.ToolCreateSandbox
	call.Params.Arguments = map[string]any{"image": "busybox:latest"}
	res, err := c.CallTool(context.Background(), call)
	if err != nil || res.IsError {
		t.Fatalf("createViaMCP: err=%v result=%q", err, resultText(res))
	}
	m, _ := res.StructuredContent.(map[string]any)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("createViaMCP: no id in %q", resultText(res))
	}
	return id
}

func createViaREST(t *testing.T, baseURL, key string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/sandboxes", strings.NewReader(`{"image":"busybox:latest"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("REST create: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("REST create status = %d, want 201", resp.StatusCode)
	}
	var body struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.ID == "" {
		t.Fatal("REST create returned no id")
	}
	return body.ID
}
