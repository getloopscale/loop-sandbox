//go:build linux

package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agent"
	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// The subcommands used in a container sandbox.
const (
	modeDaemon  = "daemon"
	modeConnect = "connect"
)

// connectWait is how long connect retries while the daemon is still starting.
const connectWait = 5 * time.Second

func socketFlag(fs *flag.FlagSet) *string {
	return fs.String("socket", agentproto.ContainerSocket, "unix socket of the agent")
}

// serveUnix runs the agent on a unix socket. It does not return.
func serveUnix(args []string) {
	fs := flag.NewFlagSet(modeDaemon, flag.ExitOnError)
	socket := socketFlag(fs)
	_ = fs.Parse(args)

	if err := os.MkdirAll(filepath.Dir(*socket), 0o700); err != nil {
		fatal("create socket directory: %v", err)
	}
	_ = os.Remove(*socket) // a socket left by a previous worker
	l, err := net.Listen("unix", *socket)
	if err != nil {
		fatal("listen on %s: %v", *socket, err)
	}
	log.Printf("lsb-agent: serving on %s, workspace %s", *socket, workspaceDir)

	// Second listener: the terminal channel. Each connection gets a shell under a
	// pseudo-terminal.
	ptySocket := filepath.Join(filepath.Dir(*socket), filepath.Base(agentproto.ContainerPTYSocket))
	_ = os.Remove(ptySocket)
	if ptyL, perr := net.Listen("unix", ptySocket); perr == nil {
		go acceptLoop(ptyL, "pty", func(conn net.Conn) error { return agent.ServePTY(conn) })
	} else {
		log.Printf("lsb-agent: pty listen on %s: %v", ptySocket, perr)
	}

	h := agent.New(workspaceDir)
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigc
		h.CloseSessions()
		os.Exit(0)
	}()
	acceptLoop(l, "serve", func(conn net.Conn) error { return h.Serve(conn) })
}

// acceptLoop serves every connection of l with serve, each in its own goroutine.
// It does not return.
func acceptLoop(l net.Listener, what string, serve func(net.Conn) error) {
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("lsb-agent: %s: accept: %v", what, err)
			time.Sleep(50 * time.Millisecond) // a persistent accept error must not spin
			continue
		}
		go func() {
			defer func() { _ = conn.Close() }()
			if serr := serve(conn); serr != nil {
				log.Printf("lsb-agent: %s: %v", what, serr)
			}
		}()
	}
}

// runConnect joins standard input and output to the agent's socket and exits
// when the agent closes the connection. It does not return.
func runConnect(args []string) {
	fs := flag.NewFlagSet(modeConnect, flag.ExitOnError)
	socket := socketFlag(fs)
	_ = fs.Parse(args)

	conn, err := dialWithRetry(*socket, connectWait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lsb-agent: connect to %s: %v\n", *socket, err)
		os.Exit(111)
	}
	go func() {
		// When the host stops sending, tell the agent; when the host goes away, this
		// process is killed with the exec and the agent sees the socket close.
		_, _ = io.Copy(conn, os.Stdin)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	_, _ = io.Copy(os.Stdout, conn)
	os.Exit(0)
}

func dialWithRetry(socket string, wait time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(wait)
	for {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}
