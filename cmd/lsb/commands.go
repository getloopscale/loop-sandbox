package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// Exit codes of the CLI itself. `sandbox exec` returns the exit code of the
// command it ran instead.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// logPollWait is how long one request of `logs --follow` waits for new output.
const logPollWait = 20 * time.Second

const usageText = `lsb is the LoopSandbox CLI.

Usage:
  lsb sandbox create [--image IMAGE | --template ID] [--ttl 10m] [--env K=V] [--label K=V]
  lsb sandbox list [--state STATE] [--json]
  lsb sandbox exec [--timeout 30s] SANDBOX -- COMMAND [ARG...]
  lsb sandbox connect SANDBOX
  lsb sandbox logs [--follow] SANDBOX
  lsb sandbox kill SANDBOX [SANDBOX...]

Environment:
  LOOPSANDBOX_API_KEY  API key (required)
  LOOPSANDBOX_URL      address of the API (default http://localhost:8080)
`

// usageError is a mistake in how the command was called.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// exitCodeError carries the exit code of a command run in a sandbox.
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("the command exited with %d", e.code) }

// cli is one invocation of lsb, with its streams and environment injected.
type cli struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
}

// run executes the command line and returns the process exit code.
func (c *cli) run(ctx context.Context, args []string) int {
	err := c.dispatch(ctx, args)
	var (
		ue *usageError
		ee *exitCodeError
	)
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ee):
		return ee.code
	case errors.As(err, &ue):
		_, _ = fmt.Fprintf(c.stderr, "lsb: %s\n\n%s", ue.msg, usageText)
		return exitUsage
	default:
		_, _ = fmt.Fprintf(c.stderr, "lsb: %v\n", err)
		return exitError
	}
}

func (c *cli) dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("no command given")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, _ = io.WriteString(c.stdout, usageText)
		return nil
	}
	if args[0] != "sandbox" {
		return usagef("unknown command %q", args[0])
	}
	if len(args) < 2 {
		return usagef("sandbox needs a subcommand")
	}
	commands := map[string]func(context.Context, *client, []string) error{
		"create": c.create, "list": c.list, "exec": c.exec, "connect": c.connect, "logs": c.logs, "kill": c.kill,
	}
	command, ok := commands[args[1]]
	if !ok {
		return usagef("unknown subcommand %q of sandbox", args[1])
	}
	api, err := newClient(c.getenv)
	if err != nil {
		return err
	}
	return command(ctx, api, args[2:])
}

// flags returns a flag set that reports mistakes as usage errors.
func (c *cli) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return usagef("%s: %v", fs.Name(), err)
	}
	return nil
}

// pairs collects repeated K=V flags.
type pairs map[string]string

func (p pairs) String() string { return fmt.Sprint(map[string]string(p)) }

func (p pairs) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("%q is not KEY=VALUE", v)
	}
	p[k] = val
	return nil
}

func (c *cli) create(ctx context.Context, api *client, args []string) error {
	fs := c.flags("sandbox create")
	image := fs.String("image", "", "image to run")
	template := fs.String("template", "", "template to start from, instead of an image")
	ttl := fs.Duration("ttl", 0, "lifetime, such as 10m; the server default when not set")
	env, labels := pairs{}, pairs{}
	fs.Var(env, "env", "KEY=VALUE set for every command; repeatable")
	fs.Var(labels, "label", "KEY=VALUE label; repeatable")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef("sandbox create takes no argument, got %q", fs.Arg(0))
	}
	if *image != "" && *template != "" {
		return usagef("--image and --template cannot be used together")
	}
	if *ttl < 0 {
		return usagef("--ttl must not be negative")
	}
	sb, err := api.createSandbox(ctx, createRequest{
		Image: *image, Template: *template, TTLSeconds: int64(ttl.Seconds()), Metadata: labels, Env: env,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(c.stdout, sb.ID)
	return err
}

func (c *cli) list(ctx context.Context, api *client, args []string) error {
	fs := c.flags("sandbox list")
	state := fs.String("state", "", "keep only this state: pending, running, paused or destroyed")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usagef("sandbox list takes no argument, got %q", fs.Arg(0))
	}
	sandboxes, err := api.listSandboxes(ctx, *state)
	if err != nil {
		return err
	}
	if *asJSON {
		if sandboxes == nil {
			sandboxes = []sandbox{}
		}
		enc := json.NewEncoder(c.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sandboxes)
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATE\tIMAGE\tEXPIRES")
	for _, sb := range sandboxes {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", sb.ID, sb.State, sb.Image, sb.ExpiresAt)
	}
	return tw.Flush()
}

func (c *cli) exec(ctx context.Context, api *client, args []string) error {
	fs := c.flags("sandbox exec")
	timeout := fs.Duration("timeout", 0, "stop the command after this long; the server default when not set")
	if err := parse(fs, args); err != nil {
		return err
	}
	// The flag package consumes the "--" that separates the sandbox from the command.
	rest := fs.Args()
	if len(rest) < 2 {
		return usagef("sandbox exec needs a sandbox and a command: lsb sandbox exec SANDBOX -- COMMAND [ARG...]")
	}
	id, cmd := rest[0], rest[1:]
	if cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		return usagef("sandbox exec needs a command after --")
	}
	if *timeout < 0 {
		return usagef("--timeout must not be negative")
	}
	res, err := api.exec(ctx, id, cmd, *timeout)
	if err != nil {
		return err
	}
	_, _ = io.WriteString(c.stdout, res.Stdout)
	_, _ = io.WriteString(c.stderr, res.Stderr)
	if res.ExitCode != 0 {
		return &exitCodeError{code: res.ExitCode}
	}
	return nil
}

func (c *cli) logs(ctx context.Context, api *client, args []string) error {
	fs := c.flags("sandbox logs")
	follow := fs.Bool("follow", false, "keep printing new output until interrupted or the sandbox is gone")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("sandbox logs needs exactly one sandbox")
	}
	id := fs.Arg(0)
	var after int64
	for first := true; ; first = false {
		wait := time.Duration(0)
		if *follow && !first {
			wait = logPollWait
		}
		page, err := api.logs(ctx, id, after, wait)
		switch {
		case ctx.Err() != nil:
			return nil // interrupted: the user asked to stop following
		case err != nil && !first && isNotFound(err):
			return nil // the sandbox was destroyed while following
		case err != nil:
			return err
		}
		if page.Truncated {
			_, _ = fmt.Fprintln(c.stderr, "lsb: older output was dropped by the sandbox log buffer")
		}
		for _, e := range page.Entries {
			out := c.stdout
			if e.Stream == "stderr" {
				out = c.stderr
			}
			_, _ = io.WriteString(out, e.Data)
		}
		if page.Next == after && !*follow {
			return nil
		}
		after = page.Next
	}
}

func (c *cli) kill(ctx context.Context, api *client, args []string) error {
	if len(args) == 0 {
		return usagef("sandbox kill needs at least one sandbox")
	}
	var failed []error
	for _, id := range args {
		if err := api.destroySandbox(ctx, id); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", id, err))
			continue
		}
		_, _ = fmt.Fprintln(c.stdout, id)
	}
	return errors.Join(failed...)
}
