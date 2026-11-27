package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// streamExec runs a command and streams stdout/stderr as SSE events, ending with a
// "done" event carrying the exit code (M15).
//
// The 200 + SSE headers are written LAZILY — only once the first chunk arrives (or the
// command completes with no output). If the service rejects the request BEFORE any output
// (unknown sandbox → 404, missing quota → 429, runtime cannot stream → 501), the status line
// is still uncommitted, so the handler returns the correct HTTP status instead of a 200 with
// the error buried in the event stream. Once bytes have flowed the status is fixed at 200 and
// a late error can only be reported as a trailing SSE `error` event.
func (h *Handler) streamExec(w http.ResponseWriter, r *http.Request, actor app.Actor, body execRequestBody) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}
	headerWritten := false
	ensureHeader := func() {
		if headerWritten {
			return
		}
		headerWritten = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}

	res, err := h.svc.RunExecStream(r.Context(), actor, r.PathValue("id"), runtime.ExecRequest{
		Cmd: body.Cmd, Env: body.Env, WorkingDir: body.WorkDir,
		Timeout: time.Duration(body.TimeoutMs) * time.Millisecond,
	}, func(c runtime.StreamChunk) {
		ensureHeader()
		b, _ := json.Marshal(map[string]string{"data": string(c.Data)})
		_, _ = w.Write([]byte("event: " + c.Stream + "\ndata: " + string(b) + "\n\n"))
		flusher.Flush()
	})
	if err != nil {
		// No chunk emitted yet → the status line is still ours to set correctly.
		if !headerWritten {
			if errors.Is(err, app.ErrExecStreamUnsupported) {
				writeError(w, http.StatusNotImplemented, "not_implemented", "streamed exec is not supported by this runtime")
				return
			}
			writeDomainError(w, err)
			return
		}
		// Bytes already flowed; status is fixed at 200 — best-effort trailing error event.
		msg, _ := json.Marshal(map[string]string{"error": err.Error(), "code": "error"})
		_, _ = w.Write([]byte("event: error\ndata: " + string(msg) + "\n\n"))
		flusher.Flush()
		return
	}
	ensureHeader() // a zero-output success still needs the SSE headers before the done event.
	done, _ := json.Marshal(map[string]any{"exit_code": res.ExitCode})
	_, _ = w.Write([]byte("event: done\ndata: " + string(done) + "\n\n"))
	flusher.Flush()
}
