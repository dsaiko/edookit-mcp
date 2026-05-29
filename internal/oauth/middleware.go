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
		token, ok := parseBearer(r.Header.Get("Authorization"))
		if !ok {
			s.writeUnauthorized(w, "invalid_request", "no bearer token in request")
			return
		}
		sub, err := s.verifyJWT(token)
		if err != nil {
			s.writeUnauthorized(w, "invalid_token", err.Error())
			return
		}
		ctx := context.WithValue(r.Context(), subjectKey, sub)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// parseBearer extracts a Bearer token from an Authorization header value
// in a way that is forgiving about case (RFC 7235 says scheme tokens are
// case-insensitive) and about whitespace (split on any LWS — SP / HTAB —
// since some HTTP clients normalise headers unhelpfully). Returns
// (token, true) on success; ("", false) on anything else.
//
// strings.Fields splits on any Unicode whitespace, which is a strict
// superset of the LWS set we care about. A well-formed token has no
// internal whitespace, so requiring exactly two fields gives us "scheme +
// credentials" while still rejecting clearly malformed inputs like
// "Bearer abc def" or a bare "Bearer".
func parseBearer(authz string) (string, bool) {
	const scheme = "bearer"
	fields := strings.Fields(authz)
	if len(fields) != 2 || !strings.EqualFold(fields[0], scheme) {
		return "", false
	}
	return fields[1], true
}

func (s *Server) writeUnauthorized(w http.ResponseWriter, code, desc string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer realm="edookit-mcp", error=%q, error_description=%q, resource_metadata=%q`,
		code, desc, s.protectedResourceMetadataURL(),
	))
	writeJSONError(w, http.StatusUnauthorized, code, desc)
}
