package domain

import "strings"

// workspaceRoot is the only writable path tree a sandbox file operation may touch.
const workspaceRoot = "/workspace"

// ValidateSandboxPath enforces that a client-supplied file path is confined to the
// sandbox workspace: absolute, rooted at /workspace, with no traversal. This is a
// trust-boundary check (path traversal / symlink escape defense — TRD §14.4).
// It returns the cleaned path or ErrInvalidArgument.
func ValidateSandboxPath(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "/") {
		return "", ErrInvalidArgument
	}
	// Reject traversal and control characters (incl. NUL and newline) before any
	// normalization — control chars break listing parsers and have no legit use in
	// a sandbox path.
	if strings.Contains(p, "..") {
		return "", ErrInvalidArgument
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", ErrInvalidArgument
		}
	}
	clean := cleanSlashPath(p)
	if clean != workspaceRoot && !strings.HasPrefix(clean, workspaceRoot+"/") {
		return "", ErrInvalidArgument
	}
	return clean, nil
}

// cleanSlashPath collapses duplicate and trailing slashes without importing
// path (which would also resolve "..", masking a traversal we want to reject).
func cleanSlashPath(p string) string {
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimRight(p, "/")
	}
	return p
}
