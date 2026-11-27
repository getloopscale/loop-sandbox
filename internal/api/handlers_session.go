package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// StartSession handles POST /v1/sandboxes/{id}/sessions — opens a stateful
// code-interpreter session (M12) and returns its id.
func (h *Handler) StartSession(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	sid, err := h.svc.StartSession(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, app.ErrSessionsUnsupported) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "code-interpreter sessions are not supported by this runtime")
			return
		}
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"session_id": sid})
}

type runCellBody struct {
	Code string `json:"code"`
}

// RunCell handles POST /v1/sandboxes/{id}/sessions/{sid}/execute — runs one code cell in
// the session; state from prior cells is visible. A cell that raises returns 200 with
// exit_code=1 (US-02: a program error is not a transport error).
func (h *Handler) RunCell(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxFileBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "read body")
		return
	}
	var req runCellBody
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	// Streaming mode: partial stdout as the cell runs, over Server-Sent Events.
	if r.URL.Query().Get("stream") == "1" || r.URL.Query().Get("stream") == "true" {
		// The stream commits to a 200 before the first chunk, so the capability check
		// has to come first.
		if !h.svc.Supports(runtime.CapSessionStream) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "streamed code cells are not supported by this runtime")
			return
		}
		h.runCellStream(w, r, actor, req.Code)
		return
	}
	res, err := h.svc.RunCell(r.Context(), actor, r.PathValue("id"), r.PathValue("sid"), req.Code)
	if err != nil {
		if errors.Is(err, app.ErrSessionsUnsupported) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "code-interpreter sessions are not supported by this runtime")
			return
		}
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exit_code": res.ExitCode,
		"stdout":    string(res.Stdout),
		"stderr":    string(res.Stderr),
	})
}

// runCellStream streams a cell's output as SSE: an "stdout"/"stderr" event per partial
// chunk as the cell runs, then a "done" event with the exit code.
func (h *Handler) runCellStream(w http.ResponseWriter, r *http.Request, actor app.Actor, code string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	res, err := h.svc.RunCellStream(r.Context(), actor, r.PathValue("id"), r.PathValue("sid"), code,
		func(c runtime.StreamChunk) {
			b, _ := json.Marshal(map[string]string{"data": string(c.Data)})
			_, _ = w.Write([]byte("event: " + c.Stream + "\ndata: " + string(b) + "\n\n"))
			flusher.Flush()
		})
	if err != nil {
		msg, _ := json.Marshal(map[string]string{"error": err.Error()})
		_, _ = w.Write([]byte("event: error\ndata: " + string(msg) + "\n\n"))
		flusher.Flush()
		return
	}
	done, _ := json.Marshal(map[string]any{"exit_code": res.ExitCode, "stderr": string(res.Stderr)})
	_, _ = w.Write([]byte("event: done\ndata: " + string(done) + "\n\n"))
	flusher.Flush()
}

// CloseSession handles DELETE /v1/sandboxes/{id}/sessions/{sid}.
func (h *Handler) CloseSession(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := h.svc.CloseSession(r.Context(), actor, r.PathValue("id"), r.PathValue("sid")); err != nil {
		if errors.Is(err, app.ErrSessionsUnsupported) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "code-interpreter sessions are not supported by this runtime")
			return
		}
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
