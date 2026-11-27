package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// secretResponse describes a secret. It has no field for the value: no route
// returns it.
type secretResponse struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toSecretResponse(s *domain.Secret) secretResponse {
	return secretResponse{Name: s.Name, CreatedAt: formatTime(s.CreatedAt), UpdatedAt: formatTime(s.UpdatedAt)}
}

// PutSecret handles PUT /v1/secrets/{name} {value}: 201 when it creates the
// secret, 200 when it replaces the value.
func (h *Handler) PutSecret(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	// A value is at most MaxEnvValueLen bytes; JSON escaping can multiply it by six.
	if err := json.NewDecoder(io.LimitReader(r.Body, 8*domain.MaxEnvValueLen)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	secret, created, err := h.svc.PutSecret(r.Context(), actor, r.PathValue("name"), req.Value)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, toSecretResponse(secret))
}

// ListSecrets handles GET /v1/secrets.
func (h *Handler) ListSecrets(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	secrets, err := h.svc.ListSecrets(r.Context(), actor)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	out := make([]secretResponse, 0, len(secrets))
	for _, s := range secrets {
		out = append(out, toSecretResponse(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": out})
}

// DeleteSecret handles DELETE /v1/secrets/{name}.
func (h *Handler) DeleteSecret(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteSecret(r.Context(), actor, r.PathValue("name")); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
