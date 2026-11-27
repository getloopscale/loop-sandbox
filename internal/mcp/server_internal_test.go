package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	tcplan "github.com/getloopscale/loop-sandbox/internal/plan"
	gomcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

type noAudit struct{}

func (noAudit) Record(_, _, _, _, _, _ string, _ time.Time) {}

func newSvc() *app.Service {
	return app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		&seq{}, noAudit{}, nil, nil)
}

type seq struct{ n int }

func (s *seq) New() string { s.n++; return "sb-" + string(rune('a'+s.n)) }

func ctxWith(tenant string, scopes ...string) context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{TenantID: tenant, KeyID: "k", Scopes: scopes})
}

func callArgs(name string, args map[string]any) gomcp.CallToolRequest {
	var r gomcp.CallToolRequest
	r.Params.Name = name
	r.Params.Arguments = args
	return r
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

// createOne creates a sandbox via the MCP handler and returns its id.
func createOne(t *testing.T, h *handlers, ctx context.Context) string {
	t.Helper()
	res, err := h.createSandbox(ctx, callArgs(ToolCreateSandbox, map[string]any{"image": "busybox:latest"}))
	if err != nil || res.IsError {
		t.Fatalf("createSandbox: err=%v result=%q", err, resultText(res))
	}
	// StructuredContent is map[string]any{id,...}
	m, _ := res.StructuredContent.(map[string]any)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("no id in %q", resultText(res))
	}
	return id
}

func TestHandler_scope_gate_blocks_ungranted_tool(t *testing.T) {
	h := &handlers{svc: newSvc()}
	// principal WITHOUT sandbox:execute → exec must be forbidden (allowlist gates
	// EXECUTION, not just listing — top risk #1).
	res, _ := h.exec(ctxWith("t", "some:other"), callArgs(ToolExec, map[string]any{"sandbox_id": "x", "cmd": []any{"echo"}}))
	if !res.IsError || !strings.Contains(resultText(res), "forbidden") {
		t.Errorf("ungranted exec = %q (isErr %v), want forbidden", resultText(res), res.IsError)
	}
}

func TestHandler_unauthenticated_is_error(t *testing.T) {
	h := &handlers{svc: newSvc()}
	res, _ := h.createSandbox(context.Background(), callArgs(ToolCreateSandbox, nil)) // no principal
	if !res.IsError || !strings.Contains(resultText(res), "unauthorized") {
		t.Errorf("no principal = %q, want unauthorized", resultText(res))
	}
}

// Cross-surface consistency (top risk #2): a sandbox created for tenant-a is visible
// and executable via MCP with tenant-a's principal (same Service, same tenant), and
// NOT visible to tenant-b (isolation, US-05) — proven through the SAME app.Service
// the REST surface uses.
func TestHandler_cross_surface_same_tenant_and_isolation(t *testing.T) {
	h := &handlers{svc: newSvc()}
	ctxA := ctxWith("tenant-a", ScopeExecute)
	id := createOne(t, h, ctxA)

	// tenant-a: write then read the SAME file via MCP → same data.
	if r, _ := h.writeFile(ctxA, callArgs(ToolWriteFile, map[string]any{"sandbox_id": id, "path": "/workspace/a.txt", "content": "hi"})); r.IsError {
		t.Fatalf("writeFile: %q", resultText(r))
	}
	if r, _ := h.readFile(ctxA, callArgs(ToolReadFile, map[string]any{"sandbox_id": id, "path": "/workspace/a.txt"})); resultText(r) != "hi" {
		t.Errorf("readFile = %q, want hi (same data)", resultText(r))
	}

	// tenant-b: exec on tenant-a's sandbox → not_found (cross-tenant isolation).
	ctxB := ctxWith("tenant-b", ScopeExecute)
	r, _ := h.exec(ctxB, callArgs(ToolExec, map[string]any{"sandbox_id": id, "cmd": []any{"echo", "x"}}))
	if !r.IsError || !strings.Contains(resultText(r), "not found") {
		t.Errorf("cross-tenant exec = %q, want not found", resultText(r))
	}
}

func TestHandler_plan_enforcement_on_mcp(t *testing.T) {
	svc := newSvc()
	// starter creations cap 50, usage at 50 → MCP create must error (402-equivalent).
	su := stubPU{tier: tcplan.PlanStarter, value: 50}
	svc.SetEnforcer(enforcement.New(su), su)
	h := &handlers{svc: svc}
	res, _ := h.createSandbox(ctxWith("t", ScopeExecute), callArgs(ToolCreateSandbox, nil))
	if !res.IsError || !strings.Contains(resultText(res), "cost cap exceeded") {
		t.Errorf("over-cap MCP create = %q, want cost cap exceeded", resultText(res))
	}
}

type stubPU struct {
	tier  tcplan.Plan
	value int64
}

func (s stubPU) PlanFor(context.Context, string) (tcplan.Plan, error)        { return s.tier, nil }
func (s stubPU) Value(context.Context, string, string, int64) (int64, error) { return s.value, nil }

// TestHandler_every_tool_gated_by_its_own_scope proves each of the 6 handlers wired
// the CORRECT tool constant into actorFor: a no-scope principal is forbidden on every
// tool (a copy-paste of the wrong constant would let a wrong scope through).
func TestHandler_every_tool_gated_by_its_own_scope(t *testing.T) {
	h := &handlers{svc: newSvc()}
	noScope := ctxWith("t") // no scopes
	args := map[string]any{"sandbox_id": "x", "path": "/workspace/a", "content": "c", "cmd": []any{"echo"}}

	type call func(context.Context, gomcp.CallToolRequest) (*gomcp.CallToolResult, error)
	for name, fn := range map[string]call{
		ToolCreateSandbox:  h.createSandbox,
		ToolExec:           h.exec,
		ToolWriteFile:      h.writeFile,
		ToolReadFile:       h.readFile,
		ToolListFiles:      h.listFiles,
		ToolDestroySandbox: h.destroySandbox,
	} {
		res, _ := fn(noScope, callArgs(name, args))
		if !res.IsError || !strings.Contains(resultText(res), "forbidden") {
			t.Errorf("tool %q no-scope = %q (isErr %v), want forbidden", name, resultText(res), res.IsError)
		}
	}
}
