//go:build linux

// Command lsb-agent is the agent that runs inside a sandbox and serves the
// agentproto protocol, so the host driver can exec commands, transfer files and
// run code sessions.
//
// With no argument it is the agent of a Firecracker microVM, listening on vsock.
// "daemon" is the agent of a container sandbox, listening on a unix socket, and
// "connect" joins standard input and output to that socket, which is how the
// host reaches it through an exec.
//
// As PID 1, in either sandbox, it is a minimal init: it starts a copy of itself
// as the worker and then only reaps children, so an orphaned process never stays
// a zombie. In the microVM it first mounts the pseudo-filesystems. PID 1 never
// exits on its own; on a fatal error it logs and halts.
package main

import (
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mdlayher/vsock"

	"github.com/getloopscale/loop-sandbox/internal/agent"
	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// vsockPort is the fixed guest port the host driver connects to.
const vsockPort = 1024

// workspaceDir is the in-guest sandbox workspace (also the exec working dir).
const workspaceDir = "/workspace"

// defaultPath is the base PATH the agent establishes for exec'd commands. As PID 1
// the process inherits no environment, so without this every bare command name
// (echo, cat, python) fails LookPath. HOME is set so tools that expect it behave.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func main() {
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode == modeConnect {
		runConnect(os.Args[2:]) // never returns
	}
	if mode == modeNetguard {
		runNetguard() // never returns
	}
	if os.Getpid() == 1 {
		if mode == "" {
			// The microVM guest: nothing is mounted yet.
			remountRootIfRequested()
			mountPseudoFS()
		}
		setBaseEnv()
		runInit() // never returns
	}
	// Started by the init above, or run by hand outside PID 1 for development.
	setBaseEnv()
	if mode == modeDaemon {
		serveUnix(os.Args[2:]) // never returns
	}
	serve()
}

// setBaseEnv gives exec'd commands a PATH and a HOME: PID 1 inherits no environment.
func setBaseEnv() {
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", defaultPath)
	}
	if os.Getenv("HOME") == "" {
		_ = os.Setenv("HOME", "/root")
	}
}

// serve runs the agent's listeners. It does not return.
func serve() {
	l, err := vsock.Listen(vsockPort, nil)
	if err != nil {
		fatal("vsock listen: %v", err)
	}
	log.Printf("lsb-agent: serving on vsock port %d, workspace %s", vsockPort, workspaceDir)

	h := agent.New(workspaceDir)
	// Second listener: interactive PTY channel (M15). Each connection gets a shell under a
	// pseudo-terminal, pumped bidirectionally.
	if ptyL, perr := vsock.Listen(agentproto.PTYGuestPort, nil); perr == nil {
		go func() {
			for {
				conn, aerr := ptyL.Accept()
				if aerr != nil {
					continue
				}
				go func() {
					defer func() { _ = conn.Close() }()
					if serr := agent.ServePTY(conn); serr != nil {
						log.Printf("lsb-agent: pty: %v", serr)
					}
				}()
			}
		}()
	} else {
		log.Printf("lsb-agent: pty listen: %v", perr)
	}
	// On a shutdown signal (the VMM may SIGTERM before killing the guest), terminate the
	// live interpreter kernels (M12) cleanly rather than orphaning them.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigc
		h.CloseSessions()
		os.Exit(0)
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("lsb-agent: accept: %v", err)
			continue
		}
		go func() {
			defer func() { _ = conn.Close() }()
			if serr := h.Serve(conn); serr != nil {
				log.Printf("lsb-agent: serve: %v", serr)
			}
		}()
	}
}

// mountPseudoFS mounts the pseudo-filesystems the guest userspace expects. The
// rootfs is mounted read-only (shared base for snapshots/warm pools), so /workspace
// is a writable tmpfs (exec allowed — it runs user code); /tmp is tmpfs noexec.
// Failures are logged, not fatal — a snapshot-restored VM may already have them.
func mountPseudoFS() {
	type m struct {
		source, target, fstype string
		flags                  uintptr
	}
	for _, mt := range []m{
		{"proc", "/proc", "proc", 0},
		{"sysfs", "/sys", "sysfs", 0},
		{"tmpfs", "/tmp", "tmpfs", syscall.MS_NOEXEC | syscall.MS_NOSUID | syscall.MS_NODEV},
		{"tmpfs", workspaceDir, "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV},
		// devpts is required to allocate pseudo-terminals for the interactive PTY (M15).
		{"devpts", "/dev/pts", "devpts", 0},
	} {
		_ = os.MkdirAll(mt.target, 0o755)
		if err := syscall.Mount(mt.source, mt.target, mt.fstype, mt.flags, ""); err != nil {
			log.Printf("lsb-agent: mount %s: %v (continuing)", mt.target, err)
		}
	}
}

// remountRootIfRequested remounts / read-write when the host passes "lsb.rw" on the kernel
// command line (M17 template builder): a build's RUN steps must persist into the rootfs image.
// Normal sandboxes boot with a read-only rootfs (shared base for snapshots) and never see lsb.rw.
func remountRootIfRequested() {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return
	}
	for _, tok := range strings.Fields(string(b)) {
		if tok == "lsb.rw" {
			if err := syscall.Mount("", "/", "", syscall.MS_REMOUNT, ""); err != nil {
				log.Printf("lsb-agent: remount / rw: %v", err)
			}
			return
		}
	}
}

// fatal logs and halts. As PID 1 we must not return from main (kernel panic).
func fatal(format string, a ...any) {
	log.Printf("lsb-agent: FATAL: "+format, a...)
	select {} // block forever; the host will tear the VM down.
}
