package scan

import (
	"context"
	"errors"
	"testing"
)

// The publish gate maps trivy's exit code to pass/fail: 0=clean→pass, 1=vulns→fail (not an
// error), other=scan error.
func TestTrivy_exit_code_maps_to_gate(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		code     int
		runErr   error
		wantPass bool
		wantErr  bool
	}{
		{"clean", 0, nil, true, false},
		{"vulns_found", 1, nil, false, false},
		{"scan_error", 2, nil, false, true},
		{"binary_missing", -1, errors.New("exec: trivy not found"), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tv := &Trivy{Bin: "trivy", Severities: "HIGH,CRITICAL",
				run: func(context.Context, string, ...string) (int, error) { return tc.code, tc.runErr }}
			pass, err := tv.Scan(ctx, "img@sha256:deadbeef")
			if pass != tc.wantPass {
				t.Errorf("pass = %v, want %v", pass, tc.wantPass)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
