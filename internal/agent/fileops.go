package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// fileErr maps a filesystem error to the Response of a file operation.
func fileErr(err error) *agentproto.Response {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &agentproto.Response{Err: agentproto.ErrNotFound}
	case errors.Is(err, fs.ErrExist):
		return &agentproto.Response{Err: agentproto.ErrExists}
	default:
		return &agentproto.Response{Err: err.Error()}
	}
}

func (h *Handler) stat(req *agentproto.Request) *agentproto.Response {
	local, err := h.localPath(req.Path)
	if err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	// Lstat: a symlink is reported as itself, never followed out of the workspace.
	info, err := os.Lstat(local)
	if err != nil {
		return fileErr(err)
	}
	out := &agentproto.FileInfo{IsDir: info.IsDir(), Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime().Unix()}
	if !info.IsDir() {
		out.Size = info.Size()
	}
	return &agentproto.Response{Info: out}
}

func (h *Handler) mkdir(req *agentproto.Request) *agentproto.Response {
	local, err := h.localPath(req.Path)
	if err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	if info, serr := os.Lstat(local); serr == nil && !info.IsDir() {
		return &agentproto.Response{Err: agentproto.ErrExists}
	}
	if err := os.MkdirAll(local, 0o755); err != nil {
		// MkdirAll reports a non-directory on the way as ENOTDIR.
		return fileErr(err)
	}
	return &agentproto.Response{}
}

func (h *Handler) move(req *agentproto.Request) *agentproto.Response {
	from, err := h.localPath(req.Path)
	if err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	to, err := h.localPath(req.To)
	if err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	if from == h.root || to == h.root {
		return &agentproto.Response{Err: "the workspace root cannot be moved or replaced"}
	}
	if _, err := os.Lstat(from); err != nil {
		return fileErr(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fileErr(err)
	}
	if !req.Overwrite {
		return responseOf(renameNoReplace(from, to))
	}
	// rename(2) replaces a file or an empty directory but not a directory with
	// content, so that case is cleared first.
	if info, serr := os.Lstat(to); serr == nil && info.IsDir() {
		if err := os.RemoveAll(to); err != nil {
			return fileErr(err)
		}
	}
	return responseOf(os.Rename(from, to))
}

func responseOf(err error) *agentproto.Response {
	if err != nil {
		return fileErr(err)
	}
	return &agentproto.Response{}
}
