package api

import "net/http"

type capabilityResponse struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
}

type capabilitiesResponse struct {
	Driver       string               `json:"driver"`
	Capabilities []capabilityResponse `json:"capabilities"`
}

// Capabilities handles GET /v1/capabilities: which optional features the runtime
// driver of this deployment supports. A route whose capability is unsupported
// answers 501, and this endpoint lets a client know that in advance.
func (h *Handler) Capabilities(w http.ResponseWriter, r *http.Request) {
	if _, ok := actorFrom(w, r); !ok {
		return
	}
	driver, caps := h.svc.Capabilities()
	out := capabilitiesResponse{Driver: driver, Capabilities: make([]capabilityResponse, 0, len(caps))}
	for _, c := range caps {
		out.Capabilities = append(out.Capabilities, capabilityResponse{Name: string(c.Name), Supported: c.Supported})
	}
	writeJSON(w, http.StatusOK, out)
}
