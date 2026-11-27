package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

type startProcessBody struct {
	Cmd     []string          `json:"cmd"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
}

type processResponse struct {
	PID       int      `json:"pid"`
	Cmd       []string `json:"cmd"`
	StartedAt string   `json:"started_at"`
	Running   bool     `json:"running"`
	// ExitCode is null while the process runs.
	ExitCode *int `json:"exit_code"`
}

func toProcessResponse(p *runtime.ProcessInfo) processResponse {
	out := processResponse{PID: p.PID, Cmd: p.Cmd, StartedAt: formatTime(p.StartedAt), Running: p.Running}
	if !p.Running {
		code := p.ExitCode
		out.ExitCode = &code
	}
	return out
}

// pidFrom reads the {pid} path value, answering 404 for one that cannot name a
// process.
func pidFrom(w http.ResponseWriter, r *http.Request) (int, bool) {
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil || pid <= 0 {
		writeError(w, http.StatusNotFound, "not_found", "process not found")
		return 0, false
	}
	return pid, true
}

// StartProcess handles POST /v1/sandboxes/{id}/processes.
func (h *Handler) StartProcess(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var body startProcessBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	info, err := h.svc.StartProcess(r.Context(), actor, r.PathValue("id"),
		runtime.ProcessSpec{Cmd: body.Cmd, Env: body.Env, WorkingDir: body.WorkDir})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toProcessResponse(info))
}

// ListProcesses handles GET /v1/sandboxes/{id}/processes.
func (h *Handler) ListProcesses(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListProcesses(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	out := make([]processResponse, 0, len(list))
	for i := range list {
		out = append(out, toProcessResponse(&list[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"processes": out})
}

// GetProcess handles GET /v1/sandboxes/{id}/processes/{pid}.
func (h *Handler) GetProcess(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	pid, ok := pidFrom(w, r)
	if !ok {
		return
	}
	info, err := h.svc.GetProcess(r.Context(), actor, r.PathValue("id"), pid)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toProcessResponse(info))
}

type processOutputResponse struct {
	Stdout    string          `json:"stdout"`
	Stderr    string          `json:"stderr"`
	Next      int64           `json:"next"`
	Truncated bool            `json:"truncated"`
	Process   processResponse `json:"process"`
}

// ProcessOutput handles GET /v1/sandboxes/{id}/processes/{pid}/output.
func (h *Handler) ProcessOutput(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	pid, ok := pidFrom(w, r)
	if !ok {
		return
	}
	after, ok := queryInt(w, r, "after")
	if !ok {
		return
	}
	waitMS, ok := queryInt(w, r, "wait_ms")
	if !ok {
		return
	}
	// Bounded before the multiplication, so a huge value cannot overflow into a
	// valid duration.
	if waitMS > app.MaxProcessOutputWait.Milliseconds() {
		waitMS = app.MaxProcessOutputWait.Milliseconds() + 1
	}
	out, err := h.svc.ProcessOutput(r.Context(), actor, r.PathValue("id"), pid, after, time.Duration(waitMS)*time.Millisecond)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, processOutputResponse{
		Stdout: string(out.Stdout), Stderr: string(out.Stderr), Next: out.Next,
		Truncated: out.Truncated, Process: toProcessResponse(&out.Process),
	})
}

// queryInt reads an optional integer query parameter, 0 when absent.
func queryInt(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, true
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", name+": must be an integer")
		return 0, false
	}
	return v, true
}

// WriteProcessStdin handles POST /v1/sandboxes/{id}/processes/{pid}/stdin. The
// body is the bytes to write.
func (h *Handler) WriteProcessStdin(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	pid, ok := pidFrom(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, app.MaxProcessStdinBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "read body")
		return
	}
	if err := h.svc.WriteProcessStdin(r.Context(), actor, r.PathValue("id"), pid, data); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CloseProcessStdin handles DELETE /v1/sandboxes/{id}/processes/{pid}/stdin.
func (h *Handler) CloseProcessStdin(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	pid, ok := pidFrom(w, r)
	if !ok {
		return
	}
	if err := h.svc.CloseProcessStdin(r.Context(), actor, r.PathValue("id"), pid); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type signalBody struct {
	Signal string `json:"signal"`
}

// SignalProcess handles POST /v1/sandboxes/{id}/processes/{pid}/signal.
func (h *Handler) SignalProcess(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	pid, ok := pidFrom(w, r)
	if !ok {
		return
	}
	var body signalBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	if err := h.svc.SignalProcess(r.Context(), actor, r.PathValue("id"), pid, body.Signal); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
