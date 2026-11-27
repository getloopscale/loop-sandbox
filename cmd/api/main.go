// Command api is the LoopSandbox control-plane HTTP API (M0 walking skeleton).
// Composition root: wires the PostgreSQL repository + fake driver into the service.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/getloopscale/loop-sandbox/internal/ratelimit"
	"github.com/getloopscale/loop-sandbox/internal/secretbox"
	"github.com/getloopscale/loop-sandbox/internal/signedurl"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/audit"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	lsauth "github.com/getloopscale/loop-sandbox/internal/auth/loopscale"
	tjwt "github.com/getloopscale/loop-sandbox/internal/auth/loopscale/jwt"
	"github.com/getloopscale/loop-sandbox/internal/auth/loopscale/serviceauth"
	"github.com/getloopscale/loop-sandbox/internal/cells"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/filestore"
	"github.com/getloopscale/loop-sandbox/internal/mcp"
	"github.com/getloopscale/loop-sandbox/internal/metering"
	"github.com/getloopscale/loop-sandbox/internal/metrics"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
	tcplan "github.com/getloopscale/loop-sandbox/internal/plan"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/container"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
	"github.com/getloopscale/loop-sandbox/internal/runtime/firecracker"
	"github.com/getloopscale/loop-sandbox/internal/scan"
	"github.com/getloopscale/loop-sandbox/internal/templates"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
	"github.com/getloopscale/loop-sandbox/internal/webhook"
	"github.com/getloopscale/loop-sandbox/internal/worker"
	"github.com/getloopscale/loop-sandbox/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger) // packages without an injected logger log through the same handler
	if err := run(logger); err != nil {
		logger.Error("api failed", "error", err)
		os.Exit(1)
	}
}

// shutdownTimeout bounds how long in-flight requests get to finish after a
// termination signal.
const shutdownTimeout = 30 * time.Second

func run(logger *slog.Logger) error {
	addr := envOr("LOOPSANDBOX_ADDR", ":8080")
	dsn := envOr("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/loopsandbox?sslmode=disable")

	// SIGINT/SIGTERM cancel ctx, which stops the HTTP server and the worker loops, so
	// the deferred cleanups below run (close the driver, flush the audit sink).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tracesOn, shutdownTraces, err := tracing.Setup(ctx, "loopsandbox-api")
	if err != nil {
		return fmt.Errorf("set up tracing: %w", err)
	}
	// Flush buffered spans on the way out; ctx is already cancelled by then.
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTraces(flushCtx); err != nil {
			logger.Error("flush traces", "error", err)
		}
	}()
	logger.Info("tracing", "otlp_export", tracesOn)

	pool, err := connectWithRetry(ctx, dsn, 30, time.Second)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	schema, err := migrations.All()
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	applied, err := postgres.Migrate(ctx, pool, schema)
	if err != nil {
		return fmt.Errorf("apply migrations (%d applied before the failure): %w", applied, err)
	}
	logger.Info("schema migrations", "applied", applied, "known", len(schema))

	store := postgres.New(pool)
	auditSink := audit.NewSink(store, 256, logger)
	defer auditSink.Close()

	driver, err := selectDriver(envOr("LOOPSANDBOX_DRIVER", "fake"))
	if err != nil {
		return fmt.Errorf("select driver: %w", err)
	}
	// Runtime resources carry the id of this deployment, so the orphan reaper
	// leaves alone those of another deployment on the same host.
	if scoped, ok := driver.(runtime.InstanceScoped); ok {
		id, err := store.DeploymentID(ctx)
		if err != nil {
			return err
		}
		scoped.SetInstance(id)
	}
	// Tear down runtime resources (e.g. Firecracker microVMs + warm pool) on exit.
	if closer, ok := driver.(interface{ Close() error }); ok {
		defer func() { _ = closer.Close() }()
	}
	svc := app.NewService(store, driver, clock.System{}, api.PrefixedIDGen{Prefix: "sbx_"}, auditSink, store, store)
	reg := prometheus.NewRegistry()
	svc.SetObserver(metrics.New(reg))
	// Plan-quota enforcement (M8): read usage through a short-TTL cache off the DB
	// hot path; resolve the tenant's plan from the tenant store. Enterprise/unknown
	// plans are Unlimited, so this is a no-op for them.
	usage := enforcement.NewCachedUsageReader(costUsageAdapter{store}, 5*time.Second, nil)
	svc.SetEnforcer(enforcement.New(usage), planResolver{store})
	// Cell sharding (M11 B10): route each tenant to a stable cell. Default topology is a
	// single cell, so routing is LIVE + observable (span + metric) even before multi-cell
	// worker dispatch (the NATS seam) is deployed. LOOPSANDBOX_CELLS overrides the set.
	router, err := cells.NewRouter(splitCells(envOr("LOOPSANDBOX_CELLS", "cell-0")))
	if err != nil {
		return fmt.Errorf("build cell router: %w", err)
	}
	svc.SetCellRouter(router)
	// Artifact FileTransport (M11 B6): only wired when object storage is configured.
	// Absent config → the /artifact-url endpoint returns 501 (feature not deployed).
	if fs := buildArtifactStore(logger); fs != nil {
		svc.SetArtifactStore(fs)
	}
	// Durable volumes: the store records them; the driver decides whether it can attach one.
	svc.SetVolumeStore(store)
	svc.SetSnapshotStore(store)
	svc.SetAPIKeyStore(store)
	// Template registry (M17): the DB store holds templates; publishing a PUBLIC template is
	// gated on a trivy vulnerability scan (private templates register unscanned). Digest-pinning
	// is enforced in the use case.
	svc.SetTemplateRegistry(store, scan.NewTrivy(os.Getenv("LOOPSANDBOX_TRIVY_BIN")))
	// Lifecycle webhooks (M20 agent-native DX): opt-in via LOOPSANDBOX_WEBHOOK_URL. When set,
	// sandbox created/exec/destroyed events are POSTed to it (best-effort, async, egress-guarded).
	if hook := webhook.NewHTTP(os.Getenv("LOOPSANDBOX_WEBHOOK_URL")); hook != nil {
		svc.SetWebhookSink(hook)
	}
	handler := api.NewHandler(svc, store)
	// Signed file URLs: off without a key. The key is shared by every replica, so
	// a URL issued by one is honoured by the others.
	if key := os.Getenv("LOOPSANDBOX_URL_SIGNING_KEY"); key != "" {
		signer, err := signedurl.New([]byte(key))
		if err != nil {
			return fmt.Errorf("LOOPSANDBOX_URL_SIGNING_KEY: %w", err)
		}
		handler.SetURLSigner(signer, os.Getenv("LOOPSANDBOX_PUBLIC_URL"), nil)
	}
	// Secrets: off without a key. The key encrypts every secret at rest, so every
	// replica needs the same one, and losing it loses the secrets.
	if raw := os.Getenv("LOOPSANDBOX_SECRETS_KEY"); raw != "" {
		key, err := hex.DecodeString(raw)
		if err != nil {
			return errors.New("LOOPSANDBOX_SECRETS_KEY: want 64 hexadecimal characters, such as the output of `openssl rand -hex 32`")
		}
		box, err := secretbox.New(key)
		if err != nil {
			return fmt.Errorf("LOOPSANDBOX_SECRETS_KEY: want 64 hexadecimal characters: %w", err)
		}
		svc.SetSecrets(store, box)
	}
	handler.ReadyCheck = func(ctx context.Context) error { return pool.Ping(ctx) }
	authMW, err := buildAuthMiddleware(envOr("LOOPSANDBOX_AUTH", "standalone"), store, logger)
	if err != nil {
		return fmt.Errorf("build auth: %w", err)
	}
	limiter, err := buildRateLimiter()
	if err != nil {
		return err
	}
	apiRouter := api.NewRouter(handler, api.Chain(authMW, api.RateLimit(limiter, nil)), reg)

	// MCP surface (M9): a SECOND surface over the SAME service — the same API key
	// resolves to the same tenant/data as REST (cross-surface consistency). Mounted
	// at /mcp/sandbox with the standalone API-key authenticator.
	mcpHandler := mcp.NewHTTPHandler(mcp.NewServer(svc), auth.NewAPIKeyAuthenticator(store, store))
	top := http.NewServeMux()
	top.Handle("/mcp/sandbox", mcpHandler)
	top.Handle("/mcp/sandbox/", mcpHandler)
	top.Handle("/", apiRouter)

	// Background loops (TTL, orphans, stuck jobs, metering). A driver that keeps its
	// sandboxes in process memory can only be reconciled from this process, so the
	// loops are mandatory here; for other drivers they are opt-in and the standalone
	// worker binary is the default.
	workerDone := make(chan error, 1)
	embedWorker, err := shouldEmbedWorker(driver)
	if err != nil {
		return err
	}
	if embedWorker {
		interval, ierr := reconcileInterval()
		if ierr != nil {
			return ierr
		}
		usageSink, uerr := metering.SinkFromEnv(os.Getenv, logger)
		if uerr != nil {
			return uerr
		}
		q, reaper, closeQueue, qerr := worker.QueueFromEnv(ctx, pool, logger)
		if qerr != nil {
			return fmt.Errorf("build job queue: %w", qerr)
		}
		defer closeQueue()
		go func() {
			workerDone <- worker.Run(ctx, worker.Deps{
				Repo: store, Driver: driver, Audit: auditSink, Clock: clock.System{},
				Queue: q, Reaper: reaper, Idle: svc,
				Meter:    metering.NewEmitter(store, usageSink, 500),
				Logger:   logger,
				Interval: interval,
			})
		}()
	} else {
		close(workerDone)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           top,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	logger.Info("loopsandbox api listening", "addr", addr, "driver", driver.Name(), "embedded_worker", embedWorker)

	select {
	case err := <-serveErr:
		// The listener failed on its own (e.g. the port is taken): stop the loops too.
		stop()
		if werr := <-workerDone; werr != nil {
			logger.Error("worker loops", "error", werr)
		}
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	logger.Info("shutting down", "timeout", shutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if werr := <-workerDone; werr != nil {
		return fmt.Errorf("worker loops: %w", werr)
	}
	if shutdownErr != nil {
		return fmt.Errorf("http shutdown: %w", shutdownErr)
	}
	return nil
}

// shouldEmbedWorker decides whether this process runs the background loops.
// LOOPSANDBOX_EMBEDDED_WORKER=true forces them on. A process-local driver needs
// them here regardless, and asking to turn them off for such a driver is a
// configuration error: its sandboxes would never expire.
func shouldEmbedWorker(driver runtime.Driver) (bool, error) {
	raw := os.Getenv("LOOPSANDBOX_EMBEDDED_WORKER")
	local := runtime.IsProcessLocal(driver)
	if raw == "" {
		return local, nil
	}
	want, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("LOOPSANDBOX_EMBEDDED_WORKER=%q: want true or false", raw)
	}
	if local && !want {
		return false, fmt.Errorf("LOOPSANDBOX_EMBEDDED_WORKER=false with the %s driver: "+
			"its sandboxes live in this process, so no other process can enforce their TTL", driver.Name())
	}
	return want, nil
}

func reconcileInterval() (time.Duration, error) {
	v := os.Getenv("LOOPSANDBOX_RECONCILE_INTERVAL")
	if v == "" {
		return worker.DefaultInterval, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("LOOPSANDBOX_RECONCILE_INTERVAL=%q: want a positive duration such as 10s", v)
	}
	return d, nil
}

// costUsageAdapter bridges the domain CostStore to the enforcement UsageReader
// (string metric ↔ domain.CostMetric).
type costUsageAdapter struct{ store *postgres.Store }

func (a costUsageAdapter) Value(ctx context.Context, tenantID, metric string, periodStart int64) (int64, error) {
	return a.store.Value(ctx, tenantID, domain.CostMetric(metric), periodStart)
}

// planResolver resolves a tenant's plan from the tenant store (domain.Plan → plan.Plan).
type planResolver struct{ store *postgres.Store }

func (r planResolver) PlanFor(ctx context.Context, tenantID string) (tcplan.Plan, error) {
	tn, err := r.store.GetTenant(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return tcplan.Plan(tn.Plan), nil
}

func connectWithRetry(ctx context.Context, dsn string, attempts int, delay time.Duration) (*pgxpool.Pool, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				return pool, nil
			} else {
				lastErr = pingErr
				pool.Close()
			}
		} else {
			lastErr = err
		}
		time.Sleep(delay)
	}
	return nil, lastErr
}

// buildAuthMiddleware selects the auth gate for sandbox routes. "standalone" (M1)
// authenticates a lsb_ API key; "loopscale" (M7) verifies an Ed25519-signed edge forward
// + a LoopScale JWT (tenant_id + scope) via LoopScale contracts, fail-closed.
// "gateway" verifies the signed forward alone: the gateway authenticated the caller.
func buildAuthMiddleware(mode string, store *postgres.Store, logger *slog.Logger) (api.Middleware, error) {
	switch mode {
	case "standalone":
		return auth.Middleware(auth.NewAPIKeyAuthenticator(store, store)), nil
	case "loopscale":
		keys, err := parseEd25519Keys(os.Getenv("LOOPSANDBOX_LOOPSCALE_EDGE_PUBKEYS"))
		if err != nil {
			return nil, fmt.Errorf("LoopScale edge pubkeys: %w", err)
		}
		jwksURL := os.Getenv("LOOPSANDBOX_LOOPSCALE_JWKS_URL")
		if jwksURL == "" {
			return nil, fmt.Errorf("LOOPSANDBOX_LOOPSCALE_JWKS_URL required in LoopScale mode")
		}
		validator := tjwt.NewValidator(jwksURL, os.Getenv("LOOPSANDBOX_LOOPSCALE_ISSUER"), os.Getenv("LOOPSANDBOX_LOOPSCALE_AUDIENCE"))
		// Replay-window tolerance and the nonce-store TTL: the TTL MUST exceed the
		// tolerance, else a signature still inside the window could outlive its nonce
		// entry and be replayed. nonceTTL is derived to hold this invariant.
		const tolerance = 30 * time.Second
		nonceTTL := 10 * tolerance
		nonces, nErr := buildNonceStore(nonceTTL, logger)
		if nErr != nil {
			return nil, nErr
		}
		verifier := lsauth.NewForwardAuthVerifier(keys, tolerance, nonces)
		fa := lsauth.NewForwardAuthenticator(verifier, validator, "sandbox:execute")
		return fa.Middleware, nil
	case "gateway":
		keys, err := parseEd25519Keys(os.Getenv("LOOPSANDBOX_GATEWAY_PUBKEYS"))
		if err != nil {
			return nil, fmt.Errorf("LOOPSANDBOX_GATEWAY_PUBKEYS: %w", err)
		}
		tenantPlan := domain.Plan(envOr("LOOPSANDBOX_GATEWAY_TENANT_PLAN", string(domain.PlanStarter)))
		if !domain.ValidPlan(tenantPlan) {
			return nil, fmt.Errorf("LOOPSANDBOX_GATEWAY_TENANT_PLAN=%q: want starter, pro or team", tenantPlan)
		}
		const tolerance = 30 * time.Second
		nonces, nErr := buildNonceStore(10*tolerance, logger)
		if nErr != nil {
			return nil, nErr
		}
		verifier := lsauth.NewForwardAuthVerifier(keys, tolerance, nonces)
		return lsauth.NewGatewayAuthenticator(verifier, store, tenantPlan, []string{domain.ScopeSandboxExecute}).Middleware, nil
	default:
		return nil, fmt.Errorf("unknown auth mode %q (want standalone|loopscale|gateway)", mode)
	}
}

// buildNonceStore picks the replay nonce store. Multi-replica deployments MUST use
// Redis (a process-local map does not see nonces claimed on another replica — a
// captured signed forward could be replayed against a different replica within the
// window). When LOOPSANDBOX_LOOPSCALE_NONCE_REDIS_ADDR is set, use Redis; otherwise
// fall back to the in-memory store with a LOUD warning.
func buildNonceStore(ttl time.Duration, logger *slog.Logger) (serviceauth.NonceStore, error) {
	if addr := os.Getenv("LOOPSANDBOX_LOOPSCALE_NONCE_REDIS_ADDR"); addr != "" {
		client := redis.NewClient(&redis.Options{
			Addr:     addr,
			Password: os.Getenv("LOOPSANDBOX_LOOPSCALE_NONCE_REDIS_PASSWORD"),
		})
		if err := client.Ping(context.Background()).Err(); err != nil {
			return nil, fmt.Errorf("LoopScale nonce redis ping: %w", err)
		}
		return serviceauth.NewRedisNonceStore(client, ttl), nil
	}
	logger.Warn("LoopScale auth: using IN-MEMORY nonce store — replay protection is " +
		"single-replica only; set LOOPSANDBOX_LOOPSCALE_NONCE_REDIS_ADDR for multi-replica")
	return serviceauth.NewInMemoryNonceStore(ttl), nil
}

// parseEd25519Keys decodes a comma-separated list of base64 Ed25519 public keys.
func parseEd25519Keys(csv string) ([]ed25519.PublicKey, error) {
	if csv == "" {
		return nil, fmt.Errorf("no edge public keys configured")
	}
	var keys []ed25519.PublicKey
	for _, b64 := range strings.Split(csv, ",") {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return nil, err
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid ed25519 public key size %d", len(raw))
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys, nil
}

// selectDriver picks the runtime driver by name. "fake" is the default (no host
// runtime needed — used in k3d nodes without a reachable Docker daemon and in
// tests); "container" creates hardened Docker containers on the node (M2).
func selectDriver(name string) (runtime.Driver, error) {
	switch name {
	case "fake":
		return fake.New(), nil
	case "container":
		// The official template images form the default allow-list so tenants can
		// launch any vetted template out of the box (M5).
		return container.NewFromEnv(envOr("LOOPSANDBOX_SANDBOX_IMAGE", "busybox:latest"), templates.Images()...)
	case "firecracker":
		// microVM driver (M6): hardware-virtualised isolation, exec/files via the
		// in-guest agent over vsock. Requires /dev/kvm + FC assets (see NewFromEnv).
		fc, err := firecracker.NewFromEnv()
		if err != nil {
			return nil, err
		}
		// Opt-in warm pool for near-instant provisioning (snapshot restore).
		if n := envOr("LOOPSANDBOX_FC_WARMPOOL", "0"); n != "0" {
			size, _ := strconv.Atoi(n)
			if perr := fc.EnableWarmPool(context.Background(), size); perr != nil {
				return nil, fmt.Errorf("enable warm pool: %w", perr)
			}
		}
		return fc, nil
	default:
		return nil, fmt.Errorf("unknown driver %q (want fake|container|firecracker)", name)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitCells parses a comma-separated cell list, trimming blanks.
func splitCells(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// buildArtifactStore constructs the S3 FileTransport from env, or returns nil when the
// bucket is not configured (the feature is optional; the endpoint then returns 501).
// Credentials come from LOOPSANDBOX_ARTIFACT_{ACCESS_KEY,SECRET_KEY}; a custom
// endpoint (LocalStack / MinIO / non-AWS S3) is honored via _ENDPOINT.
func buildArtifactStore(logger *slog.Logger) app.ArtifactStore {
	bucket := os.Getenv("LOOPSANDBOX_ARTIFACT_BUCKET")
	if bucket == "" {
		return nil
	}
	opts := s3.Options{
		Region: envOr("LOOPSANDBOX_ARTIFACT_REGION", "us-east-1"),
		Credentials: credentials.NewStaticCredentialsProvider(
			os.Getenv("LOOPSANDBOX_ARTIFACT_ACCESS_KEY"),
			os.Getenv("LOOPSANDBOX_ARTIFACT_SECRET_KEY"), ""),
	}
	if ep := os.Getenv("LOOPSANDBOX_ARTIFACT_ENDPOINT"); ep != "" {
		opts.BaseEndpoint = aws.String(ep)
		opts.UsePathStyle = true // path-style for non-AWS S3 endpoints
	}
	logger.Info("artifact FileTransport wired", "bucket", bucket)
	return filestore.NewS3(s3.New(opts), bucket)
}

// Defaults of the per-key request limit.
const (
	defaultRateLimitRPS   = 100
	defaultRateLimitBurst = 200
)

// buildRateLimiter reads the per-key request limit from the environment. A rate
// of 0 turns the limit off. The buckets live in this process, so each replica
// of the API allows the configured rate on its own.
func buildRateLimiter() (*ratelimit.Limiter, error) {
	rps, burst := float64(defaultRateLimitRPS), defaultRateLimitBurst
	if raw := os.Getenv("LOOPSANDBOX_RATE_LIMIT_RPS"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("LOOPSANDBOX_RATE_LIMIT_RPS=%q: must be a number, 0 or more", raw)
		}
		rps, burst = v, int(2*v)
	}
	if raw := os.Getenv("LOOPSANDBOX_RATE_LIMIT_BURST"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return nil, fmt.Errorf("LOOPSANDBOX_RATE_LIMIT_BURST=%q: must be an integer, 1 or more", raw)
		}
		burst = v
	}
	return ratelimit.New(rps, burst), nil
}
