package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// recordingMounts replaces the mount syscalls of a handler and records the calls.
type recordingMounts struct {
	mounted   []string
	unmounted []string
}

func handlerWithRecordedMounts(t *testing.T) (*Handler, *recordingMounts, string) {
	t.Helper()
	root := t.TempDir()
	rec := &recordingMounts{}
	h := New(root)
	h.mount = func(device, dir string) error {
		rec.mounted = append(rec.mounted, device+" "+dir)
		return nil
	}
	h.unmount = func(dir string) error {
		rec.unmounted = append(rec.unmounted, dir)
		return nil
	}
	return h, rec, root
}

func TestVolumeMountCreatesTheMountPointUnderTheWorkspace(t *testing.T) {
	h, rec, root := handlerWithRecordedMounts(t)

	resp := h.handle(&agentproto.Request{Op: agentproto.OpVolumeMount, Volume: "data"})

	if resp.Err != "" {
		t.Fatalf("mount: %s", resp.Err)
	}
	want := VolumeDevice + " " + filepath.Join(root, "data")
	if len(rec.mounted) != 1 || rec.mounted[0] != want {
		t.Errorf("mount calls = %v, want [%s]", rec.mounted, want)
	}
	if info, err := os.Stat(filepath.Join(root, "data")); err != nil || !info.IsDir() {
		t.Errorf("mount point not created: %v", err)
	}
}

func TestVolumeMountRejectsANameThatCouldLeaveTheWorkspace(t *testing.T) {
	for _, name := range []string{"", "..", "../etc", "a/b", "/etc", ".hidden", "UPPER"} {
		t.Run(name, func(t *testing.T) {
			h, rec, _ := handlerWithRecordedMounts(t)

			resp := h.handle(&agentproto.Request{Op: agentproto.OpVolumeMount, Volume: name})

			if resp.Err == "" || len(rec.mounted) != 0 {
				t.Errorf("name %q: err=%q, mount calls=%v; want an error and no mount", name, resp.Err, rec.mounted)
			}
		})
	}
}

func TestVolumeMountRefusesASymlinkAsMountPoint(t *testing.T) {
	h, rec, root := handlerWithRecordedMounts(t)
	if err := os.Symlink("/etc", filepath.Join(root, "data")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	resp := h.handle(&agentproto.Request{Op: agentproto.OpVolumeMount, Volume: "data"})

	if !strings.Contains(resp.Err, "not a directory") || len(rec.mounted) != 0 {
		t.Errorf("err=%q, mount calls=%v; want a refusal and no mount", resp.Err, rec.mounted)
	}
}

func TestVolumeMountRefusesADirectoryThatHoldsFiles(t *testing.T) {
	h, rec, root := handlerWithRecordedMounts(t)
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp := h.handle(&agentproto.Request{Op: agentproto.OpVolumeMount, Volume: "data"})

	if !strings.Contains(resp.Err, "not empty") || len(rec.mounted) != 0 {
		t.Errorf("err=%q, mount calls=%v; want a refusal and no mount", resp.Err, rec.mounted)
	}
}

func TestVolumeUnmountRemovesTheMountPoint(t *testing.T) {
	h, rec, root := handlerWithRecordedMounts(t)
	if resp := h.handle(&agentproto.Request{Op: agentproto.OpVolumeMount, Volume: "data"}); resp.Err != "" {
		t.Fatalf("mount: %s", resp.Err)
	}

	resp := h.handle(&agentproto.Request{Op: agentproto.OpVolumeUnmount, Volume: "data"})

	if resp.Err != "" {
		t.Fatalf("unmount: %s", resp.Err)
	}
	if len(rec.unmounted) != 1 || rec.unmounted[0] != filepath.Join(root, "data") {
		t.Errorf("unmount calls = %v", rec.unmounted)
	}
	if _, err := os.Lstat(filepath.Join(root, "data")); !os.IsNotExist(err) {
		t.Errorf("mount point still exists after unmount: %v", err)
	}
}
