// Package mcp exposes the sandbox over the Model Context Protocol (Model B broker),
// as a SECOND surface over the SAME app.Service the REST API uses — so the same
// consumer key resolves to the same tenant and data on both surfaces (cross-surface
// consistency by construction). The capability binding (ScopeTools) is the SINGLE
// source of truth for which tools a scope may see and call — no tool is reachable
// without the scope that lists it (defense against tool-allowlist drift).
package mcp

// Tool names exposed over MCP. These map 1:1 to app.Service operations.
const (
	ToolCreateSandbox  = "create_sandbox"
	ToolExec           = "exec"
	ToolWriteFile      = "write_file"
	ToolReadFile       = "read_file"
	ToolListFiles      = "list_files"
	ToolDestroySandbox = "destroy_sandbox"
	// M18 computer-use tools.
	ToolBrowserNavigate   = "browser_navigate"
	ToolBrowserScreenshot = "browser_screenshot"
	ToolBrowserDOM        = "browser_dom"
	ToolBrowserClick      = "browser_click"
	ToolBrowserType       = "browser_type"
)

// ScopeExecute is the scope granting the sandbox tool-set (aligned with REST M7).
//
// Note on the standalone-mode surface asymmetry (intentional, safe direction): the
// MCP surface always requires this scope (capability-scoped, fail-closed), whereas
// standalone REST is coarse-grained (any authenticated key may call any endpoint —
// the M1 design). MCP is therefore the STRICTER surface and cannot bypass anything
// REST already permits (no privilege escalation). In `loopscale` mode both surfaces
// require the scope at the edge (forward-auth / mcp-resolve). Tightening standalone
// REST to match MCP is the safe future direction; weakening MCP is never correct.
const ScopeExecute = "sandbox:execute"

// scopeTools is the capability binding: scope → the tools it grants. It is the ONLY
// place that decides tool exposure/access. Adding a tool without listing it here
// makes it unreachable (fail-closed); listing a dangerous tool under a broad scope
// is caught in review because this table is the single audit point.
var scopeTools = map[string][]string{
	ScopeExecute: {
		ToolCreateSandbox, ToolExec, ToolWriteFile, ToolReadFile, ToolListFiles, ToolDestroySandbox,
		ToolBrowserNavigate, ToolBrowserScreenshot, ToolBrowserDOM, ToolBrowserClick, ToolBrowserType,
	},
}

// The tools defined as data in tools.go are part of the same tool-set.
func init() {
	for _, t := range dataTools() {
		scopeTools[ScopeExecute] = append(scopeTools[ScopeExecute], t.spec.Name)
	}
}

// AllowedTools returns the set of tool names the given scopes grant (the union).
func AllowedTools(scopes []string) map[string]bool {
	allowed := map[string]bool{}
	for _, s := range scopes {
		for _, tool := range scopeTools[s] {
			allowed[tool] = true
		}
	}
	return allowed
}

// AllToolNames returns every tool the binding knows (for registration).
func AllToolNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, tools := range scopeTools {
		for _, t := range tools {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}
