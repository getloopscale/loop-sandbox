//go:build linux

package agent_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

func startWatch(t *testing.T, conn net.Conn, path string, recursive bool) string {
	t.Helper()
	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWatchStart, Path: path, Recursive: recursive})
	if resp.Err != "" || resp.WatchID == "" {
		t.Fatalf("start watch on %s: %+v", path, resp)
	}
	return resp.WatchID
}

// eventsUntil reads events from cursor 0 until want is among them.
func eventsUntil(t *testing.T, conn net.Conn, id string, want agentproto.FileEvent) []agentproto.FileEvent {
	t.Helper()
	var all []agentproto.FileEvent
	var after int64
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWatchEvents, WatchID: id, After: after, WaitMS: 1000})
		if resp.Err != "" {
			t.Fatalf("events: %q", resp.Err)
		}
		all = append(all, resp.Events...)
		after = resp.Next
		for _, ev := range all {
			if ev == want {
				return all
			}
		}
	}
	t.Fatalf("event %+v never arrived; got %+v", want, all)
	return nil
}

func has(events []agentproto.FileEvent, want agentproto.FileEvent) bool {
	for _, ev := range events {
		if ev == want {
			return true
		}
	}
	return false
}

func TestWatchReportsAFileCreatedInTheDirectory(t *testing.T) {
	root := t.TempDir()
	conn := serveConn(t, root)
	id := startWatch(t, conn, "/workspace", false)

	mustWrite(t, filepath.Join(root, "a.txt"), "x")

	eventsUntil(t, conn, id, agentproto.FileEvent{Type: agentproto.EventCreate, Path: "/workspace/a.txt"})
}

func TestWatchReportsWriteRemoveRenameAndChmod(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "x")
	mustWrite(t, filepath.Join(root, "b.txt"), "x")
	conn := serveConn(t, root)
	id := startWatch(t, conn, "/workspace", false)

	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("more"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "a.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "a.txt"), filepath.Join(root, "c.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}

	events := eventsUntil(t, conn, id, agentproto.FileEvent{Type: agentproto.EventRemove, Path: "/workspace/b.txt"})
	for _, want := range []agentproto.FileEvent{
		{Type: agentproto.EventWrite, Path: "/workspace/a.txt"},
		{Type: agentproto.EventChmod, Path: "/workspace/a.txt"},
		{Type: agentproto.EventRename, Path: "/workspace/a.txt"},
		{Type: agentproto.EventCreate, Path: "/workspace/c.txt"},
	} {
		if !has(events, want) {
			t.Errorf("missing %+v in %+v", want, events)
		}
	}
}

func TestWatchWithoutRecursiveIgnoresSubdirectories(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "sub", "seed.txt"), "x")
	conn := serveConn(t, root)
	id := startWatch(t, conn, "/workspace", false)

	mustWrite(t, filepath.Join(root, "sub", "deep.txt"), "x")
	mustWrite(t, filepath.Join(root, "marker.txt"), "x")

	events := eventsUntil(t, conn, id, agentproto.FileEvent{Type: agentproto.EventCreate, Path: "/workspace/marker.txt"})
	if has(events, agentproto.FileEvent{Type: agentproto.EventCreate, Path: "/workspace/sub/deep.txt"}) {
		t.Errorf("a non-recursive watch reported a file in a subdirectory: %+v", events)
	}
}

func TestRecursiveWatchReportsFilesInExistingAndNewSubdirectories(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "old", "seed.txt"), "x")
	conn := serveConn(t, root)
	id := startWatch(t, conn, "/workspace", true)

	mustWrite(t, filepath.Join(root, "old", "a.txt"), "x")
	// The directory and its file are created back to back: the file may exist
	// before the kernel is asked to watch the new directory.
	mustWrite(t, filepath.Join(root, "new", "deep", "b.txt"), "x")

	events := eventsUntil(t, conn, id, agentproto.FileEvent{Type: agentproto.EventCreate, Path: "/workspace/new/deep/b.txt"})
	for _, want := range []agentproto.FileEvent{
		{Type: agentproto.EventCreate, Path: "/workspace/old/a.txt"},
		{Type: agentproto.EventCreate, Path: "/workspace/new", IsDir: true},
		{Type: agentproto.EventCreate, Path: "/workspace/new/deep", IsDir: true},
	} {
		if !has(events, want) {
			t.Errorf("missing %+v in %+v", want, events)
		}
	}
}

func TestWatchEventsResumeFromACursor(t *testing.T) {
	root := t.TempDir()
	conn := serveConn(t, root)
	id := startWatch(t, conn, "/workspace", false)
	mustWrite(t, filepath.Join(root, "a.txt"), "")
	eventsUntil(t, conn, id, agentproto.FileEvent{Type: agentproto.EventCreate, Path: "/workspace/a.txt"})
	seen := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWatchEvents, WatchID: id}).Next

	mustWrite(t, filepath.Join(root, "b.txt"), "")
	var later []agentproto.FileEvent
	for deadline := time.Now().Add(10 * time.Second); len(later) == 0 && time.Now().Before(deadline); {
		later = roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWatchEvents, WatchID: id, After: seen, WaitMS: 1000}).Events
	}

	if len(later) == 0 || later[0].Path != "/workspace/b.txt" {
		t.Errorf("events after cursor %d = %+v, want them to start at b.txt", seen, later)
	}
}

func TestWatchEventsWithNothingNewWaitAndReturnEmpty(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	id := startWatch(t, conn, "/workspace", false)
	begin := time.Now()

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWatchEvents, WatchID: id, WaitMS: 300})

	if took := time.Since(begin); took < 250*time.Millisecond || took > 5*time.Second {
		t.Errorf("the request took %s, want about 300ms", took)
	}
	if len(resp.Events) != 0 || resp.Closed || resp.Next != 0 {
		t.Errorf("resp = %+v, want no events on an open watch", resp)
	}
}

func TestWatchIsClosedWhenItsDirectoryIsRemoved(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "dir", "a.txt"), "x")
	conn := serveConn(t, root)
	id := startWatch(t, conn, "/workspace/dir", false)

	if err := os.RemoveAll(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}

	eventsUntil(t, conn, id, agentproto.FileEvent{Type: agentproto.EventRemove, Path: "/workspace/dir", IsDir: true})
	if resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWatchEvents, WatchID: id}); !resp.Closed {
		t.Errorf("resp = %+v, want a closed watch", resp)
	}
}

func TestWatchStopEndsTheWatch(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	id := startWatch(t, conn, "/workspace", false)

	stopped := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWatchStop, WatchID: id})
	after := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWatchEvents, WatchID: id})

	if stopped.Err != "" || after.Err != agentproto.ErrNotFound {
		t.Errorf("stop err=%q, events after stop err=%q; want none and not_found", stopped.Err, after.Err)
	}
}

func TestWatchStartRefusesWhatItCannotWatch(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "x")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ path, want string }{
		"a missing path":         {"/workspace/nope", agentproto.ErrNotFound},
		"a file":                 {"/workspace/a.txt", agentproto.ErrNotDirectory},
		"a path outside":         {"/etc", agentproto.ErrInvalidPath},
		"a link out of the root": {"/workspace/escape", agentproto.ErrInvalidPath},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			conn := serveConn(t, root)

			resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWatchStart, Path: c.path})

			if resp.Err != c.want {
				t.Errorf("Err = %q, want %q", resp.Err, c.want)
			}
		})
	}
}

func TestRecursiveWatchDoesNotFollowALinkOutOfTheWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	conn := serveConn(t, root)
	id := startWatch(t, conn, "/workspace", true)

	mustWrite(t, filepath.Join(outside, "secret.txt"), "x")
	mustWrite(t, filepath.Join(root, "marker.txt"), "x")

	events := eventsUntil(t, conn, id, agentproto.FileEvent{Type: agentproto.EventCreate, Path: "/workspace/marker.txt"})
	for _, ev := range events {
		if ev.Path != "/workspace/marker.txt" {
			t.Errorf("unexpected event %+v: the watch saw outside the workspace", ev)
		}
	}
}

func TestWatchStartIsRefusedBeyondTheLimit(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	for range 16 {
		startWatch(t, conn, "/workspace", false)
	}

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWatchStart, Path: "/workspace"})

	if resp.Err != agentproto.ErrTooManyWatches {
		t.Errorf("Err = %q, want too_many_watches", resp.Err)
	}
}
