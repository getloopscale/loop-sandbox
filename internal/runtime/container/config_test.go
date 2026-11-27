package container

import (
	"slices"
	"strings"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

func TestHardenedConfig_applies_trd_10_3_profile(t *testing.T) {
	cfg, host := hardenedConfig(Spec{
		SandboxID: "sb-1", TenantID: "tenant-a", Image: "busybox:latest",
		AppArmorProfile: "docker-default",
		CPUNanos:        1_000_000_000, MemoryBytes: 512 << 20, PidsLimit: 128,
	})

	// Container config
	if cfg.User != "65534:65534" {
		t.Errorf("User = %q, want non-root 65534:65534", cfg.User)
	}
	if !cfg.NetworkDisabled {
		t.Error("NetworkDisabled must be true (deny-all)")
	}
	if len(cfg.Cmd) == 0 || cfg.Cmd[0] != "sleep" {
		t.Errorf("Cmd = %v, want keepalive sleep", cfg.Cmd)
	}
	if cfg.Labels[LabelManaged] != "true" || cfg.Labels[LabelSandboxID] != "sb-1" || cfg.Labels[LabelTenantID] != "tenant-a" {
		t.Errorf("labels = %v", cfg.Labels)
	}

	// Host config — the hardening profile
	if !host.ReadonlyRootfs {
		t.Error("ReadonlyRootfs must be true")
	}
	if host.Privileged {
		t.Error("Privileged must be false")
	}
	if string(host.NetworkMode) != "none" {
		t.Errorf("NetworkMode = %q, want none", host.NetworkMode)
	}
	if string(host.IpcMode) != "private" {
		t.Errorf("IpcMode = %q, want private", host.IpcMode)
	}
	if len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", host.CapDrop)
	}
	if !slices.Contains(host.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("SecurityOpt = %v, want no-new-privileges", host.SecurityOpt)
	}
	if !slices.Contains(host.SecurityOpt, "apparmor=docker-default") {
		t.Errorf("SecurityOpt = %v, want apparmor=docker-default when host supports it", host.SecurityOpt)
	}
	// /tmp forbids execution; /workspace permits it (holds user code) but still nosuid/nodev.
	if tmp := host.Tmpfs["/tmp"]; !strings.Contains(tmp, "noexec") || !strings.Contains(tmp, "nosuid") || !strings.Contains(tmp, "nodev") {
		t.Errorf("/tmp tmpfs = %q, want noexec,nosuid,nodev", tmp)
	}
	ws := host.Tmpfs["/workspace"]
	if !strings.Contains(","+ws+",", ",exec,") {
		t.Errorf("/workspace tmpfs = %q, want exec spelled out: Docker mounts a tmpfs noexec by default", ws)
	}
	if !slices.Contains(cfg.Env, "HOME=/workspace") {
		t.Errorf("env = %v, want HOME=/workspace: the image's home is on the read-only root", cfg.Env)
	}
	if strings.Contains(ws, "noexec") {
		t.Errorf("/workspace tmpfs = %q, must NOT be noexec (it runs user code)", ws)
	}
	if !strings.Contains(ws, "nosuid") || !strings.Contains(ws, "nodev") {
		t.Errorf("/workspace tmpfs = %q, want nosuid,nodev", ws)
	}
	if host.NanoCPUs != 1_000_000_000 || host.Memory != 512<<20 {
		t.Errorf("resources = %+v, want cpu/mem applied", host.Resources)
	}
	if host.PidsLimit == nil || *host.PidsLimit != 128 {
		t.Errorf("PidsLimit = %v, want 128", host.PidsLimit)
	}
	for _, limit := range host.Ulimits {
		if limit.Name == "nproc" {
			t.Error("nproc ulimit set: it counts the user's threads on the whole host; PidsLimit bounds the container")
		}
	}
	if len(host.Ulimits) == 0 {
		t.Error("Ulimits must be set (nofile)")
	}
}

func TestHardenedConfig_no_apparmor_when_unavailable(t *testing.T) {
	_, host := hardenedConfig(Spec{SandboxID: "s", Image: "x"}) // no AppArmorProfile
	for _, o := range host.SecurityOpt {
		if strings.HasPrefix(o, "apparmor=") {
			t.Errorf("apparmor set without host support: %v", host.SecurityOpt)
		}
	}
}

func TestHardenedConfig_clamps_tmpfs(t *testing.T) {
	_, host := hardenedConfig(Spec{SandboxID: "s", Image: "x", TmpfsSizeBytes: 10 << 30}) // 10 GiB
	if !strings.Contains(host.Tmpfs["/tmp"], "size=") {
		t.Fatal("missing tmpfs size")
	}
	// clamped to maxTmpfsBytes (512Mi); must not carry the 10Gi request.
	if strings.Contains(host.Tmpfs["/tmp"], "size=10737418240") {
		t.Errorf("/tmp tmpfs not clamped: %q", host.Tmpfs["/tmp"])
	}
}

func TestHardenedConfig_runtime_selection(t *testing.T) {
	_, runc := hardenedConfig(Spec{SandboxID: "s", Image: "x"})
	if runc.Runtime != "" {
		t.Errorf("default Runtime = %q, want empty (docker default runc)", runc.Runtime)
	}
	_, gvisor := hardenedConfig(Spec{SandboxID: "s", Image: "x", Runtime: "runsc"})
	if gvisor.Runtime != "runsc" {
		t.Errorf("gVisor Runtime = %q, want runsc", gvisor.Runtime)
	}
}

// TestWithDefaults_fills_unset_bounds proves the "always bounded" invariant: a
// request omitting limits gets safe defaults; explicit values pass through.
func TestWithDefaults_fills_unset_bounds(t *testing.T) {
	got := withDefaults(runtime.CreateRequest{})
	if got.CPUNanos != defaultCPUNanos || got.MemoryBytes != defaultMemoryBytes || got.PidsLimit != defaultPidsLimit {
		t.Errorf("defaults not applied: %+v", got)
	}
	explicit := withDefaults(runtime.CreateRequest{CPUNanos: 5, MemoryBytes: 7, PidsLimit: 9})
	if explicit.CPUNanos != 5 || explicit.MemoryBytes != 7 || explicit.PidsLimit != 9 {
		t.Errorf("explicit values overwritten: %+v", explicit)
	}
}

func TestHardenedConfigWithoutAgentKeepsThePlaceholderCommand(t *testing.T) {
	cfg, host := hardenedConfig(Spec{SandboxID: "sbx_1", Image: "busybox:latest"})

	if len(cfg.Cmd) != 2 || cfg.Cmd[0] != "sleep" || len(host.Mounts) != 0 {
		t.Errorf("cmd = %v mounts = %v, want sleep infinity and no mounts", cfg.Cmd, host.Mounts)
	}
}

func TestHardenedConfigWithAgentRunsItFromAReadOnlyVolume(t *testing.T) {
	cfg, host := hardenedConfig(Spec{SandboxID: "sbx_1", Image: "busybox:latest", AgentVolume: "loopsandbox-agent-abc"})

	if len(cfg.Cmd) != 2 || cfg.Cmd[0] != "/opt/lsb/lsb-agent" || cfg.Cmd[1] != "daemon" {
		t.Errorf("cmd = %v, want the agent daemon", cfg.Cmd)
	}
	if len(host.Mounts) != 1 || host.Mounts[0].Source != "loopsandbox-agent-abc" || host.Mounts[0].Target != "/opt/lsb" || !host.Mounts[0].ReadOnly {
		t.Errorf("mounts = %+v, want the agent volume read-only at /opt/lsb", host.Mounts)
	}
	if opts := host.Tmpfs["/run/lsb"]; !strings.Contains(opts, "noexec") || !strings.Contains(opts, "mode=0700") || !strings.Contains(opts, "uid=65534") {
		t.Errorf("/run/lsb tmpfs = %q, want noexec, mode 0700 and owned by the sandbox user", opts)
	}
	if cfg.User != "65534:65534" || !host.ReadonlyRootfs || host.NetworkMode != "none" {
		t.Errorf("the agent weakened the profile: user %q readonly %v network %q", cfg.User, host.ReadonlyRootfs, host.NetworkMode)
	}
}

func TestHardenedConfigLabelsTheContainerWithItsInstance(t *testing.T) {
	scoped, _ := hardenedConfig(Spec{SandboxID: "sb-1", TenantID: "tenant-a", Image: "busybox:latest", Instance: "deployment-1"})
	unscoped, _ := hardenedConfig(Spec{SandboxID: "sb-1", TenantID: "tenant-a", Image: "busybox:latest"})

	if scoped.Labels[LabelInstance] != "deployment-1" {
		t.Errorf("labels = %v, want %s=deployment-1", scoped.Labels, LabelInstance)
	}
	if _, ok := unscoped.Labels[LabelInstance]; ok {
		t.Errorf("labels = %v, want no %s without an instance", unscoped.Labels, LabelInstance)
	}
}
