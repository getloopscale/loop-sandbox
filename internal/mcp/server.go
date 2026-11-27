package mcp

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// NewServer builds the MCP server exposing the sandbox tools over the SAME
// app.Service the REST API uses. The tool filter and every handler consult the
// ScopeTools binding, so a tool is neither listed NOR callable without the scope
// that grants it (the allowlist gates execution, not merely visibility).
func NewServer(svc *app.Service) *server.MCPServer {
	s := server.NewMCPServer("loopsandbox", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithToolFilter(scopeFilter),
	)
	h := &handlers{svc: svc}

	s.AddTool(mcp.NewTool(ToolCreateSandbox,
		mcp.WithDescription("Create a sandbox for the caller's tenant."),
		mcp.WithString("image", mcp.Description("container image (server allow-list)")),
		mcp.WithString("snapshot", mcp.Description("id of a snapshot to start from; cannot be combined with image")),
		mcp.WithNumber("ttl_seconds", mcp.Description("time-to-live in seconds")),
		mcp.WithArray("secrets", mcp.WithStringItems(), mcp.Description("names of the tenant's secrets the commands of the sandbox receive as environment variables")),
		mcp.WithNumber("auto_pause_seconds", mcp.Description("pause after this many idle seconds (10 to 86400) and resume on the next use; needs the capability pause")),
	), h.createSandbox)

	s.AddTool(mcp.NewTool(ToolExec,
		mcp.WithDescription("Run a command in a sandbox (argv, no shell)."),
		mcp.WithString("sandbox_id", mcp.Required()),
		mcp.WithArray("cmd", mcp.Required(), mcp.WithStringItems(), mcp.Description("argv")),
		mcp.WithNumber("timeout_ms", mcp.Description("per-command timeout")),
	), h.exec)

	s.AddTool(mcp.NewTool(ToolWriteFile,
		mcp.WithDescription("Write a file under /workspace."),
		mcp.WithString("sandbox_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("content", mcp.Required()),
	), h.writeFile)

	s.AddTool(mcp.NewTool(ToolReadFile,
		mcp.WithDescription("Read a file under /workspace."),
		mcp.WithString("sandbox_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
	), h.readFile)

	s.AddTool(mcp.NewTool(ToolListFiles,
		mcp.WithDescription("List a directory under /workspace."),
		mcp.WithString("sandbox_id", mcp.Required()),
		mcp.WithString("path", mcp.Description("directory (default /workspace)")),
	), h.listFiles)

	s.AddTool(mcp.NewTool(ToolDestroySandbox,
		mcp.WithDescription("Destroy a sandbox."),
		mcp.WithString("sandbox_id", mcp.Required()),
	), h.destroySandbox)

	// M18 computer-use tools — drive a headless browser in the sandbox.
	s.AddTool(mcp.NewTool(ToolBrowserNavigate,
		mcp.WithDescription("Navigate the sandbox browser to a URL."),
		mcp.WithString("sandbox_id", mcp.Required()),
		mcp.WithString("url", mcp.Required()),
	), h.browserNavigate)
	s.AddTool(mcp.NewTool(ToolBrowserScreenshot,
		mcp.WithDescription("Capture a PNG screenshot of the sandbox browser (base64)."),
		mcp.WithString("sandbox_id", mcp.Required()),
	), h.browserScreenshot)
	s.AddTool(mcp.NewTool(ToolBrowserDOM,
		mcp.WithDescription("Get the DOM snapshot (outer HTML) of the sandbox browser page."),
		mcp.WithString("sandbox_id", mcp.Required()),
	), h.browserDOM)
	s.AddTool(mcp.NewTool(ToolBrowserClick,
		mcp.WithDescription("Click at (x, y) in the sandbox browser."),
		mcp.WithString("sandbox_id", mcp.Required()),
		mcp.WithNumber("x", mcp.Required()),
		mcp.WithNumber("y", mcp.Required()),
	), h.browserClick)
	s.AddTool(mcp.NewTool(ToolBrowserType,
		mcp.WithDescription("Type text into the focused element in the sandbox browser."),
		mcp.WithString("sandbox_id", mcp.Required()),
		mcp.WithString("text", mcp.Required()),
	), h.browserType)

	for _, t := range dataTools() {
		s.AddTool(t.spec, h.handler(t))
	}
	return s
}

func (h *handlers) browserNavigate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actor, deny := actorFor(ctx, ToolBrowserNavigate)
	if deny != nil {
		return deny, nil
	}
	id, err := req.RequireString("sandbox_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	url, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := h.svc.BrowserNavigate(ctx, actor, id, url); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("ok"), nil
}

func (h *handlers) browserScreenshot(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actor, deny := actorFor(ctx, ToolBrowserScreenshot)
	if deny != nil {
		return deny, nil
	}
	id, err := req.RequireString("sandbox_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	png, err := h.svc.BrowserScreenshot(ctx, actor, id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultImage("screenshot.png", base64.StdEncoding.EncodeToString(png), "image/png"), nil
}

func (h *handlers) browserDOM(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actor, deny := actorFor(ctx, ToolBrowserDOM)
	if deny != nil {
		return deny, nil
	}
	id, err := req.RequireString("sandbox_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	dom, err := h.svc.BrowserDOM(ctx, actor, id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(dom), nil
}

func (h *handlers) browserClick(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actor, deny := actorFor(ctx, ToolBrowserClick)
	if deny != nil {
		return deny, nil
	}
	id, err := req.RequireString("sandbox_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := h.svc.BrowserClick(ctx, actor, id, req.GetFloat("x", 0), req.GetFloat("y", 0)); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("ok"), nil
}

func (h *handlers) browserType(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actor, deny := actorFor(ctx, ToolBrowserType)
	if deny != nil {
		return deny, nil
	}
	id, err := req.RequireString("sandbox_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	text, err := req.RequireString("text")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := h.svc.BrowserType(ctx, actor, id, text); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("ok"), nil
}

// scopeFilter restricts the tool set to what the caller's scopes grant (top risk #1).
// No principal → no tools (fail-closed).
func scopeFilter(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
	p := auth.PrincipalFrom(ctx)
	if p == nil {
		return nil
	}
	allowed := AllowedTools(p.Scopes)
	out := make([]mcp.Tool, 0, len(tools))
	for _, t := range tools {
		if allowed[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

type handlers struct{ svc *app.Service }

// actorFor extracts the authenticated Actor and enforces that the tool is granted by
// the caller's scopes — the allowlist gates EXECUTION, not just listing (defense in
// depth over the tool filter). Fail-closed: no principal or ungranted tool → error.
func actorFor(ctx context.Context, tool string) (app.Actor, *mcp.CallToolResult) {
	p := auth.PrincipalFrom(ctx)
	if p == nil {
		return app.Actor{}, mcp.NewToolResultError("unauthorized")
	}
	if !AllowedTools(p.Scopes)[tool] {
		return app.Actor{}, mcp.NewToolResultError("forbidden: scope does not grant " + tool)
	}
	return app.Actor{TenantID: p.TenantID, KeyID: p.KeyID, RequestID: uuid.NewString()}, nil
}

func (h *handlers) createSandbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actor, deny := actorFor(ctx, ToolCreateSandbox)
	if deny != nil {
		return deny, nil
	}
	image := req.GetString("image", "")
	ttl := time.Duration(req.GetInt("ttl_seconds", 0)) * time.Second
	sb, err := h.svc.CreateSandboxFromSpec(ctx, actor, app.SandboxSpec{
		Image: image, Snapshot: req.GetString("snapshot", ""), TTL: ttl,
		AutoPause: time.Duration(req.GetInt("auto_pause_seconds", 0)) * time.Second,
		Secrets:   req.GetStringSlice("secrets", nil),
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultJSON(map[string]any{
		"id": sb.ID, "tenant_id": sb.TenantID, "state": string(sb.State),
	})
}

func (h *handlers) exec(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actor, deny := actorFor(ctx, ToolExec)
	if deny != nil {
		return deny, nil
	}
	id, err := req.RequireString("sandbox_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	cmd, err := req.RequireStringSlice("cmd")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	res, err := h.svc.Exec(ctx, actor, id, runtime.ExecRequest{
		Cmd:     cmd,
		Timeout: time.Duration(req.GetInt("timeout_ms", 0)) * time.Millisecond,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// A non-zero exit is a normal result (US-02), not a tool error.
	return mcp.NewToolResultJSON(map[string]any{
		"exit_code": res.ExitCode, "stdout": string(res.Stdout), "stderr": string(res.Stderr),
	})
}

func (h *handlers) writeFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actor, deny := actorFor(ctx, ToolWriteFile)
	if deny != nil {
		return deny, nil
	}
	id, e1 := req.RequireString("sandbox_id")
	path, e2 := req.RequireString("path")
	content, e3 := req.RequireString("content")
	if err := firstErr(e1, e2, e3); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := h.svc.WriteFile(ctx, actor, id, path, strings.NewReader(content)); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("ok"), nil
}

func (h *handlers) readFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actor, deny := actorFor(ctx, ToolReadFile)
	if deny != nil {
		return deny, nil
	}
	id, e1 := req.RequireString("sandbox_id")
	path, e2 := req.RequireString("path")
	if err := firstErr(e1, e2); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	rc, err := h.svc.ReadFile(ctx, actor, id, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, 32<<20))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

func (h *handlers) listFiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actor, deny := actorFor(ctx, ToolListFiles)
	if deny != nil {
		return deny, nil
	}
	id, err := req.RequireString("sandbox_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	entries, err := h.svc.ListFiles(ctx, actor, id, req.GetString("path", "/workspace"))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{"name": e.Name, "size": e.Size, "is_dir": e.IsDir})
	}
	return mcp.NewToolResultJSON(map[string]any{"files": out})
}

func (h *handlers) destroySandbox(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	actor, deny := actorFor(ctx, ToolDestroySandbox)
	if deny != nil {
		return deny, nil
	}
	id, err := req.RequireString("sandbox_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := h.svc.DestroySandbox(ctx, actor, id); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("destroyed"), nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
