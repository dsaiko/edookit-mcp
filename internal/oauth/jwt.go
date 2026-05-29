// Package oauth implements a minimal OAuth 2.1 Authorization Server + Bearer
// JWT resource-server middleware tailored to the edookit-mcp single-user
// remote-MCP deployment.
//
// Access tokens are HMAC-SHA256-signed JWTs (HS256). Symmetric keys are fine
// because the same process issues and validates the tokens — there is no
// separate verifier that would benefit from public-key verification.
package oauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// jwtHeader is the fixed HS256 JWT header — emitted as-is, never parsed back.
// Keeping it as a literal makes the hot path allocation-free.
const jwtHeader = `{"alg":"HS256","typ":"JWT"}`

// jwtClaims is the access-token payload. Fields are the standard registered
// claims (RFC 7519) plus a free-form `scope` so the resource handler can see
// what the token was issued for.
type jwtClaims struct {
	Iss   string `json:"iss"`
	Sub   string `json:"sub"`
	Aud   string `json:"aud"`
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
	Jti   string `json:"jti"`
	Scope string `json:"scope,omitempty"`
}

// issueJWT produces a signed access token for the given subject and scope.
// Expiry is now + cfg.AccessTTL; iat/jti are filled in here.
func (s *Server) issueJWT(sub, scope string) (string, error) {
	now := s.now()
	jti, err := randomToken(16)
	if err != nil {
		return "", fmt.Errorf("jti: %w", err)
	}
	claims := jwtClaims{
		Iss:   s.cfg.PublicURL,
		Sub:   sub,
		Aud:   s.cfg.Audience,
		Iat:   now.Unix(),
		Exp:   now.Add(s.cfg.AccessTTL).Unix(),
		Jti:   jti,
		Scope: scope,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}
	headerB64 := base64.RawURLEncoding.EncodeToString([]byte(jwtHeader))
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	signing := headerB64 + "." + payloadB64
	mac := hmac.New(sha256.New, s.cfg.JWTSecret)
	mac.Write([]byte(signing))
	sigB64 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signing + "." + sigB64, nil
}

// verifyJWT parses, signature-verifies and claim-checks a token issued by this
// server. Returns the subject ("sub" claim) on success. On any failure the
// caller should treat the request as anonymous — the error string is intended
// for the WWW-Authenticate error_description, not for debug logs.
func (s *Server) verifyJWT(tok string) (string, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed JWT")
	}
	headerB64, payloadB64, sigB64 := parts[0], parts[1], parts[2]

	// Constant-time signature compare via hmac.Equal: avoids timing oracle.
	mac := hmac.New(sha256.New, s.cfg.JWTSecret)
	mac.Write([]byte(headerB64 + "." + payloadB64))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sigB64), []byte(expectedSig)) {
		return "", errors.New("bad signature")
	}

	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return "", fmt.Errorf("decode payload: %w", err)
	}
	var c jwtClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", fmt.Errorf("parse payload: %w", err)
	}
	if c.Iss != s.cfg.PublicURL {
		return "", errors.New("issuer mismatch")
	}
	if c.Aud != s.cfg.Audience {
		return "", errors.New("audience mismatch")
	}
	now := s.now().Unix()
	if c.Exp < now {
		return "", errors.New("expired")
	}
	// 5s leeway on iat to absorb clock drift between issuer and verifier
	// (here the same process — drift is zero by construction, but it keeps
	// the check symmetric with exp and makes the contract explicit).
	if c.Iat > now+5 {
		return "", errors.New("issued in the future")
	}
	return c.Sub, nil
}

// pkceVerify confirms that SHA256(verifier) base64url-encoded equals challenge.
// Method must be "S256"; "plain" is rejected per OAuth 2.1.
func pkceVerify(verifier, challenge, method string) error {
	if method != pkceMethodS256 {
		return fmt.Errorf("unsupported code_challenge_method: %s", method)
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	if !hmac.Equal([]byte(got), []byte(challenge)) {
		return errors.New("PKCE verifier does not match challenge")
	}
	return nil
}
