package agent_test

import (
	"net"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/agent"
	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// serveConn starts a handler over one end of a net.Pipe and returns the client end.
func serveConn(t *testing.T, root string) net.Conn {
	t.Helper()
	server, client := net.Pipe()
	h := agent.New(root)
	go func() { _ = h.Serve(server) }()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client
}

func roundTrip(t *testing.T, conn net.Conn, req *agentproto.Request) *agentproto.Response {
	t.Helper()
	if err := agentproto.WriteFrame(conn, req); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	resp, err := agentproto.ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	return resp
}

func TestAgent_exec_echo(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpExec, Cmd: []string{"echo", "-n", "hello"}})
	if resp.ExitCode != 0 || string(resp.Stdout) != "hello" {
		t.Errorf("exec echo = exit %d stdout %q, want 0 'hello'", resp.ExitCode, resp.Stdout)
	}
}

func TestAgent_exec_nonzero_exit_is_reported_not_error(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpExec, Cmd: []string{"false"}})
	if resp.Err != "" {
		t.Errorf("Err = %q, want empty (non-zero exit is not an agent error)", resp.Err)
	}
	if resp.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want non-zero")
	}
}

func TestAgent_write_read_delete_list(t *testing.T) {
	conn := serveConn(t, t.TempDir())

	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWrite, Path: "/workspace/a.txt", Data: []byte("data")}); r.Err != "" {
		t.Fatalf("write: %s", r.Err)
	}
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpRead, Path: "/workspace/a.txt"}); r.Err != "" || string(r.Data) != "data" {
		t.Fatalf("read = %q (err %q), want 'data'", r.Data, r.Err)
	}
	// NOTE: exec `cat /workspace/a.txt` seeing the written file is proven in the
	// real-VM integration test (T4) where root IS /workspace; here root is a temp
	// dir so a real subprocess cannot resolve the logical path.
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpList, Path: "/workspace"}); len(r.Entries) != 1 || r.Entries[0].Name != "a.txt" {
		t.Errorf("list = %+v, want [a.txt]", r.Entries)
	}
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpDelete, Path: "/workspace/a.txt"}); r.Err != "" {
		t.Fatalf("delete: %s", r.Err)
	}
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpRead, Path: "/workspace/a.txt"}); r.Err != "not_found" {
		t.Errorf("read after delete err = %q, want not_found", r.Err)
	}
}

func TestAgent_rejects_path_traversal(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	for _, p := range []string{"/etc/passwd", "/workspace/../etc/passwd", "/workspace/../../x"} {
		r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWrite, Path: p, Data: []byte("x")})
		if r.Err == "" {
			t.Errorf("write %q: expected rejection, got success", p)
		}
	}
}

func TestAgent_ping(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpPing}); r.Err != "" {
		t.Errorf("ping err = %q, want empty", r.Err)
	}
}

func TestAgent_exec_empty_cmd(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpExec, Cmd: nil})
	if r.ExitCode != 127 || string(r.Stderr) != "empty command" {
		t.Errorf("empty cmd = exit %d stderr %q, want 127 'empty command'", r.ExitCode, r.Stderr)
	}
}

func TestAgent_exec_stdin_passthrough(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpExec, Cmd: []string{"cat"}, StdinB64: []byte("piped-in")})
	if r.ExitCode != 0 || string(r.Stdout) != "piped-in" {
		t.Errorf("cat stdin = exit %d out %q, want 0 'piped-in'", r.ExitCode, r.Stdout)
	}
}

func TestAgent_exec_env_passthrough(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpExec, Cmd: []string{"sh", "-c", "printf %s \"$LSB_A\""}, Env: map[string]string{"LSB_A": "envval"}})
	if string(r.Stdout) != "envval" {
		t.Errorf("env passthrough = %q, want 'envval'", r.Stdout)
	}
}

func TestAgent_exec_workdir_confined_and_invalid_rejected(t *testing.T) {
	root := t.TempDir()
	conn := serveConn(t, root)
	// valid workdir: write a file with a RELATIVE path → lands under root/workspace.
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpExec, WorkDir: "/workspace", Cmd: []string{"sh", "-c", "printf hi > rel.txt"}}); r.Err != "" {
		t.Fatalf("exec in workdir: %s", r.Err)
	}
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpRead, Path: "/workspace/rel.txt"}); string(r.Data) != "hi" {
		t.Errorf("relative write via workdir = %q, want 'hi'", r.Data)
	}
	// invalid workdir outside /workspace → rejected.
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpExec, WorkDir: "/etc", Cmd: []string{"pwd"}}); r.Err == "" {
		t.Errorf("workdir /etc: expected rejection")
	}
}

func TestAgent_write_nested_dir(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpWrite, Path: "/workspace/a/b/c.txt", Data: []byte("deep")}); r.Err != "" {
		t.Fatalf("nested write: %s", r.Err)
	}
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpRead, Path: "/workspace/a/b/c.txt"}); string(r.Data) != "deep" {
		t.Errorf("nested read = %q, want 'deep'", r.Data)
	}
}

func TestAgent_list_missing_dir_is_not_found(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpList, Path: "/workspace/nope"}); r.Err != "not_found" {
		t.Errorf("list missing = %q, want not_found", r.Err)
	}
}

func TestAgent_delete_missing_is_idempotent(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpDelete, Path: "/workspace/nope"}); r.Err != "" {
		t.Errorf("delete missing = %q, want no error (idempotent)", r.Err)
	}
}

func TestAgent_unknown_op(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.Op("bogus")}); r.Err == "" {
		t.Errorf("unknown op: expected error")
	}
}

func TestAgent_rejects_symlink_escape(t *testing.T) {
	root := t.TempDir()
	conn := serveConn(t, root)
	// user code plants a symlink /workspace/evil -> /etc, then tries to read through it.
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpExec, Cmd: []string{"ln", "-s", "/etc", root + "/evil"}}); r.Err != "" {
		t.Fatalf("ln -s setup: %s", r.Err)
	}
	if r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpRead, Path: "/workspace/evil/hostname"}); r.Err == "" {
		t.Errorf("symlink escape read: expected rejection, got data %q", r.Data)
	}
}

func TestAgent_exec_output_truncated_at_cap(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	// 9 MiB of output exceeds the 8 MiB cap → truncated flagged, content capped.
	r := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpExec, Cmd: []string{"head", "-c", "9437184", "/dev/zero"}})
	if !r.Truncated {
		t.Errorf("Truncated = false, want true for >8MiB output")
	}
	if len(r.Stdout) != 8<<20 {
		t.Errorf("stdout len = %d, want %d (8 MiB cap)", len(r.Stdout), 8<<20)
	}
}
