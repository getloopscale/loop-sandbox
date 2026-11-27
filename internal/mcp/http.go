package mcp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/mark3labs/mcp-go/server"

	"github.com/getloopscale/loop-sandbox/internal/auth"
)

// NewHTTPHandler wraps the MCP streamable-HTTP server with authentication: it
// resolves the caller's credential to a Principal (the SAME TenantAuthenticator as
// REST → same tenant on both surfaces), injects it into the request context so the
// tool filter and handlers can read it, and fails CLOSED (401) when unauthenticated.
func NewHTTPHandler(mcpSrv *server.MCPServer, authenticator auth.TenantAuthenticator) http.Handler {
	streamable := server.NewStreamableHTTPServer(mcpSrv,
		// The context here already carries the Principal (injected by the outer
		// middleware below), so the tool filter / handlers see it.
		server.WithHTTPContextFunc(func(ctx context.Context, _ *http.Request) context.Context {
			return ctx
		}),
	)
	return authMiddleware(authenticator, streamable)
}

// authMiddleware authenticates and injects the Principal, or 401s (fail-closed).
func authMiddleware(a auth.TenantAuthenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Authenticate(r.Context(), r.Header.Get("Authorization"))
		if err != nil || p == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "unauthorized", "message": "invalid or missing credential"},
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}
