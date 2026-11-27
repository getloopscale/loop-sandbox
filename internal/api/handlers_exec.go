package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

const maxFileBytes = 32 << 20 // 32 MiB upload cap (M3; object storage in M11)

type execRequestBody struct {
	Cmd       []string          `json:"cmd"`
	Env       map[string]string `json:"env,omitempty"`
	WorkDir   string            `json:"workdir,omitempty"`
	TimeoutMs int64             `json:"timeout_ms,omitempty"`
}

type execResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// CreateExecution handles POST /v1/sandboxes/{id}/executions. A non-zero exit is a
// 200 with the exit code in the body (US-02: process failure ≠ transport failure).
func (h *Handler) CreateExecution(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var body execRequestBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	if len(body.Cmd) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "cmd is required")
		return
	}
	// Streamed exec (M15): partial stdout/stderr over SSE as the command runs.
	if r.URL.Query().Get("stream") == "1" || r.URL.Query().Get("stream") == "true" {
		h.streamExec(w, r, actor, body)
		return
	}
	res, err := h.svc.Exec(r.Context(), actor, r.PathValue("id"), runtime.ExecRequest{
		Cmd: body.Cmd, Env: body.Env, WorkingDir: body.WorkDir,
		Timeout: time.Duration(body.TimeoutMs) * time.Millisecond,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, execResponse{
		ExitCode: res.ExitCode, Stdout: string(res.Stdout), Stderr: string(res.Stderr),
	})
}

// PutFile handles PUT /v1/sandboxes/{id}/files/{path...}. An upload over the size
// cap is rejected with 413 (fail-fast) rather than silently truncated.
func (h *Handler) PutFile(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
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
	if err := h.svc.WriteFile(r.Context(), actor, r.PathValue("id"), "/"+r.PathValue("path"), bytes.NewReader(content)); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetFile handles GET /v1/sandboxes/{id}/files/{path...} — streams content with a
// size and SHA-256 checksum header.
func (h *Handler) GetFile(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	rc, err := h.svc.ReadFile(r.Context(), actor, r.PathValue("id"), "/"+r.PathValue("path"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	// Buffer to compute checksum + length (M3 files are small; streaming to object
	// storage is the M11 FileTransport seam).
	content, err := io.ReadAll(io.LimitReader(rc, maxFileBytes))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "read error")
		return
	}
	sum := sha256.Sum256(content)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("X-Checksum-SHA256", hex.EncodeToString(sum[:]))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// DeleteFile handles DELETE /v1/sandboxes/{id}/files/{path...}.
func (h *Handler) DeleteFile(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteFile(r.Context(), actor, r.PathValue("id"), "/"+r.PathValue("path")); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type fileEntryResponse struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

// ListFiles handles GET /v1/sandboxes/{id}/files?path=/workspace.
func (h *Handler) ListFiles(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "/workspace"
	}
	entries, err := h.svc.ListFiles(r.Context(), actor, r.PathValue("id"), dir)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	out := make([]fileEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, fileEntryResponse{Name: e.Name, Size: e.Size, IsDir: e.IsDir})
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out})
}

// ArtifactURL handles GET /v1/sandboxes/{id}/artifact-url/{path...} — mints a
// short-lived pre-signed download URL for a large artifact so the client fetches it
// from object storage directly, not through the control plane (M11 B6 FileTransport).
func (h *Handler) ArtifactURL(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	url, err := h.svc.PresignArtifactURL(r.Context(), actor, r.PathValue("id"), "/"+r.PathValue("path"), artifactURLTTL)
	if err != nil {
		if errors.Is(err, app.ErrArtifactStoreUnset) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "artifact object storage is not configured")
			return
		}
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": url, "expires_in_seconds": int(artifactURLTTL.Seconds())})
}

const artifactURLTTL = 10 * time.Minute
