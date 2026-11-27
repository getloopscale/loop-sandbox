package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

type sandboxListResponse struct {
	Sandboxes []sandboxResponse `json:"sandboxes"`
	// NextCursor is present when more sandboxes follow this page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// cursorWire is the JSON inside the opaque list cursor.
type cursorWire struct {
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"id"`
}

func encodeCursor(c domain.SandboxCursor) string {
	raw, _ := json.Marshal(cursorWire{CreatedAt: c.CreatedAt.UTC(), ID: c.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (*domain.SandboxCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c cursorWire
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.ID == "" || c.CreatedAt.IsZero() {
		return nil, domain.ErrInvalidArgument
	}
	return &domain.SandboxCursor{CreatedAt: c.CreatedAt, ID: c.ID}, nil
}

// parseSandboxQuery reads the list filters from the query string: state (may
// repeat, or hold a comma-separated list), metadata (key=value, may repeat),
// limit and cursor.
func parseSandboxQuery(r *http.Request) (domain.SandboxQuery, *domain.ValidationError) {
	var q domain.SandboxQuery
	params := r.URL.Query()
	for _, raw := range params["state"] {
		for _, st := range strings.Split(raw, ",") {
			if st = strings.TrimSpace(st); st != "" {
				q.States = append(q.States, domain.State(st))
			}
		}
	}
	for _, pair := range params["metadata"] {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return q, &domain.ValidationError{Field: "metadata", Reason: "filter must be key=value"}
		}
		if q.Metadata == nil {
			q.Metadata = map[string]string{}
		}
		q.Metadata[key] = value
	}
	if raw := params.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return q, &domain.ValidationError{Field: "limit", Reason: "must be between 1 and 200"}
		}
		q.Limit = n
	}
	if raw := params.Get("cursor"); raw != "" {
		c, err := decodeCursor(raw)
		if err != nil {
			return q, &domain.ValidationError{Field: "cursor", Reason: "is not a cursor returned by this API"}
		}
		q.After = c
	}
	return q, nil
}

// ListSandboxes handles GET /v1/sandboxes: one page of the tenant's sandboxes,
// newest first.
func (h *Handler) ListSandboxes(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	q, verr := parseSandboxQuery(r)
	if verr != nil {
		writeDomainError(w, verr)
		return
	}
	page, err := h.svc.ListSandboxes(r.Context(), actor, q)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	out := sandboxListResponse{Sandboxes: make([]sandboxResponse, 0, len(page.Sandboxes))}
	for _, sb := range page.Sandboxes {
		out.Sandboxes = append(out.Sandboxes, toResponse(sb))
	}
	if page.Next != nil {
		out.NextCursor = encodeCursor(*page.Next)
	}
	writeJSON(w, http.StatusOK, out)
}

type timeoutRequest struct {
	TTLSeconds int64 `json:"ttl_seconds"`
}

// SetSandboxTimeout handles POST /v1/sandboxes/{id}/timeout: the sandbox now
// expires ttl_seconds from the time of the request.
func (h *Handler) SetSandboxTimeout(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var req timeoutRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	sb, err := h.svc.SetSandboxTTL(r.Context(), actor, r.PathValue("id"), time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(sb))
}
