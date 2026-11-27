package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/getloopscale/loop-sandbox/internal/app"
)

// ForkSandbox handles POST /v1/sandboxes/{id}/fork — clones a running sandbox into a new
// isolated one (M13). Returns the new sandbox (201).
func (h *Handler) ForkSandbox(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	// An empty body is one fork, answered as a single sandbox. A body with
	// "count" is a batch, answered as a list even when count is 1.
	var req struct {
		Count *int `json:"count"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	count := 1
	if req.Count != nil {
		count = *req.Count
	}
	forks, err := h.svc.ForkSandboxes(r.Context(), actor, r.PathValue("id"), count)
	if err != nil {
		if errors.Is(err, app.ErrForkUnsupported) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "fork is not supported by this runtime")
			return
		}
		writeDomainError(w, err)
		return
	}
	if req.Count == nil {
		writeJSON(w, http.StatusCreated, toResponse(forks[0]))
		return
	}
	out := make([]any, 0, len(forks))
	for _, sb := range forks {
		out = append(out, toResponse(sb))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sandboxes": out})
}
