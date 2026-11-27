package domain_test

import (
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

func TestValidateSandboxPath_accepts_workspace_paths(t *testing.T) {
	for _, p := range []string{"/workspace", "/workspace/main.py", "/workspace/a/b.txt"} {
		if _, err := domain.ValidateSandboxPath(p); err != nil {
			t.Errorf("ValidateSandboxPath(%q) = %v, want ok", p, err)
		}
	}
}

func TestValidateSandboxPath_rejects_traversal_and_escapes(t *testing.T) {
	bad := []string{
		"",                         // empty
		"relative/path",            // not absolute
		"/etc/passwd",              // outside workspace
		"/workspace/../etc/passwd", // traversal
		"/workspace/..",            // traversal
		"/work",                    // prefix-but-not-workspace
		"/workspaces/x",            // sibling prefix
		"/workspace/\x00evil",      // NUL
	}
	for _, p := range bad {
		if _, err := domain.ValidateSandboxPath(p); err != domain.ErrInvalidArgument {
			t.Errorf("ValidateSandboxPath(%q) = %v, want ErrInvalidArgument", p, err)
		}
	}
}

func TestValidateSandboxPath_normalizes(t *testing.T) {
	got, err := domain.ValidateSandboxPath("/workspace//a///b/")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "/workspace/a/b" {
		t.Errorf("cleaned = %q, want /workspace/a/b", got)
	}
}
