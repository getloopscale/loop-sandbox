// Command lsb is the user CLI of LoopSandbox. It talks to the HTTP API with an
// API key and needs no access to the database or to the host.
//
// Usage:
//
//	lsb sandbox create [--image IMAGE | --template ID] [--ttl 10m] [--env K=V] [--label K=V]
//	                   [--allow-out HOST] [--deny-out CIDR]
//	lsb sandbox list [--state STATE] [--json]
//	lsb sandbox exec [--timeout 30s] SANDBOX -- COMMAND [ARG...]
//	lsb sandbox connect SANDBOX
//	lsb sandbox logs [--follow] SANDBOX
//	lsb sandbox kill SANDBOX [SANDBOX...]
//
// Configuration comes from the environment: LOOPSANDBOX_API_KEY (required) and
// LOOPSANDBOX_URL (default http://localhost:8080).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	c := &cli{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}
	code := c.run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
