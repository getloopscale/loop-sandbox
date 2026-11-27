package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type fileInfoResponse struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
	// Mode is the permission bits in octal, as chmod takes them: "0644".
	Mode       string `json:"mode"`
	ModifiedAt string `json:"modified_at"`
}

// FileInfo handles GET /v1/sandboxes/{id}/file-info/{path...}.
func (h *Handler) FileInfo(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	filePath := "/" + r.PathValue("path")
	info, err := h.svc.StatFile(r.Context(), actor, r.PathValue("id"), filePath)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fileInfoResponse{
		Name: info.Name, Path: filePath, Size: info.Size, IsDir: info.IsDir,
		Mode: fmt.Sprintf("%04o", info.Mode), ModifiedAt: formatTime(info.ModTime),
	})
}

// MakeDirectory handles PUT /v1/sandboxes/{id}/directories/{path...}.
func (h *Handler) MakeDirectory(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := h.svc.MakeDir(r.Context(), actor, r.PathValue("id"), "/"+r.PathValue("path")); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type moveRequest struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// MoveFile handles POST /v1/sandboxes/{id}/file-moves.
func (h *Handler) MoveFile(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var req moveRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	if err := h.svc.MoveFile(r.Context(), actor, r.PathValue("id"), req.From, req.To, req.Overwrite); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
