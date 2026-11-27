package mcp_test

import (
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/mcp"
)

func TestAllowedTools_grants_full_set_for_execute_scope(t *testing.T) {
	allowed := mcp.AllowedTools([]string{mcp.ScopeExecute})
	for _, want := range []string{
		mcp.ToolCreateSandbox, mcp.ToolExec, mcp.ToolWriteFile,
		mcp.ToolReadFile, mcp.ToolListFiles, mcp.ToolDestroySandbox,
	} {
		if !allowed[want] {
			t.Errorf("scope %q should grant %q", mcp.ScopeExecute, want)
		}
	}
}

// Top risk #1: a principal WITHOUT sandbox:execute must be granted NOTHING — a
// dangerous tool (exec/destroy) is never reachable without the scope that lists it.
func TestAllowedTools_no_scope_grants_nothing(t *testing.T) {
	if got := mcp.AllowedTools(nil); len(got) != 0 {
		t.Errorf("no scopes → %v, want empty", got)
	}
	if got := mcp.AllowedTools([]string{"unrelated:scope"}); len(got) != 0 {
		t.Errorf("unrelated scope → %v, want empty (exec/destroy NOT exposed)", got)
	}
	if mcp.AllowedTools([]string{"unrelated:scope"})[mcp.ToolExec] {
		t.Error("exec reachable without sandbox:execute — allowlist drift")
	}
}

func TestAllToolNames_covers_the_binding(t *testing.T) {
	if got, want := len(mcp.AllToolNames()), len(mcp.Coverage()); got != want {
		t.Errorf("AllToolNames = %d, want the %d tools of the coverage table", got, want)
	}
}
