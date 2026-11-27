package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// namePrefix is the container name prefix for managed sandboxes.
const namePrefix = "lsb-sbx-"

// ErrImageNotAllowed is returned when a create request names an image outside the
// driver's allow-list. Untrusted tenants must not pull arbitrary images onto the
// node (supply-chain + DoS boundary). It wraps runtime.ErrInvalidRequest so the
// application layer maps it to a client error (4xx), not a 500.
var ErrImageNotAllowed = fmt.Errorf("%w: image not allowed", runtime.ErrInvalidRequest)

// ErrHostNotHardened is returned when the Docker daemon's default seccomp profile
// is disabled — the driver fails closed rather than run untrusted code unconfined.
var ErrHostNotHardened = errors.New("docker daemon seccomp default is unconfined/absent")

// Default resource bounds applied to every sandbox when the request omits them,
// so no sandbox is ever unbounded (deny by default, limits always applied).
const (
	defaultCPUNanos    = 1_000_000_000 // 1 core
	defaultMemoryBytes = 512 << 20     // 512 MiB
	defaultPidsLimit   = 128
)

// Driver implements runtime.Driver by creating hardened Docker containers on the
// node. It is the M2 default runtime; gVisor is selected per-request via OCIRuntime.
type Driver struct {
	cli          *client.Client
	defaultImage string
	apparmor     string          // "docker-default" when the host supports AppArmor, else ""
	allowed      map[string]bool // image allow-list (defaultImage always permitted)
	agentVolume  string          // volume holding lsb-agent; "" when sandboxes run without the agent
	instance     string          // the deployment this driver belongs to; "" when not set
	networkDNS   []string        // resolvers of a sandbox; empty when sandboxes have no network
}

var _ runtime.InstanceScoped = (*Driver)(nil)

// SetInstance names the deployment the driver belongs to. Containers created
// afterwards carry it as a label, and ListOrphans returns only those that do.
// Call it before the driver is used.
func (d *Driver) SetInstance(id string) { d.instance = id }

// New returns a container Driver over a Docker client. defaultImage is used when a
// create request carries no image. apparmor is the AppArmor profile to apply (or
// "" when unavailable). allowedImages restricts which images may be pulled/run.
func New(cli *client.Client, defaultImage, apparmor string, allowedImages []string) *Driver {
	if defaultImage == "" {
		defaultImage = "busybox:latest"
	}
	allowed := map[string]bool{defaultImage: true}
	for _, img := range allowedImages {
		if img != "" {
			allowed[img] = true
		}
	}
	return &Driver{cli: cli, defaultImage: defaultImage, apparmor: apparmor, allowed: allowed}
}

// withDefaults fills unset resource bounds so every sandbox is limited.
func withDefaults(req runtime.CreateRequest) runtime.CreateRequest {
	if req.CPUNanos == 0 {
		req.CPUNanos = defaultCPUNanos
	}
	if req.MemoryBytes == 0 {
		req.MemoryBytes = defaultMemoryBytes
	}
	if req.PidsLimit == 0 {
		req.PidsLimit = defaultPidsLimit
	}
	return req
}

// NewFromEnv builds a Driver from the ambient Docker environment and verifies the
// host is hardened (fail-closed on unconfined seccomp), detecting AppArmor support.
func NewFromEnv(defaultImage string, allowedImages ...string) (*Driver, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	apparmor, err := verifyHost(context.Background(), cli)
	if err != nil {
		return nil, err
	}
	d := New(cli, defaultImage, apparmor, allowedImages)
	if bin := os.Getenv(AgentBinEnv); bin != "" {
		if err := d.EnableAgent(context.Background(), bin); err != nil {
			return nil, fmt.Errorf("%s=%s: %w", AgentBinEnv, bin, err)
		}
	}
	if v := os.Getenv(NetworkEnv); v != "" {
		var dns []string
		if v != "on" {
			dns = strings.Split(v, ",")
		}
		if err := d.EnableNetwork(dns...); err != nil {
			return nil, fmt.Errorf("%s=%s: %w", NetworkEnv, v, err)
		}
	}
	return d, nil
}

// verifyHost fails closed if the daemon's default seccomp profile is unconfined/
// absent, and reports whether AppArmor is available so the profile can pin it.
func verifyHost(ctx context.Context, cli *client.Client) (apparmor string, err error) {
	info, err := cli.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("docker info: %w", err)
	}
	seccompOK, hasAppArmor := false, false
	for _, opt := range info.SecurityOptions {
		// Format: "name=seccomp,profile=builtin", "name=apparmor", ...
		if strings.Contains(opt, "name=seccomp") && !strings.Contains(opt, "profile=unconfined") {
			seccompOK = true
		}
		if strings.Contains(opt, "name=apparmor") {
			hasAppArmor = true
		}
	}
	if !seccompOK {
		return "", ErrHostNotHardened
	}
	if hasAppArmor {
		return "docker-default", nil
	}
	return "", nil
}

// Name implements runtime.Driver.
func (d *Driver) Name() string { return "container" }

func containerName(id runtime.SandboxID) string { return namePrefix + string(id) }

// Create pulls the image if needed, then creates and starts a hardened container.
func (d *Driver) Create(ctx context.Context, req runtime.CreateRequest) (*runtime.Sandbox, error) {
	req = withDefaults(req)
	img := req.Image
	if img == "" {
		img = d.defaultImage
	}
	if !d.allowed[img] {
		return nil, fmt.Errorf("%w: %q", ErrImageNotAllowed, img)
	}
	if err := d.ensureImage(ctx, img); err != nil {
		return nil, err
	}
	guardID := ""
	if d.networked(req) {
		id, err := d.startGuard(ctx, req, d.defaultImage)
		if err != nil {
			return nil, err
		}
		guardID = id
	}
	// Cleanup with a cancel-immune context so teardown survives a cancelled create.
	dropGuard := func() {
		if guardID != "" {
			_ = d.removeGuard(context.WithoutCancel(ctx), req.SandboxID)
		}
	}
	cfg, host := hardenedConfig(Spec{
		NetworkContainer: guardID,
		SandboxID:        string(req.SandboxID),
		TenantID:         req.TenantID,
		Image:            img,
		Runtime:          req.OCIRuntime,
		AppArmorProfile:  d.apparmor,
		CPUNanos:         req.CPUNanos,
		MemoryBytes:      req.MemoryBytes,
		PidsLimit:        req.PidsLimit,
		GPU:              req.GPU,
		AgentVolume:      d.agentVolume,
		Instance:         d.instance,
	})
	created, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, containerName(req.SandboxID))
	if err != nil {
		dropGuard()
		return nil, fmt.Errorf("container create: %w", err)
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = d.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
		dropGuard()
		return nil, fmt.Errorf("container start: %w", err)
	}
	return &runtime.Sandbox{ID: req.SandboxID, State: runtime.StateRunning}, nil
}

// Inspect maps the container state to the runtime state. An unknown container is
// reported as destroyed (nothing running).
func (d *Driver) Inspect(ctx context.Context, id runtime.SandboxID) (*runtime.Sandbox, error) {
	info, err := d.cli.ContainerInspect(ctx, containerName(id))
	if errdefs.IsNotFound(err) {
		return &runtime.Sandbox{ID: id, State: runtime.StateDestroyed}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("container inspect: %w", err)
	}
	st := runtime.StatePending
	if info.State != nil && info.State.Running {
		st = runtime.StateRunning
	} else if info.State != nil && (info.State.Dead || info.State.Status == "exited") {
		st = runtime.StateDestroyed
	}
	return &runtime.Sandbox{ID: id, State: st}, nil
}

// Destroy stops and removes the container. It is idempotent: an unknown container
// is not an error (desired end state reached).
func (d *Driver) Destroy(ctx context.Context, id runtime.SandboxID) error {
	// The sandbox goes first: the guard is what keeps its network closed.
	err := d.cli.ContainerRemove(ctx, containerName(id), container.RemoveOptions{Force: true})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("container remove: %w", err)
	}
	return d.removeGuard(ctx, id)
}

// ListOrphans returns managed sandbox containers still present on the node — used
// by the reconciler (M4) to reap sandboxes with no live record. With an instance
// set, only the containers of that deployment are returned: another deployment
// on the same Docker host has its own records, and reaping by ours would destroy
// its live sandboxes.
func (d *Driver) ListOrphans(ctx context.Context) ([]runtime.Sandbox, error) {
	f := filters.NewArgs(filters.Arg("label", LabelManaged+"=true"))
	if d.instance != "" {
		f.Add("label", LabelInstance+"="+d.instance)
	}
	list, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("container list: %w", err)
	}
	return orphansFrom(list), nil
}

// orphansFrom turns the managed containers into one entry per sandbox. A sandbox
// with a network has two containers; its state is the sandbox container's. A
// guard whose sandbox container is gone is reported as destroyed, so the
// reconciler removes it.
func orphansFrom(list []container.Summary) []runtime.Sandbox {
	out := make([]runtime.Sandbox, 0, len(list))
	index := map[runtime.SandboxID]int{}
	for _, c := range list {
		id := runtime.SandboxID(c.Labels[LabelSandboxID])
		guard := c.Labels[LabelRole] == roleNetguard
		state := runtime.StateRunning
		if guard || c.State != "running" {
			state = runtime.StateDestroyed
		}
		if i, seen := index[id]; seen {
			if !guard {
				out[i].State = state
			}
			continue
		}
		index[id] = len(out)
		out = append(out, runtime.Sandbox{ID: id, TenantID: c.Labels[LabelTenantID], State: state})
	}
	return out
}

// ensureImage pulls the image if it is not already present locally.
func (d *Driver) ensureImage(ctx context.Context, ref string) error {
	if _, err := d.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("image inspect: %w", err)
	}
	rc, err := d.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image pull: %w", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("image pull drain: %w", err)
	}
	return nil
}

var _ runtime.Driver = (*Driver)(nil)
