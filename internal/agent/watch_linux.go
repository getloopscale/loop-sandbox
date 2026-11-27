//go:build linux

package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// Limits of the directory watches of one sandbox.
const (
	// maxWatches bounds how many watches exist at once.
	maxWatches = 16
	// maxWatchDirs bounds the directories one recursive watch follows.
	maxWatchDirs = 4096
	// maxWatchEvents is how many events of one watch are kept for readers.
	maxWatchEvents = 4096
	// watchIdleLimit is how long a watch nobody reads is kept.
	watchIdleLimit = 10 * time.Minute
)

// watchMask is what a watch asks the kernel to report for each directory.
const watchMask = unix.IN_CREATE | unix.IN_MODIFY | unix.IN_DELETE | unix.IN_MOVED_FROM |
	unix.IN_MOVED_TO | unix.IN_ATTRIB | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF |
	unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW

// watch is one inotify instance over a directory, with its events kept as a log
// addressed by a cursor that counts events.
type watch struct {
	mu        sync.Mutex
	changed   *sync.Cond
	file      *os.File // the inotify descriptor, read through the runtime poller
	fd        int      // the same descriptor; File.Fd would switch it to blocking mode
	root      string   // local path of the watched directory
	rootWD    int
	recursive bool
	logical   func(local string) string
	dirs      map[int]string // watch descriptor to local directory
	events    []agentproto.FileEvent
	base      int64 // cursor of the first event still kept
	total     int64
	delivered int64 // highest cursor a reader was given
	closed    bool
	lastRead  time.Time
}

type watches struct {
	mu sync.Mutex
	m  map[string]*watch
}

func newWatches() *watches { return &watches{m: map[string]*watch{}} }

func (t *watches) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, w := range t.m {
		w.close()
		delete(t.m, id)
	}
}

func (h *Handler) watchOp(req *agentproto.Request) *agentproto.Response {
	t := h.watches
	if req.Op == agentproto.OpWatchStart {
		return h.startWatch(req)
	}
	t.mu.Lock()
	w := t.m[req.WatchID]
	if w != nil && req.Op == agentproto.OpWatchStop {
		delete(t.m, req.WatchID)
	}
	t.mu.Unlock()
	if w == nil {
		return &agentproto.Response{Err: agentproto.ErrNotFound}
	}
	if req.Op == agentproto.OpWatchStop {
		w.close()
		return &agentproto.Response{}
	}
	return w.eventsAfter(max(req.After, 0), min(time.Duration(req.WaitMS)*time.Millisecond, maxOutputWait))
}

func (h *Handler) startWatch(req *agentproto.Request) *agentproto.Response {
	local, err := h.localPath(req.Path)
	if err != nil {
		return &agentproto.Response{Err: agentproto.ErrInvalidPath}
	}
	info, err := os.Lstat(local)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &agentproto.Response{Err: agentproto.ErrNotFound}
	case err != nil:
		return &agentproto.Response{Err: "watch: " + err.Error()}
	case !info.IsDir():
		return &agentproto.Response{Err: agentproto.ErrNotDirectory}
	}

	t := h.watches
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for id, w := range t.m {
		if w.idleSince(now) > watchIdleLimit {
			w.close()
			delete(t.m, id)
		}
	}
	if len(t.m) >= maxWatches {
		return &agentproto.Response{Err: agentproto.ErrTooManyWatches}
	}

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return &agentproto.Response{Err: "watch: inotify: " + err.Error()}
	}
	root := h.root
	w := &watch{
		file: os.NewFile(uintptr(fd), "inotify"), fd: fd, root: local, recursive: req.Recursive,
		dirs: map[int]string{}, lastRead: now,
		logical: func(p string) string {
			return workspaceLogical + filepath.ToSlash(strings.TrimPrefix(p, root))
		},
	}
	w.changed = sync.NewCond(&w.mu)
	w.mu.Lock()
	err = w.addTree(local, false)
	w.mu.Unlock()
	if err != nil {
		w.close()
		return &agentproto.Response{Err: "watch: " + err.Error()}
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		w.close()
		return &agentproto.Response{Err: "watch: id: " + err.Error()}
	}
	id := "wch_" + hex.EncodeToString(raw[:])
	t.m[id] = w
	go w.run()
	return &agentproto.Response{WatchID: id}
}

func (w *watch) idleSince(now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return now.Sub(w.lastRead)
}

// addTree watches dir and, in a recursive watch, the directories under it. With
// announce set it also records a create event for everything found under dir:
// that is for a directory that appeared after the watch began, whose content may
// have been written before the kernel was asked to report it. w.mu is held.
func (w *watch) addTree(dir string, announce bool) error {
	if len(w.dirs) >= maxWatchDirs {
		return errors.New("too many directories to watch")
	}
	wd, err := unix.InotifyAddWatch(w.fd, dir, watchMask)
	if err != nil {
		return err
	}
	w.dirs[wd] = dir
	if dir == w.root {
		w.rootWD = wd
	}
	if !w.recursive && !announce {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		child := filepath.Join(dir, e.Name())
		if announce {
			w.record(agentproto.EventCreate, child, e.IsDir())
		}
		if w.recursive && e.IsDir() { // ReadDir does not follow links, so a link to a directory is not entered
			if err := w.addTree(child, announce); err != nil {
				return err
			}
		}
	}
	return nil
}

// record appends an event. w.mu is held.
func (w *watch) record(kind, local string, isDir bool) {
	ev := agentproto.FileEvent{Type: kind, IsDir: isDir}
	if local != "" {
		ev.Path = w.logical(local)
	}
	// A file being written produces a burst of identical events; one that no
	// reader has seen yet already says it.
	if n := len(w.events); n > 0 && kind == agentproto.EventWrite && w.events[n-1] == ev && w.total > w.delivered {
		return
	}
	w.events = append(w.events, ev)
	w.total++
	if len(w.events) > maxWatchEvents {
		drop := len(w.events) - maxWatchEvents
		w.events = w.events[drop:]
		w.base += int64(drop)
	}
	w.changed.Broadcast()
}

// run reads the kernel's events until the watch is closed.
func (w *watch) run() {
	buf := make([]byte, 64<<10)
	for {
		n, err := w.file.Read(buf)
		if err != nil || n < unix.SizeofInotifyEvent {
			w.mu.Lock()
			w.closed = true
			w.changed.Broadcast()
			w.mu.Unlock()
			return
		}
		w.mu.Lock()
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off])) //nolint:gosec // the kernel's event layout
			nameLen := int(raw.Len)
			name := strings.TrimRight(string(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+nameLen]), "\x00")
			off += unix.SizeofInotifyEvent + nameLen
			w.handle(int(raw.Wd), raw.Mask, name)
		}
		ended := w.closed
		w.mu.Unlock()
		if ended {
			_ = w.file.Close()
			return
		}
	}
}

// handle turns one kernel event into recorded events. w.mu is held.
func (w *watch) handle(wd int, mask uint32, name string) {
	if mask&unix.IN_Q_OVERFLOW != 0 {
		w.record(agentproto.EventOverflow, "", false)
		return
	}
	dir, known := w.dirs[wd]
	if mask&unix.IN_IGNORED != 0 {
		delete(w.dirs, wd)
		return
	}
	if !known {
		return
	}
	if mask&(unix.IN_DELETE_SELF|unix.IN_MOVE_SELF) != 0 {
		if wd == w.rootWD {
			// The watched directory itself is gone: nothing more can be reported.
			w.record(agentproto.EventRemove, w.root, true)
			w.closed = true
			w.changed.Broadcast()
		}
		return
	}
	if name == "" {
		return // a change to the watched directory itself, such as its own mode
	}
	path := filepath.Join(dir, name)
	isDir := mask&unix.IN_ISDIR != 0
	switch {
	case mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0:
		w.record(agentproto.EventCreate, path, isDir)
		if isDir && w.recursive {
			if err := w.addTree(path, true); err != nil {
				// The directory vanished already, or the watch is at its limit: the
				// client is told that what it sees may be incomplete.
				w.record(agentproto.EventOverflow, "", false)
			}
		}
	case mask&unix.IN_MOVED_FROM != 0:
		w.record(agentproto.EventRename, path, isDir)
		if isDir {
			w.forget(path)
		}
	case mask&unix.IN_DELETE != 0:
		w.record(agentproto.EventRemove, path, isDir)
	case mask&unix.IN_MODIFY != 0:
		w.record(agentproto.EventWrite, path, isDir)
	case mask&unix.IN_ATTRIB != 0:
		w.record(agentproto.EventChmod, path, isDir)
	}
}

// forget stops watching a directory that was moved away, and the ones under it:
// their descriptors would keep reporting under the old path. w.mu is held.
func (w *watch) forget(moved string) {
	for wd, dir := range w.dirs {
		if dir == moved || strings.HasPrefix(dir, moved+string(filepath.Separator)) {
			_, _ = unix.InotifyRmWatch(w.fd, uint32(wd)) //nolint:gosec // a descriptor the kernel gave us
			delete(w.dirs, wd)
		}
	}
}

func (w *watch) eventsAfter(after int64, wait time.Duration) *agentproto.Response {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastRead = time.Now()
	if wait > 0 && w.total <= after && !w.closed {
		deadline := time.Now().Add(wait)
		timer := time.AfterFunc(wait, func() {
			w.mu.Lock()
			w.changed.Broadcast()
			w.mu.Unlock()
		})
		defer timer.Stop()
		for w.total <= after && !w.closed && time.Now().Before(deadline) {
			w.changed.Wait()
		}
		w.lastRead = time.Now()
	}
	resp := &agentproto.Response{Next: max(after, w.total), Closed: w.closed}
	if after < w.base {
		resp.Truncated = true
		after = w.base
	}
	if after < w.total {
		resp.Events = append([]agentproto.FileEvent(nil), w.events[after-w.base:]...)
	}
	w.delivered = max(w.delivered, resp.Next)
	return resp
}

// close ends the watch. The reader goroutine sees its read fail and returns.
func (w *watch) close() {
	w.mu.Lock()
	already := w.closed
	w.closed = true
	w.changed.Broadcast()
	w.mu.Unlock()
	if !already {
		_ = w.file.Close()
	}
}
