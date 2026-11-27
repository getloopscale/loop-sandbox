package fake

import (
	"context"
	"path"
	"strings"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// fakeModTime is the modification time the fake reports for every path.
var fakeModTime = time.Unix(0, 0).UTC()

// isDirLocked reports whether p is a directory of the sandbox: one created with
// MakeDir, or the parent of a stored file.
func (d *Driver) isDirLocked(id runtime.SandboxID, p string) bool {
	if d.dirs[id][p] {
		return true
	}
	for name := range d.backing(id, p) {
		if strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}

func (d *Driver) existsLocked(id runtime.SandboxID, p string) bool {
	_, isFile := d.backing(id, p)[p]
	return isFile || d.isDirLocked(id, p)
}

// StatFile implements runtime.FileOpsDriver.
func (d *Driver) StatFile(_ context.Context, id runtime.SandboxID, p string) (*runtime.FileInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.backing(id, p)[p]; ok {
		return &runtime.FileInfo{Name: path.Base(p), Size: int64(len(b)), Mode: 0o644, ModTime: fakeModTime}, nil
	}
	if d.isDirLocked(id, p) {
		return &runtime.FileInfo{Name: path.Base(p), IsDir: true, Mode: 0o755, ModTime: fakeModTime}, nil
	}
	return nil, runtime.ErrFileNotFound
}

// MakeDir implements runtime.FileOpsDriver.
func (d *Driver) MakeDir(_ context.Context, id runtime.SandboxID, p string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, isFile := d.backing(id, p)[p]; isFile {
		return runtime.ErrFileExists
	}
	if d.dirs[id] == nil {
		d.dirs[id] = make(map[string]bool)
	}
	d.dirs[id][p] = true
	return nil
}

// MoveFile implements runtime.FileOpsDriver. It moves a file, or a directory with
// everything under it.
func (d *Driver) MoveFile(_ context.Context, id runtime.SandboxID, from, to string, overwrite bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.existsLocked(id, from) {
		return runtime.ErrFileNotFound
	}
	if d.existsLocked(id, to) && !overwrite {
		return runtime.ErrFileExists
	}
	files := d.backing(id, from)
	for name := range files {
		if name == to || strings.HasPrefix(name, to+"/") {
			delete(files, name) // replaced by the move
		}
	}
	moved := map[string][]byte{}
	for name, content := range files {
		if name == from || strings.HasPrefix(name, from+"/") {
			delete(files, name)
			moved[to+strings.TrimPrefix(name, from)] = content
		}
	}
	for name, content := range moved {
		files[name] = content
	}
	if d.dirs[id][from] {
		delete(d.dirs[id], from)
		d.dirs[id][to] = true
	}
	return nil
}

var _ runtime.FileOpsDriver = (*Driver)(nil)
