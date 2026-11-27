package container

import (
	"context"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// Exit codes the file scripts use to report a condition instead of a failure.
const (
	exitNoSuchPath = 44
	exitPathExists = 45
)

// exitLeavesWorkspace is the exit code of a script whose path, once symbolic
// links are resolved, is outside /workspace.
const exitLeavesWorkspace = 46

// The scripts below take every path as a positional argument, never interpolated
// into the script text, and need only sh and busybox-level tools.

// confineFunc defines confine PATH: it resolves the deepest part of PATH that
// exists and exits 46 unless the result is /workspace or under it. The API
// promises that a file operation stays in the workspace, and a symbolic link
// created by code in the sandbox must not turn that into a lie.
//
// Code running in the sandbox can swap a link between this check and the
// operation. That grants it nothing: the operation runs as the sandbox's own
// user, inside the same container.
const confineFunc = `confine() {
  p=$1
  while [ ! -e "$p" ] && [ ! -L "$p" ]; do p=$(dirname -- "$p"); done
  r=$(readlink -f -- "$p" 2>/dev/null) || exit 46
  case "$r" in /workspace|/workspace/*) ;; *) exit 46 ;; esac
}
`

// confineParent applies confine to the directory that holds $0, for operations
// that act on $0 itself and do not follow it when it is a link.
const confineParent = `[ "$0" = /workspace ] || confine "$(dirname -- "$0")"
`

// writeScript stores stdin at $0, creating its parent directories.
const writeScript = confineFunc + `confine "$0"
mkdir -p -- "$(dirname -- "$0")" && cat > "$0"`

// readScript prints the file at $0.
const readScript = confineFunc + `confine "$0"
[ -f "$0" ] || exit 44
cat -- "$0"`

// deleteScript removes the file or link at $0; a missing path is not an error.
const deleteScript = confineFunc + confineParent + `rm -f -- "$0"`

// statScript prints "<size>/<type>/<octal mode>/<mtime epoch>" for the path in $0.
// A link is described as the link.
const statScript = confineFunc + confineParent + `[ -e "$0" ] || [ -L "$0" ] || exit 44
stat -c '%s/%F/%a/%Y' -- "$0"`

// mkdirScript creates the directory in $0 with its parents.
const mkdirScript = confineFunc + `confine "$0"
[ -d "$0" ] && exit 0
{ [ -e "$0" ] || [ -L "$0" ]; } && exit 45
mkdir -p -- "$0"`

// moveScript moves $0 to $1, creating the parents of $1. $2 is "1" to replace an
// existing $1. The existence check and the move are two steps: a path created in
// between by code in the sandbox is replaced.
const moveScript = confineFunc + confineParent + `confine "$(dirname -- "$1")"
[ -e "$0" ] || [ -L "$0" ] || exit 44
if [ -e "$1" ] || [ -L "$1" ]; then
  [ "$2" = 1 ] || exit 45
  rm -rf -- "$1"
fi
mkdir -p -- "$(dirname -- "$1")" && mv -- "$0" "$1"`

// runFileScript runs one of the scripts and maps its reserved exit codes.
func (d *Driver) runFileScript(ctx context.Context, id runtime.SandboxID, what, script string, args ...string) (*runtime.ExecResult, error) {
	return d.runFileScriptStdin(ctx, id, nil, what, script, args...)
}

// runFileScriptStdin is runFileScript with data for the script's standard input.
func (d *Driver) runFileScriptStdin(ctx context.Context, id runtime.SandboxID, stdin io.Reader, what, script string, args ...string) (*runtime.ExecResult, error) {
	res, err := d.Exec(ctx, id, runtime.ExecRequest{Cmd: append([]string{"sh", "-c", script}, args...), Stdin: stdin})
	if err != nil {
		return nil, err
	}
	switch res.ExitCode {
	case 0:
		return res, nil
	case exitNoSuchPath:
		return nil, runtime.ErrFileNotFound
	case exitPathExists:
		return nil, runtime.ErrFileExists
	case exitLeavesWorkspace:
		return nil, fmt.Errorf("%w: %s %q: the path leaves /workspace through a symbolic link", runtime.ErrInvalidRequest, what, args[0])
	default:
		return nil, fmt.Errorf("%s %q failed (exit %d): %s", what, args[0], res.ExitCode, clipErr(res.Stderr))
	}
}

// StatFile implements runtime.FileOpsDriver.
func (d *Driver) StatFile(ctx context.Context, id runtime.SandboxID, filePath string) (*runtime.FileInfo, error) {
	res, err := d.runFileScript(ctx, id, "stat", statScript, filePath)
	if err != nil {
		return nil, err
	}
	info, err := parseStat(string(res.Stdout))
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", filePath, err)
	}
	info.Name = path.Base(filePath)
	return info, nil
}

// parseStat reads one statScript line: size, file type, octal mode, mtime.
func parseStat(out string) (*runtime.FileInfo, error) {
	fields := strings.Split(strings.TrimSpace(out), "/")
	if len(fields) != 4 {
		return nil, fmt.Errorf("unexpected stat output %q", out)
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("stat size %q: %w", fields[0], err)
	}
	mode, err := strconv.ParseUint(fields[2], 8, 32)
	if err != nil {
		return nil, fmt.Errorf("stat mode %q: %w", fields[2], err)
	}
	mtime, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("stat mtime %q: %w", fields[3], err)
	}
	info := &runtime.FileInfo{Size: size, IsDir: fields[1] == "directory", Mode: uint32(mode) & 0o7777, ModTime: time.Unix(mtime, 0).UTC()}
	if info.IsDir {
		info.Size = 0 // a directory's own inode size says nothing about its content
	}
	return info, nil
}

// MakeDir implements runtime.FileOpsDriver.
func (d *Driver) MakeDir(ctx context.Context, id runtime.SandboxID, dirPath string) error {
	_, err := d.runFileScript(ctx, id, "mkdir", mkdirScript, dirPath)
	return err
}

// MoveFile implements runtime.FileOpsDriver.
func (d *Driver) MoveFile(ctx context.Context, id runtime.SandboxID, from, to string, overwrite bool) error {
	flag := "0"
	if overwrite {
		flag = "1"
	}
	_, err := d.runFileScript(ctx, id, "move", moveScript, from, to, flag)
	return err
}

var _ runtime.FileOpsDriver = (*Driver)(nil)
