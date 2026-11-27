package api

import (
	"errors"
	"net/http"

	"github.com/getloopscale/loop-sandbox/internal/app"
)

// PauseSandbox handles POST /v1/sandboxes/{id}/pause — snapshots a running sandbox to
// durable storage and releases compute (M16). Returns 200 on success. Pausing a
// non-running sandbox is 409 (invalid transition); an unsupported runtime is 501.
func (h *Handler) PauseSandbox(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := h.svc.PauseSandbox(r.Context(), actor, r.PathValue("id")); err != nil {
		if errors.Is(err, app.ErrPauseUnsupported) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "pause/resume is not supported by this runtime")
			return
		}
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ResumeSandbox handles POST /v1/sandboxes/{id}/resume — restores a paused sandbox from
// its snapshot and resumes execution (M16). Returns the sandbox (200).
func (h *Handler) ResumeSandbox(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	sb, err := h.svc.ResumeSandbox(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, app.ErrPauseUnsupported) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "pause/resume is not supported by this runtime")
			return
		}
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(sb))
}
