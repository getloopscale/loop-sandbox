package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/getloopscale/loop-sandbox/internal/runtime/firecracker"
)

// buildSpec is the Dockerfile-like template build spec (M17): a base rootfs image + ordered
// shell steps run in an isolated builder microVM to produce a customized rootfs.
type buildSpec struct {
	Base  string   `json:"base"`  // path to the base ext4 rootfs (defaults to the FC rootfs)
	Steps []string `json:"steps"` // shell commands run in order inside the builder VM
	Out   string   `json:"out"`   // output rootfs path
}

// templateBuild implements `sandboxctl template build --spec <file>`: it builds a template
// rootfs in an isolated Firecracker microVM (never on the host) and prints the sha256 digest.
// The resulting image is registered separately via `POST /v1/templates`.
func templateBuild(args []string) {
	fs := flag.NewFlagSet("template build", flag.ExitOnError)
	specPath := fs.String("spec", "", "path to a JSON build spec {base, steps, out}")
	_ = fs.Parse(args)
	if *specPath == "" {
		fatal("--spec required")
	}
	raw, err := os.ReadFile(*specPath)
	if err != nil {
		fatal("read spec: %v", err)
	}
	var spec buildSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		fatal("parse spec: %v", err)
	}
	if spec.Out == "" {
		fatal("spec.out required")
	}
	if len(spec.Steps) == 0 {
		fatal("spec.steps must have at least one step")
	}

	d, err := firecracker.NewFromEnv()
	if err != nil {
		fatal("firecracker builder: %v (set LOOPSANDBOX_FC_* assets + /dev/kvm)", err)
	}
	base := spec.Base
	if base == "" {
		base = os.Getenv("LOOPSANDBOX_FC_ROOTFS")
	}
	fmt.Printf("building template from %s → %s (%d steps) in an isolated microVM…\n", base, spec.Out, len(spec.Steps))
	if err := d.BuildTemplate(context.Background(), base, spec.Out, spec.Steps); err != nil {
		fatal("build: %v", err)
	}
	digest, err := fileSHA256(spec.Out)
	if err != nil {
		fatal("digest: %v", err)
	}
	fmt.Printf("template built: %s\n  digest: sha256:%s\n", spec.Out, digest)
	fmt.Printf("register it with:  POST /v1/templates {\"name\":\"…\",\"image\":\"%s@sha256:%s\"}\n", spec.Out, digest)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
