package auth

import (
	"encoding/json"
	"net/http"
)

// Middleware authenticates the Authorization header and injects the Principal into
// the request context. On failure it writes 401 and stops the chain (fail-closed).
func Middleware(a TenantAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := a.Authenticate(r.Context(), r.Header.Get("Authorization"))
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{"code": "unauthorized", "message": "invalid or missing API key"},
				})
				return
			}
			next.ServeHTTP(w, WithPrincipalRequest(r, p))
		})
	}
}

// WithPrincipalRequest returns r with the principal in its context.
func WithPrincipalRequest(r *http.Request, p *Principal) *http.Request {
	return r.WithContext(WithPrincipal(r.Context(), p))
}
