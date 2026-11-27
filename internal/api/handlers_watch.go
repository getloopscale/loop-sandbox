package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
)

type startWatchBody struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

type watchResponse struct {
	WatchID   string `json:"watch_id"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

// StartWatch handles POST /v1/sandboxes/{id}/watches.
func (h *Handler) StartWatch(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var body startWatchBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	id, clean, err := h.svc.StartWatch(r.Context(), actor, r.PathValue("id"), body.Path, body.Recursive)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, watchResponse{WatchID: id, Path: clean, Recursive: body.Recursive})
}

type fileEventResponse struct {
	Type  string `json:"type"`
	Path  string `json:"path,omitempty"`
	IsDir bool   `json:"is_dir"`
}

type watchEventsResponse struct {
	Events    []fileEventResponse `json:"events"`
	Next      int64               `json:"next"`
	Truncated bool                `json:"truncated"`
	Closed    bool                `json:"closed"`
}

// WatchEvents handles GET /v1/sandboxes/{id}/watches/{wid}/events.
func (h *Handler) WatchEvents(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
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
	if waitMS > app.MaxWatchEventsWait.Milliseconds() {
		waitMS = app.MaxWatchEventsWait.Milliseconds() + 1
	}
	events, err := h.svc.WatchEvents(r.Context(), actor, r.PathValue("id"), r.PathValue("wid"), after, time.Duration(waitMS)*time.Millisecond)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	out := watchEventsResponse{Next: events.Next, Truncated: events.Truncated, Closed: events.Closed,
		Events: make([]fileEventResponse, 0, len(events.Events))}
	for _, ev := range events.Events {
		out.Events = append(out.Events, fileEventResponse{Type: ev.Type, Path: ev.Path, IsDir: ev.IsDir})
	}
	writeJSON(w, http.StatusOK, out)
}

// StopWatch handles DELETE /v1/sandboxes/{id}/watches/{wid}.
func (h *Handler) StopWatch(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := h.svc.StopWatch(r.Context(), actor, r.PathValue("id"), r.PathValue("wid")); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
