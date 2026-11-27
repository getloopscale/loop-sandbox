package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

type snapshotResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SandboxID string `json:"sandbox_id"`
	Image     string `json:"image,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt string `json:"created_at"`
}

func toSnapshotResponse(s *domain.Snapshot) snapshotResponse {
	return snapshotResponse{
		ID: s.ID, Name: s.Name, SandboxID: s.SandboxID, Image: s.Image,
		SizeBytes: s.SizeBytes, CreatedAt: formatTime(s.CreatedAt),
	}
}

// CreateSnapshot handles POST /v1/sandboxes/{id}/snapshots {name}.
func (h *Handler) CreateSnapshot(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	snap, err := h.svc.CreateSnapshot(r.Context(), actor, r.PathValue("id"), req.Name)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSnapshotResponse(snap))
}

// ListSnapshots handles GET /v1/snapshots.
func (h *Handler) ListSnapshots(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	snaps, err := h.svc.ListSnapshots(r.Context(), actor)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	out := make([]snapshotResponse, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, toSnapshotResponse(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

// GetSnapshot handles GET /v1/snapshots/{snapid}.
func (h *Handler) GetSnapshot(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	snap, err := h.svc.GetSnapshot(r.Context(), actor, r.PathValue("snapid"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toSnapshotResponse(snap))
}

// DeleteSnapshot handles DELETE /v1/snapshots/{snapid}.
func (h *Handler) DeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteSnapshot(r.Context(), actor, r.PathValue("snapid")); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
