//go:build linux

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames from to to and fails with an "exists" error when to is
// already there. The kernel does the check and the rename as one step.
func renameNoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
