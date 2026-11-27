//go:build linux

package main

import (
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// workerRestartDelay spaces out restarts of a worker that keeps dying, so a
// broken guest does not spin.
const workerRestartDelay = time.Second

// runInit is PID 1's whole job after the mounts: keep one worker running and reap
// every child that ends. Orphans in the guest are re-parented to PID 1, and it is
// the only process that can collect them.
//
// The worker is a separate process because reaping with wait4(-1) in the process
// that also runs commands would steal the exit status of those commands.
func runInit() {
	exe, err := os.Executable()
	if err != nil {
		fatal("locate own binary: %v", err)
	}
	var stopping atomic.Bool
	var worker atomic.Int64
	start := func() {
		pid, err := syscall.ForkExec(exe, os.Args, &syscall.ProcAttr{
			Env:   os.Environ(),
			Files: []uintptr{0, 1, 2},
		})
		if err != nil {
			fatal("start worker: %v", err)
		}
		worker.Store(int64(pid))
	}
	start()

	// A termination signal goes to the worker, which closes its sessions and exits.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigc
		stopping.Store(true)
		_ = syscall.Kill(int(worker.Load()), sig.(syscall.Signal))
	}()

	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			// ECHILD cannot happen while a worker exists; anything else is unexpected.
			log.Printf("lsb-agent: init: wait: %v", err)
			time.Sleep(workerRestartDelay)
			continue
		}
		if int64(pid) != worker.Load() {
			continue // an orphan was collected
		}
		if stopping.Load() {
			os.Exit(0)
		}
		log.Printf("lsb-agent: init: worker %d ended (status %d); restarting", pid, status)
		time.Sleep(workerRestartDelay)
		start()
	}
}
