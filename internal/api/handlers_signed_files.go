package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/signedurl"
)

// Limits of signed file URLs.
const (
	defaultSignedURLTTL = 5 * time.Minute
	maxSignedURLTTL     = time.Hour
)

// signedURLKeyID is the audit identity of a request authorised by a signed URL.
const signedURLKeyID = "signed-url"

// SetURLSigner enables signed file URLs. publicURL is the address clients reach
// the API at, such as "https://sandbox.example.com"; when empty, the URL is
// built from the request that asks for it. now is the clock tokens are checked
// against; nil means the wall clock.
func (h *Handler) SetURLSigner(signer *signedurl.Signer, publicURL string, now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	h.urls, h.publicURL, h.now = signer, strings.TrimRight(publicURL, "/"), now
}

type fileURLRequest struct {
	Path             string `json:"path"`
	Operation        string `json:"operation"`
	ExpiresInSeconds int64  `json:"expires_in_seconds,omitempty"`
}

type fileURLResponse struct {
	URL       string `json:"url"`
	Method    string `json:"method"`
	ExpiresAt string `json:"expires_at"`
}

// CreateFileURL handles POST /v1/sandboxes/{id}/file-urls.
func (h *Handler) CreateFileURL(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if h.urls == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "signed file URLs are off: set LOOPSANDBOX_URL_SIGNING_KEY")
		return
	}
	var req fileURLRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	method := http.MethodGet
	switch req.Operation {
	case signedurl.OpDownload:
	case signedurl.OpUpload:
		method = http.MethodPut
	default:
		writeDomainError(w, &domain.ValidationError{Field: "operation", Reason: `must be "upload" or "download"`})
		return
	}
	clean, err := domain.ValidateSandboxPath(req.Path)
	if err != nil || clean == "/workspace" {
		writeDomainError(w, &domain.ValidationError{Field: "path", Reason: "must be a file path under /workspace"})
		return
	}
	ttl := time.Duration(req.ExpiresInSeconds) * time.Second
	switch {
	case req.ExpiresInSeconds == 0:
		ttl = defaultSignedURLTTL
	case req.ExpiresInSeconds < 0 || req.ExpiresInSeconds > int64(maxSignedURLTTL/time.Second):
		writeDomainError(w, &domain.ValidationError{Field: "expires_in_seconds", Reason: "must be between 1 and 3600"})
		return
	}
	// The URL is for a sandbox the caller owns; another tenant's answers 404.
	if _, err := h.svc.GetSandbox(r.Context(), actor, r.PathValue("id")); err != nil {
		writeDomainError(w, err)
		return
	}
	expires := h.now().Add(ttl).UTC().Truncate(time.Second)
	token, err := h.urls.Sign(signedurl.Claims{
		TenantID: actor.TenantID, SandboxID: r.PathValue("id"), Path: clean,
		Operation: req.Operation, ExpiresAt: expires,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, fileURLResponse{
		URL: h.baseURL(r) + "/v1/signed-files/" + token, Method: method, ExpiresAt: formatTime(expires),
	})
}

// baseURL is the configured public address, or the one this request came to.
func (h *Handler) baseURL(r *http.Request) string {
	if h.publicURL != "" {
		return h.publicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// signedActor verifies the token of a signed file request and returns the claims
// and the actor to act as. Every refusal is the same 403: the answer does not
// say whether a token was forged, expired or meant for the other operation.
func (h *Handler) signedActor(w http.ResponseWriter, r *http.Request, operation string) (*signedurl.Claims, app.Actor, bool) {
	refuse := func() (*signedurl.Claims, app.Actor, bool) {
		writeError(w, http.StatusForbidden, "forbidden", "the signed URL is invalid or has expired")
		return nil, app.Actor{}, false
	}
	if h.urls == nil {
		return refuse()
	}
	claims, err := h.urls.Verify(r.PathValue("token"), h.now())
	if err != nil || claims.Operation != operation {
		return refuse()
	}
	return claims, app.Actor{TenantID: claims.TenantID, KeyID: signedURLKeyID, RequestID: w.Header().Get("X-Request-ID")}, true
}

// SignedDownload handles GET /v1/signed-files/{token}.
func (h *Handler) SignedDownload(w http.ResponseWriter, r *http.Request) {
	claims, actor, ok := h.signedActor(w, r, signedurl.OpDownload)
	if !ok {
		return
	}
	rc, err := h.svc.ReadFile(r.Context(), actor, claims.SandboxID, claims.Path)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	content, err := io.ReadAll(io.LimitReader(rc, maxFileBytes))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "read error")
		return
	}
	// The file is untrusted content served from the API's origin: it is always a
	// download, never a page the browser renders.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(path.Base(claims.Path)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// SignedUpload handles PUT /v1/signed-files/{token}. The body is the file.
func (h *Handler) SignedUpload(w http.ResponseWriter, r *http.Request) {
	claims, actor, ok := h.signedActor(w, r, signedurl.OpUpload)
	if !ok {
		return
	}
	content, err := io.ReadAll(io.LimitReader(r.Body, maxFileBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "unreadable body")
		return
	}
	if len(content) > maxFileBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "file exceeds size limit")
		return
	}
	if err := h.svc.WriteFile(r.Context(), actor, claims.SandboxID, claims.Path, bytes.NewReader(content)); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
