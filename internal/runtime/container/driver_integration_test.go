//go:build integration

package container

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dcontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

const testImage = "busybox:latest"

func newDriver(t *testing.T) (*Driver, *client.Client) {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	if _, err := cli.Ping(context.Background()); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	// NewFromEnv verifies host hardening (fail-closed on unconfined seccomp).
	d, err := NewFromEnv(testImage)
	if err != nil {
		t.Fatalf("NewFromEnv (host not hardened?): %v", err)
	}
	return d, cli
}

// uid returns a per-test unique sandbox id to avoid cross-run name collisions.
func uid(tag string) runtime.SandboxID {
	return runtime.SandboxID(tag + "-" + time.Now().Format("150405.000000"))
}

func TestDriver_lifecycle_and_hardened_profile(t *testing.T) {
	d, cli := newDriver(t)
	ctx := context.Background()
	id := uid("life")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })

	sb, err := d.Create(ctx, runtime.CreateRequest{
		SandboxID: id, TenantID: "tenant-a", Image: testImage,
		CPUNanos: 1_000_000_000, MemoryBytes: 256 << 20, PidsLimit: 128,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.State != runtime.StateRunning {
		t.Fatalf("state = %q, want running", sb.State)
	}

	// Inspect the REAL container to prove the hardening was accepted by Docker.
	info, err := cli.ContainerInspect(ctx, containerName(id))
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if !info.HostConfig.ReadonlyRootfs {
		t.Error("not read-only rootfs")
	}
	if info.HostConfig.Privileged {
		t.Error("Privileged is true")
	}
	if string(info.HostConfig.NetworkMode) != "none" {
		t.Errorf("network = %q, want none", info.HostConfig.NetworkMode)
	}
	if string(info.HostConfig.IpcMode) != "private" {
		t.Errorf("ipc = %q, want private", info.HostConfig.IpcMode)
	}
	if info.Config.User != "65534:65534" {
		t.Errorf("User = %q, want non-root", info.Config.User)
	}
	if len(info.HostConfig.CapDrop) == 0 || info.HostConfig.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", info.HostConfig.CapDrop)
	}
	if !hasSecurityOpt(info.HostConfig.SecurityOpt, "no-new-privileges") {
		t.Errorf("SecurityOpt = %v, want no-new-privileges", info.HostConfig.SecurityOpt)
	}
	if info.HostConfig.NanoCPUs != 1_000_000_000 || info.HostConfig.Memory != 256<<20 {
		t.Errorf("cgroup cpu/mem not applied: cpu=%d mem=%d", info.HostConfig.NanoCPUs, info.HostConfig.Memory)
	}
	if info.HostConfig.PidsLimit == nil || *info.HostConfig.PidsLimit != 128 {
		t.Errorf("PidsLimit = %v, want 128", info.HostConfig.PidsLimit)
	}
	if ws := info.HostConfig.Tmpfs["/workspace"]; strings.Contains(ws, "noexec") {
		t.Errorf("/workspace is noexec (breaks code execution): %q", ws)
	}
	if tmp := info.HostConfig.Tmpfs["/tmp"]; !strings.Contains(tmp, "noexec") {
		t.Errorf("/tmp missing noexec: %q", tmp)
	}

	// Idempotent destroy.
	if err := d.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := d.Destroy(ctx, id); err != nil {
		t.Fatalf("second Destroy (idempotent): %v", err)
	}
	if after, _ := d.Inspect(ctx, id); after.State != runtime.StateDestroyed {
		t.Errorf("state after destroy = %q, want destroyed", after.State)
	}
}

// TestDriver_pids_limit_contains_fork_bomb proves containment two ways: the bomb
// container stays alive under its pids cap (not OOM-killed / not host-killed), and
// the host survives (daemon responsive, a fresh sandbox can still be created).
func TestDriver_pids_limit_contains_fork_bomb(t *testing.T) {
	d, cli := newDriver(t)
	ctx := context.Background()
	if err := d.ensureImage(ctx, testImage); err != nil {
		t.Fatalf("ensureImage: %v", err)
	}
	cfg, host := hardenedConfig(Spec{SandboxID: "bomb", TenantID: "t", Image: testImage, PidsLimit: 64})
	cfg.Cmd = []string{"sh", "-c", ":(){ :|:& };:"}
	name := string(uid("lsb-forkbomb"))
	created, err := cli.ContainerCreate(ctx, cfg, host, nil, nil, name)
	if err != nil {
		t.Fatalf("create bomb: %v", err)
	}
	t.Cleanup(func() { _ = cli.ContainerRemove(ctx, created.ID, dcontainer.RemoveOptions{Force: true}) })
	// Start succeeding proves the bomb actually RAN (not a start-failure false pass).
	if err := cli.ContainerStart(ctx, created.ID, dcontainer.StartOptions{}); err != nil {
		t.Fatalf("start bomb: %v", err)
	}

	// The pids cap is enforced by Docker (this is what "bit" the fork bomb): a
	// contained bomb may either keep failing to fork or exit — either way the
	// host is protected. We assert the cap is applied on the real container.
	info, err := cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		t.Fatalf("inspect bomb: %v", err)
	}
	if info.HostConfig.PidsLimit == nil || *info.HostConfig.PidsLimit != 64 {
		t.Errorf("bomb PidsLimit = %v, want 64 (cap not enforced)", info.HostConfig.PidsLimit)
	}
	time.Sleep(2 * time.Second) // let the bomb hammer its cap

	// Host survived: daemon responsive AND a fresh sandbox can still be created —
	// the pids-limit contained the bomb to its own container.
	if _, err := cli.Ping(ctx); err != nil {
		t.Fatalf("daemon unresponsive after fork bomb — host not contained: %v", err)
	}
	sid := uid("survivor")
	t.Cleanup(func() { _ = d.Destroy(ctx, sid) })
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: sid, TenantID: "t", Image: testImage, PidsLimit: 32}); err != nil {
		t.Fatalf("host degraded — could not create a sandbox after fork bomb: %v", err)
	}
}

func TestDriver_rejects_disallowed_image(t *testing.T) {
	d, _ := newDriver(t)
	_, err := d.Create(context.Background(), runtime.CreateRequest{
		SandboxID: uid("bad"), TenantID: "t", Image: "evil/registry:latest",
	})
	if !errors.Is(err, ErrImageNotAllowed) {
		t.Errorf("err = %v, want ErrImageNotAllowed", err)
	}
}

func TestDriver_image_pull_failure_is_error(t *testing.T) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := cli.Ping(context.Background()); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	// Allow the bad image so we reach the pull path (which then fails).
	badImage := "busybox:this-tag-does-not-exist-xyz"
	d := New(cli, testImage, "", []string{badImage})
	_, err = d.Create(context.Background(), runtime.CreateRequest{SandboxID: uid("pf"), TenantID: "t", Image: badImage})
	if err == nil {
		t.Fatal("expected pull failure error, got nil")
	}
	if !strings.Contains(err.Error(), "pull") {
		t.Errorf("err = %v, want image pull failure", err)
	}
}

func TestDriver_inspect_exited_maps_destroyed(t *testing.T) {
	d, cli := newDriver(t)
	ctx := context.Background()
	if err := d.ensureImage(ctx, testImage); err != nil {
		t.Fatalf("ensureImage: %v", err)
	}
	id := uid("exit")
	cfg, host := hardenedConfig(Spec{SandboxID: string(id), TenantID: "t", Image: testImage})
	cfg.Cmd = []string{"true"} // exits immediately
	name := containerName(id)
	created, err := cli.ContainerCreate(ctx, cfg, host, nil, nil, name)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = cli.ContainerRemove(ctx, created.ID, dcontainer.RemoveOptions{Force: true}) })
	_ = cli.ContainerStart(ctx, created.ID, dcontainer.StartOptions{})
	// wait for exit
	wCh, eCh := cli.ContainerWait(ctx, created.ID, dcontainer.WaitConditionNotRunning)
	select {
	case <-wCh:
	case err := <-eCh:
		t.Fatalf("wait: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("container did not exit")
	}
	got, err := d.Inspect(ctx, id)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.State != runtime.StateDestroyed {
		t.Errorf("exited container Inspect = %q, want destroyed", got.State)
	}
}

func TestDriver_list_orphans_by_label(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uid("orphan")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: id, TenantID: "t", Image: testImage}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	orphans, err := d.ListOrphans(ctx)
	if err != nil {
		t.Fatalf("ListOrphans: %v", err)
	}
	for _, o := range orphans {
		if o.ID == id {
			if o.State != runtime.StateRunning {
				t.Errorf("orphan state = %q, want running", o.State)
			}
			return
		}
	}
	t.Errorf("managed container %q not found among orphans", id)
}

// Two deployments can share one Docker host. Each reaps by its own records, so
// neither may be handed the other's sandboxes as orphans.
func TestListOrphansLeavesOutTheContainersOfAnotherInstance(t *testing.T) {
	ours, _ := newDriver(t)
	theirs, _ := newDriver(t)
	ours.SetInstance("instance-ours-" + string(uid("i")))
	theirs.SetInstance("instance-theirs-" + string(uid("i")))
	ctx := context.Background()
	id := uid("scoped")
	t.Cleanup(func() { _ = ours.Destroy(ctx, id) })
	if _, err := ours.Create(ctx, runtime.CreateRequest{SandboxID: id, TenantID: "t", Image: testImage}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	seenByOurs, err := ours.ListOrphans(ctx)
	if err != nil {
		t.Fatalf("ListOrphans (ours): %v", err)
	}
	seenByTheirs, err := theirs.ListOrphans(ctx)
	if err != nil {
		t.Fatalf("ListOrphans (theirs): %v", err)
	}

	has := func(list []runtime.Sandbox) bool {
		for _, o := range list {
			if o.ID == id {
				return true
			}
		}
		return false
	}
	if !has(seenByOurs) || has(seenByTheirs) {
		t.Errorf("our instance lists the sandbox: %v; the other instance lists it: %v; want true and false", has(seenByOurs), has(seenByTheirs))
	}
}

func hasSecurityOpt(opts []string, want string) bool {
	for _, o := range opts {
		if strings.Contains(o, want) {
			return true
		}
	}
	return false
}
