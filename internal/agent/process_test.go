//go:build unix

package agent_test

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

func startProc(t *testing.T, conn net.Conn, script string) int {
	t.Helper()
	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcStart, Cmd: []string{"sh", "-c", script}})
	if resp.Err != "" || resp.Process == nil || resp.Process.PID <= 0 || !resp.Process.Running {
		t.Fatalf("start %q: %+v (process %+v)", script, resp, resp.Process)
	}
	t.Cleanup(func() {
		// The answer is read too: the pipe is synchronous, and an unread answer
		// would block the agent.
		if agentproto.WriteFrame(conn, &agentproto.Request{Op: agentproto.OpProcSignal, PID: resp.Process.PID, Signal: "KILL"}) == nil {
			_, _ = agentproto.ReadResponse(conn)
		}
	})
	return resp.Process.PID
}

// drain reads the output of pid from cursor 0 until the process ends.
func drain(t *testing.T, conn net.Conn, pid int) (stdout, stderr string, last *agentproto.Response) {
	t.Helper()
	var after int64
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcOutput, PID: pid, After: after, WaitMS: 2000})
		if resp.Err != "" {
			t.Fatalf("output: %q", resp.Err)
		}
		stdout += string(resp.Stdout)
		stderr += string(resp.Stderr)
		after = resp.Next
		if !resp.Process.Running {
			return stdout, stderr, resp
		}
	}
	t.Fatalf("process %d did not end; stdout so far %q", pid, stdout)
	return "", "", nil
}

func TestProcessStartReturnsBeforeTheCommandEnds(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	begin := time.Now()

	startProc(t, conn, "sleep 30")

	if took := time.Since(begin); took > 5*time.Second {
		t.Errorf("start took %s, want it to return at once", took)
	}
}

func TestProcessOutputCarriesBothStreamsAndTheExitCode(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	pid := startProc(t, conn, "echo out; echo err >&2; exit 7")

	stdout, stderr, last := drain(t, conn, pid)

	if stdout != "out\n" || stderr != "err\n" || last.Process.ExitCode != 7 {
		t.Errorf("stdout=%q stderr=%q exit=%d, want out, err and 7", stdout, stderr, last.Process.ExitCode)
	}
}

func TestProcessOutputArrivesBeforeTheProcessEnds(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	pid := startProc(t, conn, "echo first; sleep 30")

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcOutput, PID: pid, WaitMS: 5000})

	if string(resp.Stdout) != "first\n" || !resp.Process.Running {
		t.Errorf("stdout=%q running=%v, want first while the process runs", resp.Stdout, resp.Process.Running)
	}
}

func TestProcessOutputCanBeReadAgainFromAnEarlierCursor(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	pid := startProc(t, conn, "printf abc; printf def")
	drain(t, conn, pid)

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcOutput, PID: pid, After: 2})

	if string(resp.Stdout) != "cdef" || resp.Next != 6 || resp.Truncated {
		t.Errorf("stdout=%q next=%d truncated=%v, want cdef, 6 and not truncated", resp.Stdout, resp.Next, resp.Truncated)
	}
}

func TestProcessOutputWithNothingNewWaitsAndReturnsEmpty(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	pid := startProc(t, conn, "sleep 30")
	begin := time.Now()

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcOutput, PID: pid, WaitMS: 300})

	if took := time.Since(begin); took < 250*time.Millisecond || took > 5*time.Second {
		t.Errorf("the request took %s, want about 300ms", took)
	}
	if len(resp.Stdout) != 0 || resp.Next != 0 || !resp.Process.Running {
		t.Errorf("resp = %+v, want no output and a running process", resp)
	}
}

func TestProcessOutputBeyondTheKeptWindowIsMarkedTruncated(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	// 3 MiB, more than the agent keeps.
	pid := startProc(t, conn, "head -c 3145728 /dev/zero | tr '\\0' x")
	for {
		resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcOutput, PID: pid, After: 1 << 40, WaitMS: 2000})
		if !resp.Process.Running {
			break
		}
	}

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcOutput, PID: pid})

	if !resp.Truncated || resp.Next != 3145728 || len(resp.Stdout) == 0 || len(resp.Stdout) > 2<<20 {
		t.Errorf("truncated=%v next=%d kept=%d, want truncated, next 3145728 and a bounded tail", resp.Truncated, resp.Next, len(resp.Stdout))
	}
}

func TestProcessReadsWhatIsWrittenToItsStdinUntilItIsClosed(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	pid := startProc(t, conn, "cat; echo done")
	first := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcStdin, PID: pid, Data: []byte("hello ")})
	second := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcStdin, PID: pid, Data: []byte("world\n")})

	closed := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcCloseStdin, PID: pid})
	stdout, _, last := drain(t, conn, pid)

	if first.Err != "" || second.Err != "" || closed.Err != "" {
		t.Fatalf("stdin errors: %q %q %q", first.Err, second.Err, closed.Err)
	}
	if stdout != "hello world\ndone\n" || last.Process.ExitCode != 0 {
		t.Errorf("stdout=%q exit=%d, want the echoed input, done and 0", stdout, last.Process.ExitCode)
	}
}

func TestProcessStdinAfterCloseIsRefused(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	pid := startProc(t, conn, "sleep 30")
	roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcCloseStdin, PID: pid})

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcStdin, PID: pid, Data: []byte("x")})

	if resp.Err != agentproto.ErrStdinClosed {
		t.Errorf("Err = %q, want stdin_closed", resp.Err)
	}
}

func TestProcessSignalEndsTheProcessAndItsChildren(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	pid := startProc(t, conn, "sleep 4141 & echo child=$!; wait")
	first := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcOutput, PID: pid, WaitMS: 5000})
	child := strings.TrimSpace(strings.TrimPrefix(string(first.Stdout), "child="))

	sent := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcSignal, PID: pid, Signal: "TERM"})
	_, _, last := drain(t, conn, pid)

	if sent.Err != "" || last.Process.ExitCode != 143 {
		t.Errorf("signal err=%q exit=%d, want none and 143", sent.Err, last.Process.ExitCode)
	}
	probe := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpExec, Cmd: []string{"sh", "-c", "kill -0 " + child}})
	if probe.ExitCode == 0 {
		t.Errorf("the child %s survived the signal", child)
	}
}

func TestProcessOpsOnAnEndedOrUnknownProcess(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	pid := startProc(t, conn, "true")
	drain(t, conn, pid)
	cases := map[string]struct {
		req  *agentproto.Request
		want string
	}{
		"signal an ended process":     {&agentproto.Request{Op: agentproto.OpProcSignal, PID: pid, Signal: "TERM"}, agentproto.ErrProcessExited},
		"stdin of an ended process":   {&agentproto.Request{Op: agentproto.OpProcStdin, PID: pid, Data: []byte("x")}, agentproto.ErrProcessExited},
		"output of an unknown pid":    {&agentproto.Request{Op: agentproto.OpProcOutput, PID: 1 << 30}, agentproto.ErrNotFound},
		"signal an unknown pid":       {&agentproto.Request{Op: agentproto.OpProcSignal, PID: 1 << 30, Signal: "TERM"}, agentproto.ErrNotFound},
		"a command that cannot start": {&agentproto.Request{Op: agentproto.OpProcStart, Cmd: []string{"/no/such/binary"}}, agentproto.ErrStartFailed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			resp := roundTrip(t, conn, c.req)

			if resp.Err != c.want {
				t.Errorf("Err = %q, want %q", resp.Err, c.want)
			}
		})
	}
}

func TestProcessSignalWithAnUnknownNameIsRefused(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	pid := startProc(t, conn, "sleep 30")

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcSignal, PID: pid, Signal: "NOPE"})

	if resp.Err == "" {
		t.Error("an unknown signal name was accepted")
	}
}

func TestProcessListShowsRunningAndEndedProcesses(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	ended := startProc(t, conn, "exit 3")
	drain(t, conn, ended)
	running := startProc(t, conn, "sleep 30")

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcList})

	byPID := map[int]agentproto.ProcessInfo{}
	for _, p := range resp.Processes {
		byPID[p.PID] = p
	}
	if len(byPID) != 2 || byPID[ended].Running || byPID[ended].ExitCode != 3 || !byPID[running].Running {
		t.Errorf("processes = %+v, want %d ended with 3 and %d running", resp.Processes, ended, running)
	}
	if got := byPID[running].Cmd; len(got) != 3 || got[2] != "sleep 30" {
		t.Errorf("cmd of the running process = %v", got)
	}
}

func TestProcessStartIsRefusedBeyondTheLimit(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	for range 32 {
		startProc(t, conn, "sleep 30")
	}

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpProcStart, Cmd: []string{"sleep", "30"}})

	if resp.Err != agentproto.ErrTooManyProcesses {
		t.Errorf("Err = %q, want too_many_processes", resp.Err)
	}
}

func TestSandboxLogHasTheOutputOfEveryProcessWithItsPID(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	first := startProc(t, conn, "echo one")
	drain(t, conn, first)
	second := startProc(t, conn, "echo two >&2")
	drain(t, conn, second)

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpLogs})

	if resp.Err != "" || len(resp.Logs) != 2 || resp.Next != 8 {
		t.Fatalf("resp = %+v, want two records and cursor 8", resp)
	}
	a, b := resp.Logs[0], resp.Logs[1]
	if a.PID != first || a.Stderr || string(a.Data) != "one\n" || b.PID != second || !b.Stderr || string(b.Data) != "two\n" || a.AtMS == 0 {
		t.Errorf("logs = %+v", resp.Logs)
	}
}

func TestSandboxLogResumesFromACursor(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	drain(t, conn, startProc(t, conn, "printf abc"))
	seen := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpLogs}).Next
	drain(t, conn, startProc(t, conn, "printf def"))

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpLogs, After: seen - 1})

	if len(resp.Logs) != 2 || string(resp.Logs[0].Data) != "c" || string(resp.Logs[1].Data) != "def" || resp.Next != 6 {
		t.Errorf("logs after %d = %+v next %d; want c, def and 6", seen-1, resp.Logs, resp.Next)
	}
}

func TestSandboxLogWaitsForOutput(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	startProc(t, conn, "sleep 0.3; echo late; sleep 30")

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpLogs, WaitMS: 10000})

	if len(resp.Logs) != 1 || string(resp.Logs[0].Data) != "late\n" {
		t.Errorf("logs = %+v, want the line written while waiting", resp.Logs)
	}
}

func TestStatsReportsCPUMemoryAndRunningProcesses(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	startProc(t, conn, "sleep 30")

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpStats})

	if resp.Err != "" || resp.Stats == nil {
		t.Fatalf("resp = %+v", resp)
	}
	if s := resp.Stats; s.CPUNanos == 0 || s.MemoryTotal == 0 || s.MemoryUsed == 0 || s.MemoryUsed > s.MemoryTotal || s.Processes != 1 {
		t.Errorf("stats = %+v, want CPU time, used memory within the total and 1 process", s)
	}
}

// A process can write before the agent has recorded its PID: the output is
// copied from the moment the process starts.
func TestSandboxLogNeverRecordsOutputWithoutThePIDOfItsProcess(t *testing.T) {
	conn := serveConn(t, t.TempDir())
	const processes = 150
	for range processes {
		drain(t, conn, startProc(t, conn, "echo x"))
	}

	resp := roundTrip(t, conn, &agentproto.Request{Op: agentproto.OpLogs})

	if len(resp.Logs) != processes {
		t.Fatalf("got %d log records, want %d", len(resp.Logs), processes)
	}
	for i, rec := range resp.Logs {
		if rec.PID == 0 {
			t.Fatalf("record %d has no PID: %+v", i, rec)
		}
	}
}
