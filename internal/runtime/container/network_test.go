package container

import (
	"errors"
	"slices"
	"testing"

	dcontainer "github.com/docker/docker/api/types/container"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

func TestSandboxWithoutAGuardHasNoNetwork(t *testing.T) {
	cfg, host := hardenedConfig(Spec{SandboxID: "sbx_1", Image: "busybox:latest"})

	if host.NetworkMode != "none" || !cfg.NetworkDisabled {
		t.Errorf("NetworkMode = %q, NetworkDisabled = %v; want none and disabled", host.NetworkMode, cfg.NetworkDisabled)
	}
}

func TestSandboxWithAGuardJoinsItsNamespaceAndGainsNoCapability(t *testing.T) {
	cfg, host := hardenedConfig(Spec{SandboxID: "sbx_1", Image: "busybox:latest", NetworkContainer: "abc123"})

	if host.NetworkMode != "container:abc123" || cfg.NetworkDisabled {
		t.Errorf("NetworkMode = %q, NetworkDisabled = %v; want container:abc123 and enabled", host.NetworkMode, cfg.NetworkDisabled)
	}
	if len(host.CapAdd) != 0 || !slices.Equal(host.CapDrop, []string{"ALL"}) {
		t.Errorf("CapAdd = %v, CapDrop = %v; a networked sandbox keeps no capability", host.CapAdd, host.CapDrop)
	}
	if cfg.User != "65534:65534" {
		t.Errorf("User = %q, want the unprivileged sandbox user", cfg.User)
	}
}

func TestGuardHoldsOnlyNetAdminAndRunsTheAgentInNetguardMode(t *testing.T) {
	cfg, host := guardConfig(guardSpec{
		SandboxID: "sbx_1", TenantID: "tnt_1", Image: "busybox:latest", AgentVolume: "vol", Instance: "inst",
		DNS: []string{"8.8.8.8"}, AllowOut: []string{"pypi.org", "*.pythonhosted.org"}, DenyOut: []string{"0.0.0.0/0"},
	})

	if !slices.Equal(host.CapDrop, []string{"ALL"}) || !slices.Equal(host.CapAdd, []string{"NET_ADMIN"}) {
		t.Errorf("CapDrop = %v, CapAdd = %v; want ALL dropped and only NET_ADMIN added", host.CapDrop, host.CapAdd)
	}
	if host.Privileged || !host.ReadonlyRootfs || !slices.Contains(host.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("guard is not hardened: privileged=%v readonly=%v secopt=%v", host.Privileged, host.ReadonlyRootfs, host.SecurityOpt)
	}
	if !slices.Equal(cfg.Cmd, []string{"/opt/lsb/lsb-agent", "netguard"}) {
		t.Errorf("Cmd = %v", cfg.Cmd)
	}
	for _, want := range []string{
		"LSB_NET_ALLOW_OUT=pypi.org,*.pythonhosted.org", "LSB_NET_DENY_OUT=0.0.0.0/0", "LSB_NET_DNS=8.8.8.8",
	} {
		if !slices.Contains(cfg.Env, want) {
			t.Errorf("Env = %v, missing %s", cfg.Env, want)
		}
	}
	if len(host.Mounts) != 1 || !host.Mounts[0].ReadOnly {
		t.Errorf("Mounts = %+v, want the agent volume read-only", host.Mounts)
	}
	if host.Memory == 0 || host.PidsLimit == nil {
		t.Error("the guard has no memory or process limit")
	}
	labels := cfg.Labels
	if labels[LabelRole] != "netguard" || labels[LabelSandboxID] != "sbx_1" || labels[LabelInstance] != "inst" || labels[LabelManaged] != "true" {
		t.Errorf("Labels = %v", labels)
	}
}

func TestEnableNetworkNeedsTheAgentAndIPv4Resolvers(t *testing.T) {
	d := New(nil, "", "", nil)
	if err := d.EnableNetwork(); !errors.Is(err, ErrNetworkNeedsAgent) {
		t.Errorf("without the agent: %v, want ErrNetworkNeedsAgent", err)
	}

	d.agentVolume = "vol"
	for _, bad := range []string{"dns.google", "2001:4860:4860::8888", ""} {
		if err := d.EnableNetwork(bad); err == nil {
			t.Errorf("resolver %q was accepted", bad)
		}
	}
	if err := d.EnableNetwork(); err != nil || !slices.Equal(d.networkDNS, []string{DefaultDNS}) {
		t.Errorf("default resolver: err=%v dns=%v", err, d.networkDNS)
	}
}

func TestAGVisorSandboxGetsNoNetwork(t *testing.T) {
	d := New(nil, "", "", nil)
	d.agentVolume = "vol"
	if err := d.EnableNetwork(); err != nil {
		t.Fatal(err)
	}

	if !d.networked(runtime.CreateRequest{}) {
		t.Error("the default runtime should get a network")
	}
	if d.networked(runtime.CreateRequest{OCIRuntime: "runsc"}) {
		t.Error("a gVisor sandbox must not get a network the guard cannot enforce")
	}
}

func TestOrphansListASandboxOnceAndReportALoneGuardAsDestroyed(t *testing.T) {
	sandbox := func(id, state string) dcontainer.Summary {
		return dcontainer.Summary{State: state, Labels: map[string]string{LabelSandboxID: id, LabelTenantID: "tnt_1"}}
	}
	guard := func(id string) dcontainer.Summary {
		return dcontainer.Summary{State: "running", Labels: map[string]string{LabelSandboxID: id, LabelRole: roleNetguard}}
	}

	got := orphansFrom([]dcontainer.Summary{
		guard("sbx_guard_first"), sandbox("sbx_guard_first", "running"),
		sandbox("sbx_sandbox_first", "running"), guard("sbx_sandbox_first"),
		sandbox("sbx_exited", "exited"), guard("sbx_exited"),
		guard("sbx_lone_guard"),
		sandbox("sbx_no_network", "running"),
	})

	want := map[runtime.SandboxID]runtime.State{
		"sbx_guard_first": runtime.StateRunning, "sbx_sandbox_first": runtime.StateRunning,
		"sbx_exited": runtime.StateDestroyed, "sbx_lone_guard": runtime.StateDestroyed, "sbx_no_network": runtime.StateRunning,
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d sandboxes, want %d: %+v", len(got), len(want), got)
	}
	for _, s := range got {
		if want[s.ID] != s.State {
			t.Errorf("%s: state %s, want %s", s.ID, s.State, want[s.ID])
		}
	}
}
