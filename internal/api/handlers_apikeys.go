package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

type apiKeyResponse struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Prefix    string   `json:"prefix"`
	Scopes    []string `json:"scopes"`
	CreatedAt string   `json:"created_at"`
	RevokedAt string   `json:"revoked_at,omitempty"`
	// Key is the secret. It is present only in the response that creates the key.
	Key string `json:"key,omitempty"`
}

func toAPIKeyResponse(k *domain.APIKey) apiKeyResponse {
	out := apiKeyResponse{ID: k.ID, Name: k.Name, Prefix: k.Prefix, Scopes: k.Scopes, CreatedAt: formatTime(k.CreatedAt)}
	if out.Scopes == nil {
		out.Scopes = []string{}
	}
	if k.RevokedAt != nil {
		out.RevokedAt = formatTime(*k.RevokedAt)
	}
	return out
}

type createAPIKeyRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes,omitempty"`
}

// CreateAPIKey handles POST /v1/api-keys. The response is the only place the
// secret ever appears.
func (h *Handler) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var req createAPIKeyRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	raw, key, err := h.svc.CreateAPIKey(r.Context(), actor, req.Name, req.Scopes)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	out := toAPIKeyResponse(key)
	out.Key = raw
	// The body holds a secret: no cache may keep it.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, out)
}

// ListAPIKeys handles GET /v1/api-keys.
func (h *Handler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	keys, err := h.svc.ListAPIKeys(r.Context(), actor)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	out := make([]apiKeyResponse, 0, len(keys))
	for _, k := range keys {
		out = append(out, toAPIKeyResponse(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_keys": out})
}

// RevokeAPIKey handles DELETE /v1/api-keys/{kid}.
func (h *Handler) RevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := h.svc.RevokeAPIKey(r.Context(), actor, r.PathValue("kid")); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
