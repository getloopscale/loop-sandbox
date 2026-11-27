package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/domain"
)

type registerTemplateBody struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	Version string `json:"version,omitempty"`
	Public  bool   `json:"public,omitempty"`
}

type templateResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Image      string `json:"image"`
	Version    string `json:"version"`
	Digest     string `json:"digest,omitempty"`
	Public     bool   `json:"public"`
	ScanStatus string `json:"scan_status"`
}

func toTemplateResponse(t *domain.Template) templateResponse {
	return templateResponse{
		ID: t.ID, Name: t.Name, Image: t.ImageRef, Version: t.Version,
		Digest: t.Digest, Public: t.Public, ScanStatus: t.ScanStatus,
	}
}

// writeTemplateError maps M17 template errors to HTTP status.
func writeTemplateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, app.ErrTemplatesUnsupported):
		writeError(w, http.StatusNotImplemented, "not_implemented", "the template registry is not available")
	case errors.Is(err, app.ErrTemplateNotPinned):
		writeError(w, http.StatusBadRequest, "invalid_request", "template image must be digest-pinned (…@sha256:…)")
	case errors.Is(err, app.ErrTemplateScanFailed):
		writeError(w, http.StatusUnprocessableEntity, "scan_failed", "template failed the vulnerability scan gate")
	default:
		writeDomainError(w, err)
	}
}

// RegisterTemplate handles POST /v1/templates — registers a versioned, digest-pinned template;
// publishing public requires passing the vulnerability scan gate (M17). Returns 201.
func (h *Handler) RegisterTemplate(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var body registerTemplateBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	if body.Name == "" || body.Image == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name and image are required")
		return
	}
	t, err := h.svc.RegisterTemplate(r.Context(), actor, body.Name, body.Image, body.Version, body.Public)
	if err != nil {
		writeTemplateError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toTemplateResponse(t))
}

// GetTemplate handles GET /v1/templates/{id} — a template visible to the tenant (M17).
func (h *Handler) GetTemplate(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	t, err := h.svc.GetTemplate(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		writeTemplateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTemplateResponse(t))
}
