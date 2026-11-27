package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	dcontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/getloopscale/loop-sandbox/internal/egress"
	"github.com/getloopscale/loop-sandbox/internal/netguard"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// NetworkEnv names the environment variable that gives sandboxes a network. Its
// value is the comma-separated list of DNS resolvers a sandbox uses, or "on" for
// the default resolver.
const NetworkEnv = "LOOPSANDBOX_CONTAINER_NETWORK"

// DefaultDNS is the resolver of a sandbox when the operator names none.
const DefaultDNS = "8.8.8.8"

// LabelRole tells a sandbox's network guard apart from the sandbox itself.
const (
	LabelRole     = "loopsandbox.role"
	roleNetguard  = "netguard"
	guardPrefix   = "lsb-net-"
	guardReadyFor = 15 * time.Second
)

// ErrNetworkNeedsAgent is returned when the network is enabled on a driver that
// has no agent: the guard is the agent binary in another mode.
var ErrNetworkNeedsAgent = errors.New("the sandbox network needs the agent; set " + AgentBinEnv)

func guardName(id runtime.SandboxID) string { return guardPrefix + string(id) }

// EnableNetwork gives every sandbox created from now on a network interface
// whose outbound traffic is judged by a guard container. dns are the resolvers a
// sandbox uses; none means DefaultDNS. Call EnableAgent first.
func (d *Driver) EnableNetwork(dns ...string) error {
	if d.agentVolume == "" {
		return ErrNetworkNeedsAgent
	}
	if len(dns) == 0 {
		dns = []string{DefaultDNS}
	}
	for _, s := range dns {
		addr, err := netip.ParseAddr(s)
		if err != nil || !addr.Is4() {
			return fmt.Errorf("sandbox DNS resolver %q is not an IPv4 address", s)
		}
	}
	d.networkDNS = dns
	return nil
}

// networked reports whether a request gets a network. A gVisor sandbox does not:
// its network stack does not pass through the firewall of the shared namespace,
// so the guard could not enforce anything.
func (d *Driver) networked(req runtime.CreateRequest) bool {
	return len(d.networkDNS) > 0 && normalizeRuntime(req.OCIRuntime) == ""
}

// guardSpec is what the guard container of one sandbox needs.
type guardSpec struct {
	SandboxID       string
	TenantID        string
	Image           string
	AgentVolume     string
	Instance        string
	AppArmorProfile string
	DNS             []string
	AllowOut        []string
	DenyOut         []string
}

// guardConfig builds the guard container. It is the only container of a sandbox
// with a capability, CAP_NET_ADMIN, and it runs no tenant code: its one process
// is the agent binary in netguard mode, on a read-only root filesystem.
func guardConfig(spec guardSpec) (*dcontainer.Config, *dcontainer.HostConfig) {
	cfg := &dcontainer.Config{
		Image: spec.Image,
		User:  "0:0", // setting the socket mark and the firewall needs CAP_NET_ADMIN in the user namespace
		Cmd:   []string{agentPath, "netguard"},
		Env: []string{
			netguard.EnvAllowOut + "=" + strings.Join(spec.AllowOut, ","),
			netguard.EnvDenyOut + "=" + strings.Join(spec.DenyOut, ","),
			netguard.EnvDNS + "=" + strings.Join(spec.DNS, ","),
		},
		Labels: map[string]string{
			LabelManaged:   "true",
			LabelSandboxID: spec.SandboxID,
			LabelTenantID:  spec.TenantID,
			LabelRole:      roleNetguard,
		},
	}
	if spec.Instance != "" {
		cfg.Labels[LabelInstance] = spec.Instance
	}
	secOpt := []string{"no-new-privileges:true"}
	if spec.AppArmorProfile != "" {
		secOpt = append(secOpt, "apparmor="+spec.AppArmorProfile)
	}
	pids := int64(64)
	host := &dcontainer.HostConfig{
		NetworkMode:    "bridge",
		DNS:            spec.DNS,
		IpcMode:        "private",
		ReadonlyRootfs: true,
		SecurityOpt:    secOpt,
		CapDrop:        strslice.StrSlice{"ALL"},
		CapAdd:         strslice.StrSlice{"NET_ADMIN"},
		Mounts:         []mount.Mount{{Type: mount.TypeVolume, Source: spec.AgentVolume, Target: agentDir, ReadOnly: true}},
		Resources: dcontainer.Resources{
			NanoCPUs:  500_000_000,
			Memory:    64 << 20,
			PidsLimit: &pids,
		},
	}
	return cfg, host
}

// startGuard creates the guard of a sandbox and returns its container ID once
// the firewall is installed. The sandbox must not start before that: until the
// rules exist, the shared namespace has an open network.
func (d *Driver) startGuard(ctx context.Context, req runtime.CreateRequest, image string) (string, error) {
	if _, err := egress.ParseRules(req.AllowOut, req.DenyOut); err != nil {
		return "", fmt.Errorf("%w: %w", runtime.ErrInvalidRequest, err)
	}
	cfg, host := guardConfig(guardSpec{
		SandboxID:       string(req.SandboxID),
		TenantID:        req.TenantID,
		Image:           image,
		AgentVolume:     d.agentVolume,
		Instance:        d.instance,
		AppArmorProfile: d.apparmor,
		DNS:             d.networkDNS,
		AllowOut:        req.AllowOut,
		DenyOut:         req.DenyOut,
	})
	created, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, guardName(req.SandboxID))
	if err != nil {
		return "", fmt.Errorf("network guard create: %w", err)
	}
	remove := func() {
		_ = d.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, dcontainer.RemoveOptions{Force: true})
	}
	if err := d.cli.ContainerStart(ctx, created.ID, dcontainer.StartOptions{}); err != nil {
		remove()
		return "", fmt.Errorf("network guard start: %w", err)
	}
	if err := d.waitGuardReady(ctx, created.ID); err != nil {
		remove()
		return "", err
	}
	return created.ID, nil
}

func (d *Driver) waitGuardReady(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, guardReadyFor)
	defer cancel()
	for {
		logs, running, err := d.guardLogs(ctx, id)
		if err != nil {
			return err
		}
		if strings.Contains(logs, netguard.ReadyLine) {
			return nil
		}
		if !running {
			return fmt.Errorf("network guard exited before it was ready: %s", strings.TrimSpace(logs))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("network guard not ready after %s: %s", guardReadyFor, strings.TrimSpace(logs))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (d *Driver) guardLogs(ctx context.Context, id string) (logs string, running bool, err error) {
	info, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return "", false, fmt.Errorf("network guard inspect: %w", err)
	}
	rc, err := d.cli.ContainerLogs(ctx, id, dcontainer.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", false, fmt.Errorf("network guard logs: %w", err)
	}
	defer func() { _ = rc.Close() }()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, rc); err != nil {
		return "", false, fmt.Errorf("network guard logs: %w", err)
	}
	return out.String(), info.State != nil && info.State.Running, nil
}

// removeGuard removes the guard of a sandbox. A sandbox without one is not an error.
func (d *Driver) removeGuard(ctx context.Context, id runtime.SandboxID) error {
	err := d.cli.ContainerRemove(ctx, guardName(id), dcontainer.RemoveOptions{Force: true})
	if err == nil || errdefs.IsNotFound(err) {
		return nil
	}
	return fmt.Errorf("network guard remove: %w", err)
}
