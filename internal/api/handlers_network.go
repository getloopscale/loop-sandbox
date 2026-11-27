package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// networkBody is the outbound rules of a sandbox as the API reads and writes them.
type networkBody struct {
	AllowOut []string `json:"allow_out"`
	DenyOut  []string `json:"deny_out"`
}

// policy returns the rules of a request body, or nil when the body had none.
func (n *networkBody) policy() *runtime.NetworkPolicy {
	if n == nil {
		return nil
	}
	return &runtime.NetworkPolicy{AllowOut: n.AllowOut, DenyOut: n.DenyOut}
}

// networkOf returns the rules of a sandbox, or nil when it has the default policy.
func networkOf(s *domain.Sandbox) *networkBody {
	if len(s.NetworkAllowOut) == 0 && len(s.NetworkDenyOut) == 0 {
		return nil
	}
	return &networkBody{AllowOut: orEmpty(s.NetworkAllowOut), DenyOut: orEmpty(s.NetworkDenyOut)}
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// UpdateSandboxNetwork handles PUT /v1/sandboxes/{id}/network: the body replaces
// the outbound rules of a running sandbox.
func (h *Handler) UpdateSandboxNetwork(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	var req networkBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}
	sb, err := h.svc.UpdateSandboxNetwork(r.Context(), actor, r.PathValue("id"), *req.policy())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, networkBody{AllowOut: orEmpty(sb.NetworkAllowOut), DenyOut: orEmpty(sb.NetworkDenyOut)})
}

type networkDenial struct {
	Time        string `json:"time"`
	Destination string `json:"destination"`
	Host        string `json:"host,omitempty"`
	Reason      string `json:"reason"`
}

type networkDenialsResponse struct {
	Denials []networkDenial `json:"denials"`
}

// ListNetworkDenials handles GET /v1/sandboxes/{id}/network/denials: the most
// recent connections the sandbox's rules refused, oldest first.
func (h *Handler) ListNetworkDenials(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n == 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit: must be between 1 and 1000")
			return
		}
		limit = n
	}
	denials, err := h.svc.SandboxNetworkDenials(r.Context(), actor, r.PathValue("id"), limit)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	out := networkDenialsResponse{Denials: make([]networkDenial, 0, len(denials))}
	for _, d := range denials {
		out.Denials = append(out.Denials, networkDenial{
			Time: d.Time.UTC().Format(time.RFC3339Nano), Destination: d.Destination, Host: d.Host, Reason: d.Reason,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
