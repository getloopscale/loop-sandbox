// Package app holds the application use cases (orchestration). It depends on the
// domain ports (Repository, runtime.Driver, AuditRecorder, CostStore) and the
// clock seam — never on concrete infrastructure.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/plan"

	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/secretbox"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// IDGen generates opaque sandbox identifiers (injected for determinism in tests).
type IDGen interface{ New() string }

// Actor is the authenticated caller context for a use case (from auth.Principal).
type Actor struct {
	TenantID  string
	KeyID     string
	RequestID string
	// Scopes are the scopes of the credential the actor presented. They bound the
	// scopes of an API key the actor creates.
	Scopes []string
}

// AuditRecorder records a mutation asynchronously (implemented by audit.Sink).
type AuditRecorder interface {
	Record(tenantID, actorID, action, resourceType, resourceID, requestID string, now time.Time)
}

// Observer records runtime metrics off the hot path (implemented by
// metrics.Prometheus). It is a port — the app layer never imports Prometheus (DIP).
type Observer interface {
	SandboxCreated(d time.Duration)
	SandboxDestroyed()
	ExecObserved(result string)
	QuotaDenied(metric string)
	QuotaFailOpen(metric string) // a quota check that could not run and allowed (fail-open)
	CellRouted(cell string)      // a tenant was routed to a cell (M11 sharding)
	ArtifactURLIssued()          // a pre-signed artifact URL was minted (M11 FileTransport)
}

// noopObserver is the default so service code can call metrics unconditionally.
type noopObserver struct{}

func (noopObserver) SandboxCreated(time.Duration) {}
func (noopObserver) SandboxDestroyed()            {}
func (noopObserver) ExecObserved(string)          {}
func (noopObserver) QuotaDenied(string)           {}
func (noopObserver) QuotaFailOpen(string)         {}
func (noopObserver) CellRouted(string)            {}
func (noopObserver) ArtifactURLIssued()           {}

// PlanResolver returns a tenant's subscription plan for quota enforcement.
type PlanResolver interface {
	PlanFor(ctx context.Context, tenantID string) (plan.Plan, error)
}

// QuotaEnforcer checks whether charging a metric would exceed the tenant's plan
// cap (implemented by enforcement.Enforcer). Returns a *plan.ExceededError → 402.
type QuotaEnforcer interface {
	Check(ctx context.Context, tenantID string, tier plan.Plan, metric plan.CostMetric, amount int64, now time.Time) error
}

// CellRouter maps a tenant to its cell (implemented by cells.Router). Kept as a
// one-method port so the app layer does not import the hashing package (ISP + DIP).
type CellRouter interface {
	CellFor(tenantID string) string
	// GPUCell returns the isolated GPU cell (M19). GPU sandboxes are pinned here so device
	// passthrough never shares a cell with CPU sandboxes (isolation risk #1).
	GPUCell() string
}

// ArtifactStore mints a pre-signed download URL for a tenant's sandbox artifact
// (implemented by filestore.S3). The app never assembles raw storage keys — the store
// owns key construction (ISP: app only needs the presign verb, not Put).
type ArtifactStore interface {
	PresignArtifact(ctx context.Context, tenantID, sandboxID, path string, ttl time.Duration) (string, error)
}

// ErrArtifactStoreUnset is returned when a presign is requested but no object store is
// wired (the FileTransport is an optional deployment feature).
var ErrArtifactStoreUnset = errors.New("artifact store not configured")

// Service is the sandbox use-case orchestrator.
type Service struct {
	repo      domain.Repository
	driver    runtime.Driver
	clock     clock.Clock
	ids       IDGen
	audit     AuditRecorder
	cost      domain.CostStore
	execs     domain.ExecutionStore // optional (nil-safe): persists executions
	obs       Observer
	quota     QuotaEnforcer        // optional (nil-safe): plan cap enforcement
	plans     PlanResolver         // optional (nil-safe): tenant → plan lookup
	router    CellRouter           // optional (nil-safe): tenant → cell routing (M11)
	files     ArtifactStore        // optional (nil-safe): pre-signed artifact URLs (M11)
	vols      domain.VolumeStore   // optional (nil-safe): durable volumes (M16)
	snaps     domain.SnapshotStore // optional (nil-safe): named snapshots
	secrets   domain.SecretStore   // optional (nil-safe): tenant secrets
	secretBox *secretbox.Box       // encrypts the secrets; nil when secrets is
	tmpls     domain.TemplateStore // optional (nil-safe): template registry (M17)
	scan      TemplateScanner      // optional (nil-safe): template vuln scan gate (M17)
	hooks     WebhookSink          // optional (nil-safe): lifecycle webhooks (M20)
	browsers  browserSessions      // per-sandbox computer-use browser sessions (M18)
	// locks serializes mutating lifecycle ops that read-modify-write shared state on the same
	// key (a sandbox id, or a tenant:volume-name). The domain state machine validates on a
	// COPY, so without this two concurrent pause/resume/destroy (or same-name volume creates)
	// would both pass validation and race to a lost update / zombie state (M16 review).
	keys  domain.APIKeyStore // optional (nil-safe): API key management
	locks keyedMutex
	// using counts the requests in flight per sandbox with an idle limit.
	using inUse
}

// keyedMutex is a set of mutexes keyed by an arbitrary string, so distinct keys never
// contend. Entries are retained for the process lifetime (a mutex is tiny); the key space is
// bounded by live sandboxes/volume-names in practice.
type keyedMutex struct{ m sync.Map }

func (k *keyedMutex) lock(key string) func() {
	mi, _ := k.m.LoadOrStore(key, &sync.Mutex{})
	mu := mi.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// NewService wires the use case. All dependencies are ports (DIP). execs may be nil.
func NewService(repo domain.Repository, driver runtime.Driver, clk clock.Clock, ids IDGen, audit AuditRecorder, cost domain.CostStore, execs domain.ExecutionStore) *Service {
	return &Service{repo: repo, driver: driver, clock: clk, ids: ids, audit: audit, cost: cost, execs: execs, obs: noopObserver{}}
}

// SetObserver installs a metrics observer (composition root only). Nil restores
// the no-op default so the service never panics on an unset observer.
func (s *Service) SetObserver(o Observer) {
	if o == nil {
		o = noopObserver{}
	}
	s.obs = o
}

// SetEnforcer installs plan-quota enforcement (composition root only). Both must be
// non-nil to enforce; otherwise the service runs unmetered (M1 behaviour).
func (s *Service) SetEnforcer(q QuotaEnforcer, plans PlanResolver) {
	s.quota = q
	s.plans = plans
}

// SetCellRouter installs tenant→cell routing (composition root only). When set,
// CreateSandbox records the routed cell on the span + a metric (M11 sharding).
func (s *Service) SetCellRouter(r CellRouter) { s.router = r }

// SetArtifactStore installs the object-storage FileTransport (composition root only).
// When set, PresignArtifactURL mints pre-signed download URLs (M11 B6).
func (s *Service) SetArtifactStore(a ArtifactStore) { s.files = a }

// SetVolumeStore installs the durable-volume store (composition root only). When set,
// the volume use cases (CreateVolume/ListVolumes/DeleteVolume) are enabled (M16).
func (s *Service) SetVolumeStore(v domain.VolumeStore) { s.vols = v }

// SetTemplateRegistry installs the template registry store and (optional) vuln scanner
// (composition root only). When set, the template use cases (M17) are enabled.
func (s *Service) SetTemplateRegistry(t domain.TemplateStore, scanner TemplateScanner) {
	s.tmpls = t
	s.scan = scanner
}

// PresignArtifactURL returns a short-lived pre-signed URL for a sandbox artifact, after
// verifying the caller owns the sandbox (cross-tenant access returns not-found, never a
// URL). Returns ErrArtifactStoreUnset when the FileTransport is not wired.
func (s *Service) PresignArtifactURL(ctx context.Context, actor Actor, sandboxID, path string, ttl time.Duration) (string, error) {
	if s.files == nil {
		return "", ErrArtifactStoreUnset
	}
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return "", err
	}
	clean, err := domain.ValidateSandboxPath(path)
	if err != nil {
		return "", err
	}
	url, err := s.files.PresignArtifact(ctx, actor.TenantID, sandboxID, clean, ttl)
	if err != nil {
		return "", err
	}
	s.obs.ArtifactURLIssued()
	return url, nil
}

// enforce checks a plan cap for the metric, returning a *plan.ExceededError (→ 402)
// on overflow. No-op when enforcement is not wired.
//
// Soft-cap semantics (accepted, documented): the check reads a (possibly cached)
// usage counter and is NOT atomic with the later increment, so N concurrent requests
// at cap-1 can all pass — worst-case overshoot ≈ in-flight concurrency + one cache
// TTL window. Plan caps are best-effort cost control, not a hard security boundary.
// A plan-lookup or usage-read error FAILS OPEN (never 402s a legit request on infra
// trouble) but is now observable via QuotaFailOpen so ops can alert on a spike.
func (s *Service) enforce(ctx context.Context, tenantID string, metric plan.CostMetric, amount int64, now time.Time) error {
	if s.quota == nil || s.plans == nil {
		return nil
	}
	tier, err := s.plans.PlanFor(ctx, tenantID)
	if err != nil {
		s.obs.QuotaFailOpen(string(metric)) // fail-open, but loud
		return nil
	}
	if err := s.quota.Check(ctx, tenantID, tier, metric, amount, now); err != nil {
		s.obs.QuotaDenied(string(metric))
		return err
	}
	return nil
}

// ownedSandbox returns the sandbox iff it belongs to the actor's tenant, else
// ErrNotFound — so exec/file ops on another tenant's sandbox are indistinguishable
// from a missing one (US-05).
func (s *Service) ownedSandbox(ctx context.Context, actor Actor, id string) (*domain.Sandbox, error) {
	return s.repo.Get(ctx, actor.TenantID, id)
}

// Exec runs a command in the actor's sandbox, records an Execution and an audit
// event. A non-zero exit is a normal result (not an error) — the caller reports it.
func (s *Service) Exec(ctx context.Context, actor Actor, sandboxID string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	ctx, end := tracing.Start(ctx, "sandbox.exec", actor.TenantID, sandboxID)
	defer end()
	sb, err := s.useSandbox(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	if err := domain.ValidateEnv(req.Env); err != nil {
		return nil, err
	}
	if req, err = s.withCommandEnv(ctx, sb, req); err != nil {
		return nil, err
	}
	if err := s.enforce(ctx, actor.TenantID, enforcement.MetricSandboxExecutions, 1, s.clock.Now()); err != nil {
		return nil, err
	}
	// A client-supplied working dir is a path at the trust boundary — confine it to
	// /workspace like every other file path (defense-in-depth, not just the runtime).
	if req.WorkingDir != "" {
		clean, err := domain.ValidateSandboxPath(req.WorkingDir)
		if err != nil {
			return nil, err
		}
		req.WorkingDir = clean
	}
	now := s.clock.Now()
	execID := "exe_" + s.ids.New()
	if s.execs != nil {
		if ex, err := domain.NewExecution(execID, sandboxID, actor.TenantID, now); err == nil {
			_ = s.execs.SaveExecution(ctx, ex)
		}
	}
	xctx, xend := tracing.Child(ctx, "runtime.exec")
	res, err := s.driver.Exec(xctx, runtime.SandboxID(sandboxID), req)
	xend()
	if err != nil {
		if errors.Is(err, runtime.ErrTimeout) {
			s.obs.ExecObserved("timeout")
		} else {
			s.obs.ExecObserved("error")
		}
		return nil, s.mapRuntimeErr(err)
	}
	// US-02: a non-zero exit is the user's program failing, NOT a platform error.
	// Label it distinctly so alerting on result="error" (driver/runtime failure) is
	// not polluted by e.g. a `grep` that legitimately exits 1.
	if res.ExitCode == 0 {
		s.obs.ExecObserved("ok")
	} else {
		s.obs.ExecObserved("nonzero_exit")
	}
	if s.execs != nil {
		if ex, e := domain.NewExecution(execID, sandboxID, actor.TenantID, now); e == nil {
			ex.Status = "completed"
			_ = s.execs.SaveExecution(ctx, ex)
		}
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.exec", "sandbox", sandboxID, actor.RequestID, now)
	s.emitWebhook(ctx, "sandbox.exec", actor.TenantID, sandboxID)
	return res, nil
}

// WriteFile validates the path and writes to the actor's sandbox.
func (s *Service) WriteFile(ctx context.Context, actor Actor, sandboxID, filePath string, r io.Reader) error {
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return err
	}
	clean, err := domain.ValidateSandboxPath(filePath)
	if err != nil {
		return err
	}
	if err := s.driver.WriteFile(ctx, runtime.SandboxID(sandboxID), clean, r); err != nil {
		return s.mapRuntimeErr(err)
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.file.write", "sandbox", sandboxID, actor.RequestID, s.clock.Now())
	return nil
}

// ReadFile validates the path and reads from the actor's sandbox.
func (s *Service) ReadFile(ctx context.Context, actor Actor, sandboxID, filePath string) (io.ReadCloser, error) {
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return nil, err
	}
	clean, err := domain.ValidateSandboxPath(filePath)
	if err != nil {
		return nil, err
	}
	rc, err := s.driver.ReadFile(ctx, runtime.SandboxID(sandboxID), clean)
	if err != nil {
		return nil, s.mapRuntimeErr(err)
	}
	return rc, nil
}

// DeleteFile validates the path and removes it from the actor's sandbox.
func (s *Service) DeleteFile(ctx context.Context, actor Actor, sandboxID, filePath string) error {
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return err
	}
	clean, err := domain.ValidateSandboxPath(filePath)
	if err != nil {
		return err
	}
	if err := s.driver.DeleteFile(ctx, runtime.SandboxID(sandboxID), clean); err != nil {
		return s.mapRuntimeErr(err)
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.file.delete", "sandbox", sandboxID, actor.RequestID, s.clock.Now())
	return nil
}

// ListFiles validates the path and lists it in the actor's sandbox.
func (s *Service) ListFiles(ctx context.Context, actor Actor, sandboxID, dirPath string) ([]runtime.FileEntry, error) {
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return nil, err
	}
	clean, err := domain.ValidateSandboxPath(dirPath)
	if err != nil {
		return nil, err
	}
	entries, err := s.driver.ListFiles(ctx, runtime.SandboxID(sandboxID), clean)
	if err != nil {
		return nil, s.mapRuntimeErr(err)
	}
	return entries, nil
}

// mapRuntimeErr translates driver sentinels into domain errors for the API layer.
func (s *Service) mapRuntimeErr(err error) error {
	switch {
	case errors.Is(err, runtime.ErrFileNotFound), errors.Is(err, runtime.ErrSandboxNotFound):
		return fmt.Errorf("%w: %v", domain.ErrNotFound, err)
	case errors.Is(err, runtime.ErrInvalidRequest):
		return fmt.Errorf("%w: %v", domain.ErrInvalidArgument, err)
	case errors.Is(err, runtime.ErrTimeout):
		return fmt.Errorf("%w: %v", domain.ErrTimeout, err)
	case errors.Is(err, runtime.ErrFileExists):
		return fmt.Errorf("%w: %v", domain.ErrAlreadyExists, err)
	case errors.Is(err, runtime.ErrVolumeConflict):
		return fmt.Errorf("%w: %v", domain.ErrInvalidTransition, err)
	default:
		return err
	}
}

// CreateSandbox creates a sandbox for the actor's tenant: persist (pending) →
// driver.Create → transition running → persist. On driver failure it compensates
// (deletes the pending row) so the DB never diverges from reality. On success it
// meters the creation and records an audit event (both off the hot path / async).
// DefaultTTL / MaxTTL bound a sandbox's mandatory lifetime (TRD §12 security).
const (
	DefaultTTL = 5 * time.Minute
	MaxTTL     = time.Hour
)

// saveTraced persists a sandbox inside a child "db.sandbox.save" span so the storage
// boundary shows up in the request's trace tree (M11 full-chain observability).
func (s *Service) saveTraced(ctx context.Context, sb *domain.Sandbox, wrap string) error {
	sctx, end := tracing.Child(ctx, "db.sandbox.save")
	err := s.repo.Save(sctx, sb)
	end()
	if err != nil {
		return fmt.Errorf("%s: %w", wrap, err)
	}
	return nil
}

// CreateSandbox provisions a sandbox from an image ref.
func (s *Service) CreateSandbox(ctx context.Context, actor Actor, image string, ttl time.Duration) (*domain.Sandbox, error) {
	return s.createSandbox(ctx, actor, image, "", "", ttl, sandboxAttributes{})
}

// CreateSandboxGPU provisions a GPU-backed sandbox (M19): scheduled onto a GPU cell, metered on
// the premium GPU tier, and passed to the driver with GPU passthrough requested.
func (s *Service) CreateSandboxGPU(ctx context.Context, actor Actor, image, gpu string, ttl time.Duration) (*domain.Sandbox, error) {
	return s.createSandbox(ctx, actor, image, "", gpu, ttl, sandboxAttributes{})
}

// createSandbox is the shared create path. rootfsImage, when set (M17 template provisioning
// from a built rootfs), is threaded to the driver so a template-provisioned microVM boots the
// template's own rootfs instead of the default one. gpu, when set (M19), requests a GPU cell +
// device passthrough.
func (s *Service) createSandbox(ctx context.Context, actor Actor, image, rootfsImage, gpu string, ttl time.Duration, attrs sandboxAttributes) (*domain.Sandbox, error) {
	ctx, end := tracing.Start(ctx, "sandbox.create", actor.TenantID, "")
	defer end()
	// Route the tenant to its cell (M11 sharding). Observable now (span + metric) so ops
	// see tenant→cell distribution before per-cell worker dispatch (the NATS seam) lands.
	if s.router != nil {
		cell := s.router.CellFor(actor.TenantID)
		tracing.SetCell(ctx, cell)
		s.obs.CellRouted(cell)
	}
	now := s.clock.Now()
	// Plan-quota gate BEFORE any work: an over-cap tenant gets 402, not a sandbox.
	if err := s.enforce(ctx, actor.TenantID, enforcement.MetricSandboxCreations, 1, now); err != nil {
		return nil, err
	}
	// GPU sandboxes are a premium, strictly-capped class (M19): a separate quota gate keeps GPU
	// scarcity under control (a tenant with no GPU allowance is denied before scheduling).
	if gpu != "" {
		if err := s.enforce(ctx, actor.TenantID, enforcement.MetricGPUSandboxCreations, 1, now); err != nil {
			return nil, err
		}
	}
	sb, err := domain.NewSandbox(s.ids.New(), actor.TenantID, image, clampTTL(ttl), now)
	if err != nil {
		return nil, err
	}
	if err := sb.SetAttributes(attrs.metadata, attrs.env); err != nil {
		return nil, err
	}
	attrs.apply(sb, now)
	sb.GPU = gpu
	if s.router != nil {
		sb.CellID = s.router.CellFor(actor.TenantID) // persist the shard (B2 partition key)
		if gpu != "" {
			// GPU sandboxes are pinned to an ISOLATED GPU cell (top risk #1: keep GPU cells
			// separate from CPU cells so passthrough never weakens a CPU sandbox's boundary).
			sb.CellID = s.router.GPUCell()
		}
	}
	if err := s.saveTraced(ctx, sb, "persist pending sandbox"); err != nil {
		return nil, err
	}
	createStart := s.clock.Now()
	rctx, rend := tracing.Child(ctx, "runtime.create")
	_, err = s.driver.Create(rctx, runtime.CreateRequest{
		SandboxID:   runtime.SandboxID(sb.ID),
		TenantID:    actor.TenantID,
		Image:       image,
		RootfsImage: rootfsImage,
		GPU:         gpu,
		AllowOut:    attrs.network.AllowOut,
		DenyOut:     attrs.network.DenyOut,
	})
	rend()
	if err != nil {
		if delErr := s.repo.Delete(ctx, actor.TenantID, sb.ID); delErr != nil {
			return nil, fmt.Errorf("driver create failed (%w); compensating delete also failed: %v", err, delErr)
		}
		// A policy/validation refusal (e.g. disallowed image) is a client error.
		if errors.Is(err, runtime.ErrInvalidRequest) {
			return nil, fmt.Errorf("%w: %v", domain.ErrInvalidArgument, err)
		}
		return nil, fmt.Errorf("driver create: %w", err)
	}
	// Provisioning latency = the driver.Create span only (excludes metering/audit
	// bookkeeping) — this is the agent-critical number ops alert on.
	createLatency := s.clock.Now().Sub(createStart)
	if err := sb.TransitionTo(domain.StateRunning, now); err != nil {
		return nil, err
	}
	if err := s.saveTraced(ctx, sb, "persist running sandbox"); err != nil {
		return nil, err
	}
	s.meter(ctx, actor.TenantID, domain.MetricSandboxCreations, now)
	if gpu != "" {
		s.meter(ctx, actor.TenantID, domain.MetricGPUSandboxCreations, now)
	}
	s.obs.SandboxCreated(createLatency)
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.create", "sandbox", sb.ID, actor.RequestID, now)
	s.emitWebhook(ctx, "sandbox.created", actor.TenantID, sb.ID)
	return sb, nil
}

// GetSandbox returns the actor tenant's sandbox or domain.ErrNotFound.
func (s *Service) GetSandbox(ctx context.Context, actor Actor, id string) (*domain.Sandbox, error) {
	return s.repo.Get(ctx, actor.TenantID, id)
}

// DestroySandbox tears down and removes a tenant's sandbox (tenant-scoped, US-05).
func (s *Service) DestroySandbox(ctx context.Context, actor Actor, id string) error {
	unlock := s.locks.lock("sb:" + id)
	defer unlock()
	sb, err := s.repo.Get(ctx, actor.TenantID, id)
	if err != nil {
		return err
	}
	// Close any computer-use browser session so it does not outlive the sandbox (M18).
	s.closeBrowser(sb.ID)
	// Detach any durable volumes FIRST so their data survives destroy and they can be
	// re-attached to a new sandbox (M16). The volume records persist (detached); only the
	// ephemeral sandbox is torn down.
	s.detachSandboxVolumes(ctx, actor.TenantID, sb.ID)
	if err := s.driver.Destroy(ctx, runtime.SandboxID(sb.ID)); err != nil {
		return fmt.Errorf("driver destroy: %w", err)
	}
	if err := s.repo.Delete(ctx, actor.TenantID, id); err != nil {
		return err
	}
	s.obs.SandboxDestroyed()
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.destroy", "sandbox", id, actor.RequestID, s.clock.Now())
	s.emitWebhook(ctx, "sandbox.destroyed", actor.TenantID, id)
	return nil
}

// detachSandboxVolumes clears the sandbox binding on every volume attached to it (M16).
// Best-effort: a volume-store blip must not block sandbox teardown, but the durable data
// is never touched (detach ≠ delete). No-op when volumes are unwired.
func (s *Service) detachSandboxVolumes(ctx context.Context, tenantID, sandboxID string) {
	if s.vols == nil {
		return
	}
	attached, err := s.vols.ListVolumes(ctx, tenantID, sandboxID)
	if err != nil {
		return
	}
	for _, v := range attached {
		if vd, ok := s.driver.(runtime.VolumeDriver); ok {
			_ = vd.DetachVolume(ctx, runtime.SandboxID(sandboxID), v.ID)
		}
		v.Detach()
		_ = s.vols.SaveVolume(ctx, v)
	}
}

// meter increments a usage counter for the current billing period. Metering must
// never fail the operation (best-effort in M1; M7 wires LoopScale enforcement).
// enforceVolumeStorage checks that provisioning addGB more storage keeps the tenant within
// its plan's volume-storage cap. Unlike the period-scoped counters, storage is a LIVE gauge
// derived from the volumes table (source of truth) — so it never desyncs across billing
// periods nor goes negative (M16 review MEDIUM). No-op when enforcement is unwired.
func (s *Service) enforceVolumeStorage(ctx context.Context, tenantID string, addGB int64, now time.Time) error {
	if s.quota == nil || s.plans == nil || s.vols == nil {
		return nil
	}
	tier, err := s.plans.PlanFor(ctx, tenantID)
	if err != nil {
		s.obs.QuotaFailOpen(string(enforcement.MetricVolumeStorageGB))
		return nil
	}
	capGB := enforcement.CapFor(tier, enforcement.MetricVolumeStorageGB)
	if capGB == plan.Unlimited {
		return nil
	}
	current, err := s.vols.TotalSizeGB(ctx, tenantID)
	if err != nil {
		return nil // best-effort: a store blip must not 402 a legit request
	}
	if current+addGB > capGB {
		s.obs.QuotaDenied(string(enforcement.MetricVolumeStorageGB))
		return &plan.ExceededError{
			TenantID: tenantID, Plan: tier, Metric: enforcement.MetricVolumeStorageGB,
			Cap: capGB, Current: current, Period: now.UTC().Format("2006-01"),
		}
	}
	return nil
}

func (s *Service) meter(ctx context.Context, tenantID string, metric domain.CostMetric, now time.Time) {
	s.meterN(ctx, tenantID, metric, 1, now)
}

// meterN increments a usage counter by n (n may be negative, e.g. releasing a storage
// gauge on volume delete). Off the hot path; best-effort (M16).
func (s *Service) meterN(ctx context.Context, tenantID string, metric domain.CostMetric, n int64, now time.Time) {
	if s.cost == nil {
		return
	}
	start, end := domain.CurrentPeriod(now)
	_ = s.cost.Increment(ctx, tenantID, metric, n, start, end)
}
