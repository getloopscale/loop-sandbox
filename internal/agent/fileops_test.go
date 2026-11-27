package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func content(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestStatDescribesAFile(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "hello")
	conn := serveConn(t, root)

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpStat, Path: "/workspace/a.txt"})

	if resp.Err != "" || resp.Info == nil {
		t.Fatalf("resp = %+v, want file info", resp)
	}
	if resp.Info.Size != 5 || resp.Info.IsDir || resp.Info.Mode != 0o640 || resp.Info.ModTime == 0 {
		t.Errorf("info = %+v, want size 5, not a dir, mode 640 and a modification time", resp.Info)
	}
}

func TestStatDescribesADirectoryWithSizeZero(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "dir", "a.txt"), "x")
	conn := serveConn(t, root)

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpStat, Path: "/workspace/dir"})

	if resp.Info == nil || !resp.Info.IsDir || resp.Info.Size != 0 {
		t.Errorf("resp = %+v info = %+v, want a directory of size 0", resp, resp.Info)
	}
}

func TestStatOfAMissingPathIsNotFound(t *testing.T) {
	conn := serveConn(t, t.TempDir())

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpStat, Path: "/workspace/nope"})

	if resp.Err != agentproto.ErrNotFound {
		t.Errorf("Err = %q, want not_found", resp.Err)
	}
}

func TestMkdirCreatesParentsAndIsRepeatable(t *testing.T) {
	root := t.TempDir()
	conn := serveConn(t, root)
	req := &agentproto.Request{Op: agentproto.OpMkdir, Path: "/workspace/a/b/c"}

	first := roundTrip(t, conn, req)
	second := roundTrip(t, conn, req)

	if first.Err != "" || second.Err != "" {
		t.Fatalf("mkdir errors: %q then %q, want none", first.Err, second.Err)
	}
	if info, err := os.Stat(filepath.Join(root, "a", "b", "c")); err != nil || !info.IsDir() {
		t.Errorf("a/b/c is not a directory: %v", err)
	}
}

func TestMkdirOverAFileIsExists(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "keep")
	conn := serveConn(t, root)

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpMkdir, Path: "/workspace/a.txt"})

	if resp.Err != agentproto.ErrExists {
		t.Errorf("Err = %q, want exists", resp.Err)
	}
	if got := content(t, filepath.Join(root, "a.txt")); got != "keep" {
		t.Errorf("the file was changed to %q", got)
	}
}

func TestMoveRenamesAFileIntoANewDirectory(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "hello")
	conn := serveConn(t, root)

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpMove, Path: "/workspace/a.txt", To: "/workspace/new/dir/b.txt"})

	if resp.Err != "" {
		t.Fatalf("move: %q", resp.Err)
	}
	if got := content(t, filepath.Join(root, "new", "dir", "b.txt")); got != "hello" {
		t.Errorf("destination = %q, want hello", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("the source still exists: %v", err)
	}
}

func TestMoveOntoAnExistingPathIsExistsAndChangesNothing(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "source")
	mustWrite(t, filepath.Join(root, "b.txt"), "destination")
	conn := serveConn(t, root)

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpMove, Path: "/workspace/a.txt", To: "/workspace/b.txt"})

	if resp.Err != agentproto.ErrExists {
		t.Errorf("Err = %q, want exists", resp.Err)
	}
	if a, b := content(t, filepath.Join(root, "a.txt")), content(t, filepath.Join(root, "b.txt")); a != "source" || b != "destination" {
		t.Errorf("after a refused move: a=%q b=%q", a, b)
	}
}

func TestMoveWithOverwriteReplacesAFile(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "source")
	mustWrite(t, filepath.Join(root, "b.txt"), "destination")
	conn := serveConn(t, root)

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpMove, Path: "/workspace/a.txt", To: "/workspace/b.txt", Overwrite: true})

	if resp.Err != "" {
		t.Fatalf("move: %q", resp.Err)
	}
	if got := content(t, filepath.Join(root, "b.txt")); got != "source" {
		t.Errorf("destination = %q, want source", got)
	}
}

func TestMoveWithOverwriteReplacesADirectoryWithContent(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "src", "new.txt"), "new")
	mustWrite(t, filepath.Join(root, "dst", "old.txt"), "old")
	conn := serveConn(t, root)

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpMove, Path: "/workspace/src", To: "/workspace/dst", Overwrite: true})

	if resp.Err != "" {
		t.Fatalf("move: %q", resp.Err)
	}
	if got := content(t, filepath.Join(root, "dst", "new.txt")); got != "new" {
		t.Errorf("dst/new.txt = %q, want new", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "dst", "old.txt")); !os.IsNotExist(err) {
		t.Errorf("dst/old.txt survived the overwrite: %v", err)
	}
}

func TestMoveOfAMissingSourceIsNotFound(t *testing.T) {
	conn := serveConn(t, t.TempDir())

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpMove, Path: "/workspace/nope", To: "/workspace/b"})

	if resp.Err != agentproto.ErrNotFound {
		t.Errorf("Err = %q, want not_found", resp.Err)
	}
}

func TestFileOpsRefuseAPathOutsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "x")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cases := map[string]*agentproto.Request{
		"stat outside":             {Op: agentproto.OpStat, Path: "/etc/passwd"},
		"mkdir outside":            {Op: agentproto.OpMkdir, Path: "/tmp/evil"},
		"mkdir through a symlink":  {Op: agentproto.OpMkdir, Path: "/workspace/escape/evil"},
		"move to outside":          {Op: agentproto.OpMove, Path: "/workspace/a.txt", To: "/etc/evil"},
		"move through a symlink":   {Op: agentproto.OpMove, Path: "/workspace/a.txt", To: "/workspace/escape/a.txt"},
		"move from outside":        {Op: agentproto.OpMove, Path: "/etc/passwd", To: "/workspace/passwd"},
		"move the workspace root":  {Op: agentproto.OpMove, Path: "/workspace", To: "/workspace/sub"},
		"move with dot-dot escape": {Op: agentproto.OpMove, Path: "/workspace/a.txt", To: "/workspace/../evil"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			conn := serveConn(t, root)

			resp := roundTrip(t, conn, req)

			if resp.Err == "" {
				t.Errorf("request %+v was accepted", req)
			}
		})
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Errorf("something was created outside the workspace: %v %v", entries, err)
	}
	if got := content(t, filepath.Join(root, "a.txt")); got != "x" {
		t.Errorf("a.txt was changed to %q", got)
	}
}
