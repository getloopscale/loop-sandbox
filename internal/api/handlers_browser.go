package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/getloopscale/loop-sandbox/internal/app"
)

// browserErr maps M18 browser errors to HTTP status.
func browserErr(w http.ResponseWriter, err error) {
	if errors.Is(err, app.ErrBrowserUnsupported) {
		writeError(w, http.StatusNotImplemented, "not_implemented", "the browser/computer-use surface is not available on this runtime")
		return
	}
	writeDomainError(w, err)
}

// BrowserNavigate handles POST /v1/sandboxes/{id}/browser/navigate {url}.
func (h *Handler) BrowserNavigate(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.URL == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "url is required")
		return
	}
	if err := h.svc.BrowserNavigate(r.Context(), actor, r.PathValue("id"), body.URL); err != nil {
		browserErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// BrowserScreenshot handles GET /v1/sandboxes/{id}/browser/screenshot — returns a PNG.
func (h *Handler) BrowserScreenshot(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	png, err := h.svc.BrowserScreenshot(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		browserErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// BrowserDOM handles GET /v1/sandboxes/{id}/browser/dom — returns the DOM snapshot HTML.
func (h *Handler) BrowserDOM(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	dom, err := h.svc.BrowserDOM(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		browserErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dom": dom})
}

// BrowserInput handles POST /v1/sandboxes/{id}/browser/input {action, x, y, dx, dy, text}.
func (h *Handler) BrowserInput(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		Action string  `json:"action"` // click | type | scroll
		X      float64 `json:"x,omitempty"`
		Y      float64 `json:"y,omitempty"`
		DX     float64 `json:"dx,omitempty"`
		DY     float64 `json:"dy,omitempty"`
		Text   string  `json:"text,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	id := r.PathValue("id")
	var err error
	switch body.Action {
	case "click":
		err = h.svc.BrowserClick(r.Context(), actor, id, body.X, body.Y)
	case "type":
		err = h.svc.BrowserType(r.Context(), actor, id, body.Text)
	case "scroll":
		err = h.svc.BrowserScroll(r.Context(), actor, id, body.X, body.Y, body.DX, body.DY)
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "action must be click|type|scroll")
		return
	}
	if err != nil {
		browserErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
