package agent_test

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

type streamFrame struct {
	resp *agentproto.Response
	at   time.Time
}

// streamExec sends an exec_stream request and reads frames up to the final one.
func streamExec(t *testing.T, conn net.Conn, req *agentproto.Request) []streamFrame {
	t.Helper()
	req.Op = agentproto.OpExecStream
	if err := agentproto.WriteFrame(conn, req); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var frames []streamFrame
	for {
		resp, err := agentproto.ReadResponse(conn)
		if err != nil {
			t.Fatalf("ReadResponse after %d frames: %v", len(frames), err)
		}
		frames = append(frames, streamFrame{resp, time.Now()})
		if resp.Stream == "" {
			return frames
		}
	}
}

func output(frames []streamFrame, stream string) string {
	var b strings.Builder
	for _, f := range frames {
		if f.resp.Stream == stream {
			b.Write(f.resp.Data)
		}
	}
	return b.String()
}

func TestExecStreamSendsEachStreamAndEndsWithTheExitCode(t *testing.T) {
	conn := serveConn(t, t.TempDir())

	frames := streamExec(t, conn, &agentproto.Request{Cmd: []string{"sh", "-c", "echo out; echo err >&2; exit 4"}})

	last := frames[len(frames)-1].resp
	if last.ExitCode != 4 || last.Err != "" {
		t.Errorf("final frame = %+v, want exit 4 and no error", last)
	}
	if output(frames, "stdout") != "out\n" || output(frames, "stderr") != "err\n" {
		t.Errorf("stdout=%q stderr=%q, want out and err", output(frames, "stdout"), output(frames, "stderr"))
	}
}

func TestExecStreamSendsOutputBeforeTheCommandEnds(t *testing.T) {
	conn := serveConn(t, t.TempDir())

	frames := streamExec(t, conn, &agentproto.Request{Cmd: []string{"sh", "-c", "echo first; sleep 1; echo second"}})

	first, last := frames[0], frames[len(frames)-1]
	if first.resp.Stream != "stdout" || !strings.Contains(string(first.resp.Data), "first") {
		t.Fatalf("first frame = %+v, want stdout with first", first.resp)
	}
	if gap := last.at.Sub(first.at); gap < 500*time.Millisecond {
		t.Errorf("first output arrived %v before the end, want at least 500ms", gap)
	}
}

func TestExecStreamPassesStdinEnvAndWorkDir(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	conn := serveConn(t, root)

	frames := streamExec(t, conn, &agentproto.Request{
		Cmd:      []string{"sh", "-c", `printf '%s|%s|' "$GREETING" "$(basename "$PWD")"; cat`},
		Env:      map[string]string{"GREETING": "hi"},
		WorkDir:  "/workspace/sub",
		StdinB64: []byte("from stdin"),
	})

	if got := output(frames, "stdout"); got != "hi|sub|from stdin" {
		t.Errorf("stdout = %q, want hi|sub|from stdin", got)
	}
}

func TestExecStreamOfAnEmptyCommandIsAnError(t *testing.T) {
	conn := serveConn(t, t.TempDir())

	frames := streamExec(t, conn, &agentproto.Request{})

	if len(frames) != 1 || frames[0].resp.Err == "" {
		t.Errorf("frames = %+v, want one final frame with an error", frames)
	}
}

func TestExecStreamOfAMissingBinaryIsAnError(t *testing.T) {
	conn := serveConn(t, t.TempDir())

	frames := streamExec(t, conn, &agentproto.Request{Cmd: []string{"/no/such/binary"}})

	if last := frames[len(frames)-1].resp; !strings.HasPrefix(last.Err, "exec: ") {
		t.Errorf("final frame = %+v, want an exec error", last)
	}
}

func TestExecStreamRejectsAWorkDirOutsideTheWorkspace(t *testing.T) {
	conn := serveConn(t, t.TempDir())

	frames := streamExec(t, conn, &agentproto.Request{Cmd: []string{"pwd"}, WorkDir: "/etc"})

	if len(frames) != 1 || frames[0].resp.Err == "" {
		t.Errorf("frames = %+v, want one final frame with an error", frames)
	}
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestExecStreamKillsTheCommandAndItsChildrenWhenTheHostHangsUp(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	// The shell starts a child and prints both pids; the child would outlive a kill
	// aimed at the shell alone.
	req := &agentproto.Request{Op: agentproto.OpExecStream, Cmd: []string{"sh", "-c", "sleep 60 & echo $$ $!; wait"}}
	if err := agentproto.WriteFrame(conn, req); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	resp, err := agentproto.ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	fields := strings.Fields(string(resp.Data))
	if len(fields) != 2 {
		t.Fatalf("first frame = %q, want two pids", resp.Data)
	}
	shell, _ := strconv.Atoi(fields[0])
	child, _ := strconv.Atoi(fields[1])
	if !alive(shell) || !alive(child) {
		t.Fatalf("processes %d and %d should be running before the hang-up", shell, child)
	}

	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for alive(shell) || alive(child) {
		if time.Now().After(deadline) {
			t.Fatalf("after the hang-up: shell alive=%v, child alive=%v; want both killed", alive(shell), alive(child))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
