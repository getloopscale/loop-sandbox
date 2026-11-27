// Package container implements runtime.Driver on-node via the Docker Engine API,
// creating hardened containers for untrusted code per TRD §10.3. gVisor (runsc)
// is selected via HostConfig.Runtime when available; the default is hardened runc.
package container

import (
	"strconv"

	dcontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	units "github.com/docker/go-units"
)

// Labels applied to every managed container (orphan reconciliation).
const (
	LabelManaged   = "loopsandbox.managed"
	LabelSandboxID = "loopsandbox.sandbox_id"
	LabelTenantID  = "loopsandbox.tenant_id"
	// LabelInstance names the deployment that created the container. The orphan
	// reaper of one deployment leaves the containers of another alone.
	LabelInstance = "loopsandbox.instance"
)

// maxTmpfsBytes clamps caller-requested tmpfs size. tmpfs pages come from HOST
// RAM and are not reliably charged to the container's memory cgroup across
// kernels, so an unbounded tmpfs is a host-memory exhaustion vector.
const maxTmpfsBytes = 512 << 20 // 512 MiB per mount

// Spec is the hardened-container request (a subset of runtime.CreateRequest plus
// resource bounds and host-capability info the driver applies).
type Spec struct {
	SandboxID       string
	TenantID        string
	Image           string
	Runtime         string // "" or "runc" → default; "runsc" → gVisor
	AppArmorProfile string // e.g. "docker-default"; "" when the host lacks AppArmor
	CPUNanos        int64
	MemoryBytes     int64
	PidsLimit       int64
	TmpfsSizeBytes  int64  // per tmpfs mount; 0 → 64Mi default; clamped to maxTmpfsBytes
	GPU             string // M19: non-empty → request NVIDIA GPU passthrough (--gpus all)
	AgentVolume     string // non-empty → the sandbox runs lsb-agent from this volume
	Instance        string // non-empty → the deployment the container belongs to
}

// hardenedConfig builds the container + host config implementing TRD §10.3:
// non-root, no-new-privileges, all caps dropped, AppArmor (when available) + the
// daemon's default seccomp profile (enforced at startup by Driver.verifyHost),
// read-only rootfs, tmpfs for /tmp (noexec) and /workspace (exec-permitted — it
// holds user code by design), deny-all network, private IPC namespace, cgroup CPU/
// memory limits, pids-limit + ulimits, no privileged mode, no host namespaces.
//
// Pure function → the hardened profile is unit-testable without Docker.
func hardenedConfig(spec Spec) (*dcontainer.Config, *dcontainer.HostConfig) {
	tmpfsSize := spec.TmpfsSizeBytes
	if tmpfsSize <= 0 {
		tmpfsSize = 64 << 20 // 64 MiB
	}
	if tmpfsSize > maxTmpfsBytes {
		tmpfsSize = maxTmpfsBytes
	}
	// /tmp: no execution ever. /workspace: holds user-supplied code, so it must
	// permit exec — isolation comes from the container/microVM boundary, not from
	// noexec on the very directory meant to run code (ADR: workspace-exec).
	tmpOpts := "rw,noexec,nosuid,nodev,size=" + strconv.FormatInt(tmpfsSize, 10)
	wsSize := tmpfsSize * 4
	if wsSize > maxTmpfsBytes {
		wsSize = maxTmpfsBytes
	}
	wsOpts := "rw,nosuid,nodev,size=" + strconv.FormatInt(wsSize, 10)

	cfg := &dcontainer.Config{
		Image:           spec.Image,
		User:            "65534:65534", // nobody:nogroup — non-root
		Cmd:             []string{"sleep", "infinity"},
		NetworkDisabled: true,
		Labels: map[string]string{
			LabelManaged:   "true",
			LabelSandboxID: spec.SandboxID,
			LabelTenantID:  spec.TenantID,
		},
	}
	if spec.Instance != "" {
		cfg.Labels[LabelInstance] = spec.Instance
	}

	secOpt := []string{"no-new-privileges:true"}
	if spec.AppArmorProfile != "" {
		secOpt = append(secOpt, "apparmor="+spec.AppArmorProfile)
	}
	// seccomp is intentionally NOT set to unconfined: omitting a seccomp entry
	// makes Docker apply its built-in default profile. Driver.verifyHost fails
	// closed at startup if the daemon's default is unconfined/absent.

	pids := spec.PidsLimit
	host := &dcontainer.HostConfig{
		Runtime:        normalizeRuntime(spec.Runtime),
		NetworkMode:    "none", // authoritative deny-all egress (NetworkDisabled is belt-and-suspenders)
		IpcMode:        "private",
		ReadonlyRootfs: true,
		Privileged:     false,
		SecurityOpt:    secOpt,
		CapDrop:        strslice.StrSlice{"ALL"},
		Tmpfs:          map[string]string{"/tmp": tmpOpts, "/workspace": wsOpts},
		Resources: dcontainer.Resources{
			NanoCPUs: spec.CPUNanos,
			Memory:   spec.MemoryBytes,
			Ulimits: []*units.Ulimit{
				{Name: "nofile", Soft: 1024, Hard: 4096},
				{Name: "nproc", Soft: 256, Hard: 512},
			},
		},
	}
	if pids > 0 {
		host.PidsLimit = &pids
	}
	if spec.AgentVolume != "" {
		// The agent is the sandbox's first process: an init that reaps orphans and a
		// worker that serves the host on a unix socket. Its binary is read-only, and
		// the socket lives in a small tmpfs only the sandbox user can enter.
		cfg.Cmd = []string{agentPath, "daemon"}
		host.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: spec.AgentVolume, Target: agentDir, ReadOnly: true}}
		host.Tmpfs[agentRunDir] = "rw,noexec,nosuid,nodev,size=1m,mode=0700,uid=65534,gid=65534"
	}
	// M19: GPU passthrough. The NVIDIA container runtime injects the requested GPUs; Count -1 =
	// all visible GPUs on the (isolated) GPU node. GPU sandboxes are scheduled onto a dedicated
	// GPU cell so this passthrough never shares a node with a CPU sandbox (isolation risk #1).
	if spec.GPU != "" {
		// Count 1 = ONE GPU per sandbox (never grab all GPUs on a multi-GPU node — that would
		// let one GPU sandbox monopolize the node and starve other paying GPU tenants, M19 review).
		host.DeviceRequests = []dcontainer.DeviceRequest{
			{Driver: "nvidia", Count: 1, Capabilities: [][]string{{"gpu"}}},
		}
	}
	return cfg, host
}

// normalizeRuntime maps "" → "" (Docker default runc) and passes through "runsc"
// (gVisor) or any explicitly configured OCI runtime.
func normalizeRuntime(r string) string {
	if r == "" || r == "runc" {
		return ""
	}
	return r
}
