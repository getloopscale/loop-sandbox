// Command bench measures a running LoopSandbox API: sandbox creation, snapshot
// restore, fork and how many sandboxes one node holds at once. It writes the
// numbers with the hardware and the versions they were measured on, as JSON
// and as a Markdown page.
//
// It needs LSB_BASE_URL and LSB_API_KEY. scripts/bench.sh starts a stack and
// runs it; run it directly to measure a deployment that is already up.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type options struct {
	samples   int
	density   int
	image     string
	jsonPath  string
	mdPath    string
	meta      metaFlags
	localHost bool
}

// metaFlags collects -meta key=value pairs: versions only the caller knows.
type metaFlags map[string]string

func (m metaFlags) String() string { return fmt.Sprint(map[string]string(m)) }

func (m metaFlags) Set(v string) error {
	key, value, ok := strings.Cut(v, "=")
	if !ok || key == "" {
		return fmt.Errorf("meta %q: want key=value", v)
	}
	m[key] = value
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run() error {
	opts := options{meta: metaFlags{}}
	flag.IntVar(&opts.samples, "samples", 20, "samples per measurement")
	flag.IntVar(&opts.density, "density", 50, "sandboxes the density measurement tries to hold at once; 0 skips it")
	flag.StringVar(&opts.image, "image", "busybox:latest", "image of the sandboxes")
	flag.StringVar(&opts.jsonPath, "json", "", "file to write the report to as JSON")
	flag.StringVar(&opts.mdPath, "markdown", "", "file to write the report to as Markdown")
	flag.BoolVar(&opts.localHost, "local", true, "the API runs on this host, so its hardware and memory are read here")
	flag.Var(opts.meta, "meta", "key=value recorded with the report, repeatable (for example commit=abc123)")
	flag.Parse()
	if opts.samples < 1 {
		return errors.New("-samples must be 1 or more")
	}
	if opts.density < 0 {
		return errors.New("-density must be 0 or more")
	}
	baseURL, apiKey := strings.TrimRight(os.Getenv("LSB_BASE_URL"), "/"), os.Getenv("LSB_API_KEY")
	if baseURL == "" || apiKey == "" {
		return errors.New("LSB_BASE_URL and LSB_API_KEY must be set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := &client{baseURL: baseURL, apiKey: apiKey, http: &http.Client{Timeout: 2 * time.Minute}}

	report, err := measure(ctx, c, opts)
	if err != nil {
		return err
	}
	if opts.jsonPath != "" {
		raw, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encode the report: %w", err)
		}
		if err := os.WriteFile(opts.jsonPath, append(raw, '\n'), 0o644); err != nil {
			return fmt.Errorf("write the JSON report: %w", err)
		}
	}
	md := report.markdown()
	if opts.mdPath != "" {
		if err := os.WriteFile(opts.mdPath, []byte(md), 0o644); err != nil {
			return fmt.Errorf("write the Markdown report: %w", err)
		}
	}
	fmt.Print(md)
	return nil
}
