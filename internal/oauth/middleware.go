package oauth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// ctxKey is unexported so external packages can't collide on the same
// context-key type even by accident.
type ctxKey int

const subjectKey ctxKey = 1

// Subject pulls the authenticated user out of the request context. Returns
// the empty string if the request was not gated by RequireBearer (e.g. a
// discovery endpoint).
func Subject(ctx context.Context) string {
	if v, ok := ctx.Value(subjectKey).(string); ok {
		return v
	}
	return ""
}

// RequireBearer wraps a handler such that requests without a valid Bearer
// JWT are rejected with 401 + a WWW-Authenticate header pointing the client
// at the protected-resource metadata URL (RFC 9728-style discovery, which is
// what MCP clients like ChatGPT use to recover from a missing/expired token).
func (s *Server) RequireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, "Bearer ") {
			s.writeUnauthorized(w, "invalid_request", "no bearer token in request")
			return
		}
		token := strings.TrimPrefix(authz, "Bearer ")
		sub, err := s.verifyJWT(token)
		if err != nil {
			s.writeUnauthorized(w, "invalid_token", err.Error())
			return
		}
		ctx := context.WithValue(r.Context(), subjectKey, sub)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) writeUnauthorized(w http.ResponseWriter, code, desc string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer realm="edookit-mcp", error=%q, error_description=%q, resource_metadata="%s/.well-known/oauth-protected-resource"`,
		code, desc, s.cfg.PublicURL,
	))
	writeJSONError(w, http.StatusUnauthorized, code, desc)
}
