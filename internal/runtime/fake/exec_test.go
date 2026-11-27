package fake_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

func TestFake_exec_echo_and_cat(t *testing.T) {
	d := fake.New()
	ctx := context.Background()
	_, _ = d.Create(ctx, runtime.CreateRequest{SandboxID: "s1"})

	res, err := d.Exec(ctx, "s1", runtime.ExecRequest{Cmd: []string{"echo", "hello", "world"}})
	if err != nil {
		t.Fatalf("Exec echo: %v", err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "hello world\n" {
		t.Errorf("echo result = %+q exit %d", res.Stdout, res.ExitCode)
	}

	// cat of a missing file → exit 1 (process failure, not a Go error).
	miss, err := d.Exec(ctx, "s1", runtime.ExecRequest{Cmd: []string{"cat", "/workspace/nope"}})
	if err != nil {
		t.Fatalf("Exec cat missing: unexpected error %v", err)
	}
	if miss.ExitCode != 1 {
		t.Errorf("cat missing exit = %d, want 1", miss.ExitCode)
	}
}

func TestFake_file_roundtrip(t *testing.T) {
	d := fake.New()
	ctx := context.Background()
	_, _ = d.Create(ctx, runtime.CreateRequest{SandboxID: "s1"})

	if err := d.WriteFile(ctx, "s1", "/workspace/a.txt", bytes.NewBufferString("data")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, err := d.ReadFile(ctx, "s1", "/workspace/a.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "data" {
		t.Errorf("read = %q, want data", b)
	}
	// cat sees the written file.
	res, _ := d.Exec(ctx, "s1", runtime.ExecRequest{Cmd: []string{"cat", "/workspace/a.txt"}})
	if string(res.Stdout) != "data" {
		t.Errorf("cat = %q, want data", res.Stdout)
	}

	entries, _ := d.ListFiles(ctx, "s1", "/workspace")
	if len(entries) != 1 || entries[0].Name != "/workspace/a.txt" {
		t.Errorf("list = %+v, want a.txt", entries)
	}

	if err := d.DeleteFile(ctx, "s1", "/workspace/a.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := d.ReadFile(ctx, "s1", "/workspace/a.txt"); err != runtime.ErrFileNotFound {
		t.Errorf("read after delete = %v, want ErrFileNotFound", err)
	}
}
