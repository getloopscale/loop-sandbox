// Package scan adapts the trivy vulnerability scanner to the app.TemplateScanner port
// (M17 template publish gate). It shells out to the trivy binary — never reimplementing CVE
// detection (Unbreakable Rule 9).
package scan

import (
	"context"
	"fmt"
	"os/exec"
)

// Trivy scans an OCI image ref for vulnerabilities at or above the configured severity.
// A publish PASSES when trivy finds none at that severity (exit 0); FAILS when it does
// (exit 1). Any other exit is a scan error (trivy missing, image unreachable, …).
type Trivy struct {
	Bin        string // trivy binary (default "trivy")
	Severities string // e.g. "HIGH,CRITICAL"
	// run is the command runner; overridable in tests. Returns the process exit code and any
	// non-exit error (e.g. binary not found).
	run func(ctx context.Context, name string, args ...string) (int, error)
}

// NewTrivy returns a Trivy scanner gating on HIGH+CRITICAL by default.
func NewTrivy(bin string) *Trivy {
	if bin == "" {
		bin = "trivy"
	}
	return &Trivy{Bin: bin, Severities: "HIGH,CRITICAL", run: runCmd}
}

// Scan reports whether imageRef passes the gate (no HIGH/CRITICAL vulns).
func (t *Trivy) Scan(ctx context.Context, imageRef string) (bool, error) {
	runner := t.run
	if runner == nil {
		runner = runCmd
	}
	// --exit-code 1 makes trivy exit non-zero when it finds vulns at --severity.
	code, err := runner(ctx, t.Bin, "image", "--quiet", "--no-progress",
		"--severity", t.Severities, "--exit-code", "1", imageRef)
	if err != nil {
		return false, fmt.Errorf("trivy scan %s: %w", imageRef, err)
	}
	switch code {
	case 0:
		return true, nil // clean
	case 1:
		return false, nil // vulns found → fails the gate (not an error)
	default:
		return false, fmt.Errorf("trivy scan %s: unexpected exit %d", imageRef, code)
	}
}

func runCmd(ctx context.Context, name string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err // binary not found / could not start
}
