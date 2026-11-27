package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/domain"
)

type createVolumeBody struct {
	SandboxID string `json:"sandbox_id"`
	Name      string `json:"name"`
	SizeGB    int64  `json:"size_gb"`
}

type volumeResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SizeGB    int64  `json:"size_gb"`
	SandboxID string `json:"sandbox_id,omitempty"`
	CreatedAt string `json:"created_at"`
}

func toVolumeResponse(v *domain.Volume) volumeResponse {
	return volumeResponse{
		ID:        v.ID,
		Name:      v.Name,
		SizeGB:    v.SizeGB,
		SandboxID: v.SandboxID,
		CreatedAt: v.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// CreateVolume handles POST /v1/volumes — provisions a durable named volume and attaches
// it to the given sandbox (M16). Body: {sandbox_id, name, size_gb}. Returns 201.
func (h *Handler) CreateVolume(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var body createVolumeBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	if body.SandboxID == "" || body.Name == "" || body.SizeGB <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "sandbox_id, name and a positive size_gb are required")
		return
	}
	vol, err := h.svc.CreateVolume(r.Context(), actor, body.SandboxID, body.Name, body.SizeGB)
	if err != nil {
		if errors.Is(err, app.ErrVolumesUnsupported) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "durable volumes are not supported by this deployment")
			return
		}
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toVolumeResponse(vol))
}

// ListVolumes handles GET /v1/volumes?sandbox_id=... — lists a sandbox's volumes (M16).
func (h *Handler) ListVolumes(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	sandboxID := r.URL.Query().Get("sandbox_id")
	if sandboxID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "sandbox_id query parameter is required")
		return
	}
	vols, err := h.svc.ListVolumes(r.Context(), actor, sandboxID)
	if err != nil {
		if errors.Is(err, app.ErrVolumesUnsupported) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "durable volumes are not supported by this deployment")
			return
		}
		writeDomainError(w, err)
		return
	}
	out := make([]volumeResponse, 0, len(vols))
	for _, v := range vols {
		out = append(out, toVolumeResponse(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": out})
}

// DeleteVolume handles DELETE /v1/volumes/{vid} — destroys a volume's backing store and
// releases its storage quota (M16). Cross-tenant access is not-found. Returns 204.
func (h *Handler) DeleteVolume(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteVolume(r.Context(), actor, r.PathValue("vid")); err != nil {
		if errors.Is(err, app.ErrVolumesUnsupported) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "durable volumes are not supported by this deployment")
			return
		}
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
