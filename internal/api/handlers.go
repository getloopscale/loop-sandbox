// Package api is the HTTP interface layer (/v1). It maps requests to use cases and
// typed domain errors to HTTP status codes (TRD §7.4). It contains no business logic.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/getloopscale/loop-sandbox/internal/signedurl"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/plan"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/templates"
)

// Handler holds the API dependencies.
type Handler struct {
	svc  *app.Service
	idem domain.IdempotencyStore
	// ReadyCheck, when set, is called by GET /v1/readyz to verify downstream
	// dependencies (e.g. the DB pool). Nil means "always ready" (used in tests).
	ReadyCheck func(ctx context.Context) error
	// Signed file URLs, off while urls is nil. See SetURLSigner.
	urls      *signedurl.Signer
	publicURL string
	now       func() time.Time
}

// NewHandler wires the API to the application service. idem may be nil (tests
// without idempotency).
func NewHandler(svc *app.Service, idem domain.IdempotencyStore) *Handler {
	return &Handler{svc: svc, idem: idem}
}

type createRequest struct {
	Image    string `json:"image"`
	Template string `json:"template,omitempty"` // M17: provision from a registered template id
	Snapshot string `json:"snapshot,omitempty"` // start from a snapshot id
	// Secrets names the tenant's secrets the sandbox receives as environment variables.
	Secrets []string `json:"secrets,omitempty"`
	// AutoPauseSeconds pauses the sandbox after that long without use.
	AutoPauseSeconds int64  `json:"auto_pause_seconds,omitempty"`
	GPU              string `json:"gpu,omitempty"` // M19: request a GPU-backed sandbox (premium)
	TTLSeconds       int64  `json:"ttl_seconds,omitempty"`
	// Metadata are labels returned as given; Env is the default environment of
	// every command run in the sandbox.
	Metadata map[string]string `json:"metadata,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

type sandboxResponse struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	Image     string `json:"image,omitempty"`
	GPU       string `json:"gpu,omitempty"` // M19: the GPU class, when a GPU sandbox
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at,omitempty"`
	// AutoPauseSeconds is the idle limit, present when the sandbox has one.
	AutoPauseSeconds int64             `json:"auto_pause_seconds,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	// Secrets are the names of the secrets the sandbox receives. Never the values.
	Secrets []string          `json:"secrets,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func toResponse(s *domain.Sandbox) sandboxResponse {
	return sandboxResponse{
		ID:               s.ID,
		TenantID:         s.TenantID,
		Image:            s.Image,
		GPU:              s.GPU,
		State:            string(s.State),
		CreatedAt:        s.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		ExpiresAt:        formatTime(s.ExpiresAt),
		AutoPauseSeconds: int64(s.AutoPauseAfter / time.Second),
		Metadata:         s.Metadata,
		Secrets:          s.Secrets,
		Env:              s.Env,
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02T15:04:05Z07:00")
}

// actorFrom builds the use-case actor from the authenticated principal.
func actorFrom(w http.ResponseWriter, r *http.Request) (app.Actor, bool) {
	p := auth.PrincipalFrom(r.Context())
	if p == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing credentials")
		return app.Actor{}, false
	}
	return app.Actor{TenantID: p.TenantID, KeyID: p.KeyID, RequestID: w.Header().Get("X-Request-ID"), Scopes: p.Scopes}, true
}

// actorFromQuiet returns the actor without writing an error when unauthenticated (for handlers
// that degrade gracefully, e.g. the templates listing shows only built-ins when anonymous).
func actorFromQuiet(r *http.Request) (app.Actor, bool) {
	p := auth.PrincipalFrom(r.Context())
	if p == nil {
		return app.Actor{}, false
	}
	return app.Actor{TenantID: p.TenantID, KeyID: p.KeyID, RequestID: r.Header.Get("X-Request-ID")}, true
}

// CreateSandbox handles POST /v1/sandboxes, honoring an optional Idempotency-Key.
// Idempotency uses reserve-before-act (TRD §6.3): the key is claimed BEFORE the
// sandbox is created, so concurrent same-key requests cannot double-create.
func (h *Handler) CreateSandbox(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "unreadable body")
		return
	}
	var req createRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
			return
		}
	}

	idemKey := r.Header.Get("Idempotency-Key")
	useIdem := idemKey != "" && h.idem != nil
	if useIdem {
		res, rerr := h.idem.Reserve(r.Context(), actor.TenantID, idemKey, hashBody(raw), time.Now().Unix())
		if rerr != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		switch res.Outcome {
		case domain.Replay:
			replay(w, res.Record)
			return
		case domain.Conflict:
			writeError(w, http.StatusConflict, "conflict", "Idempotency-Key reused with a different payload")
			return
		case domain.InProgress:
			// Distinct code from a payload-mismatch conflict: an in-flight reservation
			// is transient — a client that retried after an ambiguous network failure
			// should back off and retry, not surface a hard conflict. The SDK treats
			// "idempotency_in_progress" as retriable.
			writeError(w, http.StatusConflict, "idempotency_in_progress", "a request with this Idempotency-Key is in progress")
			return
		case domain.Claimed:
			// proceed; we now own the reservation and MUST Complete or Release.
		}
	}

	// template and gpu are mutually exclusive: a GPU template would silently drop the GPU
	// (bypassing the premium quota gate) — reject the ambiguity rather than diverge from intent.
	if req.Template != "" && req.GPU != "" {
		if useIdem {
			_ = h.idem.Release(r.Context(), actor.TenantID, idemKey)
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "template and gpu are mutually exclusive")
		return
	}
	sb, err := h.svc.CreateSandboxFromSpec(r.Context(), actor, app.SandboxSpec{
		Image:     req.Image,
		Template:  req.Template,
		Snapshot:  req.Snapshot,
		AutoPause: time.Duration(req.AutoPauseSeconds) * time.Second,
		Secrets:   req.Secrets,
		GPU:       req.GPU,
		TTL:       time.Duration(req.TTLSeconds) * time.Second,
		Metadata:  req.Metadata,
		Env:       req.Env,
	})
	if err != nil {
		if useIdem {
			_ = h.idem.Release(r.Context(), actor.TenantID, idemKey) // let a retry proceed
		}
		writeDomainError(w, err)
		return
	}
	body, _ := json.Marshal(toResponse(sb))
	if useIdem {
		_ = h.idem.Complete(r.Context(), actor.TenantID, idemKey, http.StatusCreated, body)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

// GetSandbox handles GET /v1/sandboxes/{id}.
func (h *Handler) GetSandbox(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	sb, err := h.svc.GetSandbox(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(sb))
}

// DeleteSandbox handles DELETE /v1/sandboxes/{id}.
func (h *Handler) DeleteSandbox(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := h.svc.DestroySandbox(r.Context(), actor, r.PathValue("id")); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Templates handles GET /v1/templates — the built-in catalog PLUS the tenant's registered
// templates and all public ones (M17 registry). When unauthenticated or the registry is
// unwired, only the built-in catalog is returned.
func (h *Handler) Templates(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0)
	for _, t := range templates.Official() {
		out = append(out, map[string]any{"name": t.Name, "image": t.Image, "description": t.Description, "builtin": true})
	}
	if actor, ok := actorFromQuiet(r); ok {
		regs, err := h.svc.ListTemplates(r.Context(), actor)
		// Without a registry the built-ins are the whole list. Any other failure
		// is an error: a list that omits the tenant's templates looks complete.
		if err != nil && !errors.Is(err, app.ErrTemplatesUnsupported) {
			writeDomainError(w, err)
			return
		}
		for _, t := range regs {
			out = append(out, map[string]any{
				"id": t.ID, "name": t.Name, "image": t.ImageRef, "version": t.Version,
				"digest": t.Digest, "public": t.Public, "scan_status": t.ScanStatus,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// Health handles GET /v1/healthz (liveness).
func (h *Handler) Health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Ready handles GET /v1/readyz (readiness — dependencies reachable).
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	if h.ReadyCheck != nil {
		if err := h.ReadyCheck(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "not_ready", "dependency unavailable")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func hashBody(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func replay(w http.ResponseWriter, rec *domain.IdempotencyRecord) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotent-Replay", "true")
	w.WriteHeader(rec.Status)
	_, _ = w.Write(rec.Body)
}

func writeDomainError(w http.ResponseWriter, err error) {
	var exceeded *plan.ExceededError
	var invalid *domain.ValidationError
	var secretGone *app.SecretUnavailableError
	switch {
	case errors.As(err, &invalid):
		// A validation message is written for the caller and names the field.
		writeError(w, http.StatusBadRequest, "invalid_request", invalid.Error())
	case errors.As(err, &exceeded):
		// Plan/quota overflow → HTTP 402 with the LoopScale contracts rich context.
		writeError(w, http.StatusPaymentRequired, "quota_exceeded",
			fmt.Sprintf("plan %s cap for %s reached (%d/%d, period %s)",
				exceeded.Plan, exceeded.Metric, exceeded.Current, exceeded.Cap, exceeded.Period))
	case errors.As(err, &secretGone):
		writeError(w, http.StatusConflict, "secret_unavailable", secretGone.Error())
	case errors.Is(err, app.ErrSecretsUnsupported):
		writeError(w, http.StatusNotImplemented, "not_implemented", "secrets are not supported by this deployment: set LOOPSANDBOX_SECRETS_KEY")
	case errors.Is(err, app.ErrTooManySecrets):
		writeError(w, http.StatusConflict, "too_many_secrets",
			fmt.Sprintf("the tenant is at its limit of %d secrets; delete one first", domain.MaxSecretsPerTenant))
	case errors.Is(err, app.ErrSnapshotsUnsupported):
		writeError(w, http.StatusNotImplemented, "not_implemented", "snapshots are not supported by this deployment")
	case errors.Is(err, app.ErrSnapshotNameTaken):
		writeError(w, http.StatusConflict, "already_exists", "a snapshot with this name already exists")
	case errors.Is(err, app.ErrTooManySnapshots):
		writeError(w, http.StatusConflict, "too_many_snapshots",
			fmt.Sprintf("the tenant is at its limit of %d snapshots; delete one first", domain.MaxSnapshotsPerTenant))
	case errors.Is(err, domain.ErrAlreadyExists):
		writeError(w, http.StatusConflict, "already_exists", "the destination already exists")
	case errors.Is(err, app.ErrProcessesUnsupported):
		writeError(w, http.StatusNotImplemented, "not_implemented", "background processes are not supported by this runtime")
	case errors.Is(err, app.ErrProcessExited):
		writeError(w, http.StatusConflict, "process_exited", "the process has ended")
	case errors.Is(err, app.ErrStdinClosed):
		writeError(w, http.StatusConflict, "stdin_closed", "the standard input of the process is closed")
	case errors.Is(err, app.ErrTooManyProcesses):
		writeError(w, http.StatusConflict, "too_many_processes", "the sandbox is at its limit of running processes")
	case errors.Is(err, app.ErrLogsUnsupported):
		writeError(w, http.StatusNotImplemented, "not_implemented", "sandbox logs are not supported by this runtime")
	case errors.Is(err, app.ErrMetricsUnsupported):
		writeError(w, http.StatusNotImplemented, "not_implemented", "sandbox metrics are not supported by this runtime")
	case errors.Is(err, app.ErrFileWatchUnsupported):
		writeError(w, http.StatusNotImplemented, "not_implemented", "directory watches are not supported by this runtime")
	case errors.Is(err, app.ErrTooManyWatches):
		writeError(w, http.StatusConflict, "too_many_watches", "the sandbox is at its limit of watches")
	case errors.Is(err, app.ErrFileOpsUnsupported):
		writeError(w, http.StatusNotImplemented, "not_implemented", "file operations are not supported by this runtime")
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, app.ErrAPIKeysUnsupported):
		writeError(w, http.StatusNotImplemented, "not_implemented", "this deployment does not manage API keys")
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, domain.ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid argument")
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "conflict", "invalid sandbox state")
	case errors.Is(err, domain.ErrTimeout):
		writeError(w, http.StatusGatewayTimeout, "timeout", "operation timed out")
	default:
		// The client gets a generic body; the cause goes to the log, keyed by the
		// request id the client also received, so the two can be matched.
		slog.Error("request failed with an internal error",
			"error", err, "request_id", w.Header().Get("X-Request-ID"))
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
