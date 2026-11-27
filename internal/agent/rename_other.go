//go:build !linux

package agent

import (
	"io/fs"
	"os"
)

// renameNoReplace renames from to to unless to exists. Outside Linux the check
// and the rename are two steps; the agent only runs for real on Linux.
func renameNoReplace(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: fs.ErrExist}
	}
	return os.Rename(from, to)
}
