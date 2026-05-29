package oauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OAuth wire-format constants. Re-declared as exported-looking package
// locals so the rest of the package never sprays magic strings.
const (
	responseTypeCode       = "code"
	grantAuthorizationCode = "authorization_code"
	grantRefreshToken      = "refresh_token"
	pkceMethodS256         = "S256"
	tokenTypeBearer        = "Bearer"
	scopeOfflineAccess     = "offline_access"
)

// Limits that bound the open-to-the-internet endpoints. Every value here is
// a defense against turning DCR or /authorize into a DoS / brute-force
// surface. Tuned for a single-user prototype — bump them if you're hosting
// multiple humans.
const (
	// maxRegisterBodyBytes caps a DCR request body so the open
	// /oauth/register endpoint cannot be used to drive arbitrary
	// CPU/memory in the JSON decoder.
	maxRegisterBodyBytes = 16 * 1024
	// maxRedirectURIs caps how many redirect_uris a single client
	// may list at registration time.
	maxRedirectURIs = 8
	// maxRedirectURILen caps each individual redirect_uri.
	maxRedirectURILen = 2048
	// maxClientNameLen caps the client_name display string.
	maxClientNameLen = 256
	// maxClients caps the registered-clients map. When at the cap a
	// new POST /oauth/register evicts the client with the oldest
	// IssuedAt so we never grow without bound.
	maxClients = 256

	// Login throttling — see ratelimit.go for the semantics.
	loginFailureWindow = 1 * time.Minute
	loginFailureMax    = 8
	loginBlockDuration = 15 * time.Minute
	loginFailureDelay  = 500 * time.Millisecond
)

// Config bundles the deployment-specific parameters of the OAuth server.
// Everything is wired from env vars at startup; the server is created once
// and serves until process exit.
type Config struct {
	// PublicURL is the canonical https origin the world sees, e.g.
	// "https://edookit.mcp.saiko.cz". All issued tokens use this as `iss`
	// and all discovery documents anchor their absolute URLs here.
	PublicURL string

	// LoginUsername is the human-facing login (shown in the form). May be
	// empty for prototype "anyone who knows the password gets in" mode.
	LoginUsername string

	// LoginPassword is the shared secret a human must type into the
	// authorize-page form. This is intentionally a separate secret from
	// the Edookit-side password — leaking one must not compromise the
	// other.
	LoginPassword string

	// JWTSecret is the HMAC-SHA256 key used to sign access tokens. Must be
	// at least 32 bytes for HS256 to be safe.
	JWTSecret []byte

	// Audience is the JWT `aud` claim — by convention the canonical URL of
	// the MCP endpoint itself, i.e. PublicURL + "/mcp".
	Audience string

	// AccessTTL bounds how long an access token is valid. Default 24h.
	AccessTTL time.Duration
	// RefreshTTL bounds how long a refresh token is valid. Default 30d.
	RefreshTTL time.Duration
	// CodeTTL bounds how long an authorization code is usable after
	// being issued. Default 60s (per OAuth 2.1 guidance: short-lived,
	// single-use).
	CodeTTL time.Duration
}

// Server is the in-memory OAuth Authorization Server. All client/code/refresh
// state lives in maps protected by `mu`. A single instance is safe for
// concurrent use; restarting the process drops all registrations (clients
// re-register on next ChatGPT connect — that is the expected behavior for an
// open-DCR setup with no persistence needs).
type Server struct {
	cfg Config

	mu       sync.Mutex
	clients  map[string]*ClientRegistration
	codes    map[string]*authCode
	refresh  map[string]*refreshRecord
	loginTpl *template.Template
	throttle *loginThrottle
	// now lets tests inject a deterministic clock; falls back to time.Now.
	clock func() time.Time
}

// ClientRegistration is the persisted form of an OAuth client. Public fields
// (uppercase) are serialized in the DCR response; lowercase ones are internal.
type ClientRegistration struct {
	ClientID     string    `json:"client_id"`
	ClientName   string    `json:"client_name,omitempty"`
	RedirectURIs []string  `json:"redirect_uris"`
	IssuedAt     time.Time `json:"-"`
	// IssuedAtUnix is the RFC 7591 `client_id_issued_at` field — Unix
	// seconds. Kept separate so the JSON shape matches the spec exactly.
	IssuedAtUnix int64 `json:"client_id_issued_at"`
	// TokenEndpointAuthMethod is always "none" — DCR'd clients are public
	// (PKCE). We don't issue client secrets.
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

type authCode struct {
	clientID    string
	redirectURI string
	challenge   string
	method      string
	scope       string
	sub         string
	expiresAt   time.Time
}

type refreshRecord struct {
	clientID  string
	sub       string
	scope     string
	expiresAt time.Time
}

// New constructs a Server with the given Config. Defaults are applied for
// any zero-valued TTLs. JWTSecret < 32 bytes is rejected: HS256 with a short
// key is brute-forceable.
func New(cfg Config) (*Server, error) {
	if cfg.PublicURL == "" {
		return nil, errors.New("PublicURL is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("cfg.Audience is required")
	}
	if cfg.LoginPassword == "" {
		return nil, errors.New("LoginPassword is required")
	}
	if len(cfg.JWTSecret) < 32 {
		return nil, fmt.Errorf("JWTSecret must be at least 32 bytes (got %d)", len(cfg.JWTSecret))
	}
	if cfg.AccessTTL == 0 {
		cfg.AccessTTL = 24 * time.Hour
	}
	if cfg.RefreshTTL == 0 {
		cfg.RefreshTTL = 30 * 24 * time.Hour
	}
	if cfg.CodeTTL == 0 {
		cfg.CodeTTL = 60 * time.Second
	}
	tpl, err := template.New("login").Parse(loginTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse login template: %w", err)
	}
	s := &Server{
		cfg:      cfg,
		clients:  map[string]*ClientRegistration{},
		codes:    map[string]*authCode{},
		refresh:  map[string]*refreshRecord{},
		loginTpl: tpl,
		clock:    time.Now,
	}
	s.throttle = newLoginThrottle(s.now)
	go s.gcLoop()
	return s, nil
}

func (s *Server) now() time.Time { return s.clock() }

// gcLoop periodically purges expired codes and refresh tokens. Cheap enough
// to run forever — the maps are tiny in steady state.
func (s *Server) gcLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		s.gc()
	}
}

func (s *Server) gc() {
	now := s.now()
	s.mu.Lock()
	for k, c := range s.codes {
		if now.After(c.expiresAt) {
			delete(s.codes, k)
		}
	}
	for k, r := range s.refresh {
		if now.After(r.expiresAt) {
			delete(s.refresh, k)
		}
	}
	s.mu.Unlock()
	s.throttle.gc()
}

// RegisterRoutes mounts the OAuth + discovery endpoints on the given mux.
// `/mcp` is NOT mounted here — it's the caller's responsibility to mount it
// wrapped in s.RequireBearer().
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleASMetadata)
	// RFC 9728 lets a client either probe the bare well-known path OR
	// derive a path-qualified one from the resource identifier
	// (`/.well-known/oauth-protected-resource{path-of-resource}`). Mount
	// both so the discovery survives regardless of which form the client
	// picks.
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.handleResourceMetadata)
	if p := s.protectedResourceMetadataPath(); p != "/.well-known/oauth-protected-resource" {
		mux.HandleFunc(p, s.handleResourceMetadata)
	}
	// Many OIDC libraries probe /.well-known/openid-configuration even when
	// we advertise only OAuth metadata; serve the same document so they
	// don't get confused.
	mux.HandleFunc("/.well-known/openid-configuration", s.handleASMetadata)
	mux.HandleFunc("/oauth/register", s.handleRegister)
	mux.HandleFunc("/oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("/oauth/token", s.handleToken)
}

// protectedResourceMetadataPath returns the RFC 9728 path-qualified
// well-known URL path for our resource. For audience
// `https://host/mcp` this yields `/.well-known/oauth-protected-resource/mcp`
// (so clients that pre-derive the URL from the resource identifier find it
// without needing to read our WWW-Authenticate header first).
func (s *Server) protectedResourceMetadataPath() string {
	const base = "/.well-known/oauth-protected-resource"
	u, err := url.Parse(s.cfg.Audience)
	if err != nil || u.Path == "" || u.Path == "/" {
		return base
	}
	return base + u.Path
}

// protectedResourceMetadataURL is the fully-qualified URL that should be
// emitted in WWW-Authenticate so MCP clients can fetch it directly.
func (s *Server) protectedResourceMetadataURL() string {
	return s.cfg.PublicURL + s.protectedResourceMetadataPath()
}

// --- handlers ---

func (s *Server) handleASMetadata(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"issuer":                                s.cfg.PublicURL,
		"authorization_endpoint":                s.cfg.PublicURL + "/oauth/authorize",
		"token_endpoint":                        s.cfg.PublicURL + "/oauth/token",
		"registration_endpoint":                 s.cfg.PublicURL + "/oauth/register",
		"response_types_supported":              []string{responseTypeCode},
		"grant_types_supported":                 []string{grantAuthorizationCode, grantRefreshToken},
		"code_challenge_methods_supported":      []string{pkceMethodS256},
		"token_endpoint_auth_methods_supported": []string{"none"},
		// Only advertise scopes we actually understand. `openid` is a
		// no-op acknowledged for OIDC compatibility; `offline_access`
		// gates whether a refresh_token is issued (handleTokenAuthCode
		// checks scope membership before returning one). The MCP
		// endpoint itself is gated by Bearer JWT presence, not by a
		// custom `mcp` scope, so we don't advertise one — that would
		// promise an enforcement RequireBearer doesn't actually do.
		"scopes_supported": []string{"openid", scopeOfflineAccess},
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"resource":                 s.cfg.Audience,
		"authorization_servers":    []string{s.cfg.PublicURL},
		"bearer_methods_supported": []string{"header"},
		// No custom scopes are required — Bearer JWT presence alone is
		// what gates /mcp. We deliberately omit `scopes_supported`
		// rather than advertise a scope name we don't enforce.
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// Cap the request body — DCR is open to the internet, so an unbounded
	// JSON decoder is a CPU/memory DoS vector. MaxBytesReader makes the
	// decoder surface ErrBodyTooLarge as a transparent decode error.
	r.Body = http.MaxBytesReader(w, r.Body, maxRegisterBodyBytes)
	var req struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}
	// We intentionally do NOT use DisallowUnknownFields — DCR clients
	// commonly send extra metadata (application_type, software_id,
	// software_version, contacts, ...) and rejecting any of those would
	// be brittle. The body cap above is what protects us from DoS.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "request body too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON body")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required and must be non-empty")
		return
	}
	if len(req.RedirectURIs) > maxRedirectURIs {
		writeJSONError(w, http.StatusBadRequest, "invalid_redirect_uri", fmt.Sprintf("too many redirect_uris (max %d)", maxRedirectURIs))
		return
	}
	if len(req.ClientName) > maxClientNameLen {
		writeJSONError(w, http.StatusBadRequest, "invalid_client_metadata", "client_name too long")
		return
	}
	for _, u := range req.RedirectURIs {
		if err := validateRedirectURI(u); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	id, err := randomToken(24)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "server_error", "could not generate client_id")
		return
	}
	now := s.now()
	reg := &ClientRegistration{
		ClientID:                "mcp_" + id,
		ClientName:              req.ClientName,
		RedirectURIs:            req.RedirectURIs,
		IssuedAt:                now,
		IssuedAtUnix:            now.Unix(),
		TokenEndpointAuthMethod: "none",
		// We are an authorization_code + refresh_token AS only. Echo
		// only what we actually support, regardless of what was asked.
		GrantTypes:    []string{grantAuthorizationCode, grantRefreshToken},
		ResponseTypes: []string{responseTypeCode},
	}
	s.mu.Lock()
	if len(s.clients) >= maxClients {
		s.evictOldestClientLocked()
	}
	s.clients[reg.ClientID] = reg
	s.mu.Unlock()
	log.Printf("oauth: registered client %s (name=%q, redirect_uris=%v)", reg.ClientID, reg.ClientName, reg.RedirectURIs)
	writeJSON(w, http.StatusCreated, reg)
}

// validateRedirectURI rejects anything that isn't an absolute http(s) URL
// with a host and no fragment. We can't trust /authorize's later
// "is-this-registered" check alone — if `javascript:` URIs survive
// registration, the very registration call leaks an XSS sink into the
// system; better to filter them at the door.
func validateRedirectURI(raw string) error {
	if len(raw) > maxRedirectURILen {
		return fmt.Errorf("redirect_uri too long (max %d)", maxRedirectURILen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("malformed redirect_uri %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
		// OK
	default:
		return fmt.Errorf("redirect_uri %q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("redirect_uri %q must have a host", raw)
	}
	if u.Fragment != "" {
		return fmt.Errorf("redirect_uri %q must not have a fragment", raw)
	}
	return nil
}

// evictOldestClientLocked drops the client with the oldest IssuedAt. Must be
// called with s.mu held. Linear scan is fine — maxClients is small (256).
func (s *Server) evictOldestClientLocked() {
	var oldestKey string
	var oldestAt time.Time
	for k, c := range s.clients {
		if oldestKey == "" || c.IssuedAt.Before(oldestAt) {
			oldestKey = k
			oldestAt = c.IssuedAt
		}
	}
	if oldestKey != "" {
		log.Printf("oauth: clients map at cap (%d) — evicting oldest %s (registered %s)", maxClients, oldestKey, oldestAt.Format(time.RFC3339))
		delete(s.clients, oldestKey)
	}
}

// handleAuthorize handles BOTH the GET (show login form) and POST (process
// login submission) phases of the user-delegated flow. Keeping them on the
// same path simplifies the form action — the original `?response_type=code…`
// query string is preserved across the POST via the form's hidden inputs.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleAuthorizeSubmit(w, r)
		return
	}
	s.handleAuthorizeShow(w, r)
}

// authorizeParams holds the fields extracted from a /authorize request and
// validated as a unit. Pulling parsing into one place keeps both the GET and
// POST handlers honest about what they require.
type authorizeParams struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

func parseAuthorize(r *http.Request) (authorizeParams, error) {
	if err := r.ParseForm(); err != nil {
		return authorizeParams{}, fmt.Errorf("parse form: %w", err)
	}
	p := authorizeParams{
		ResponseType:        r.Form.Get("response_type"),
		ClientID:            r.Form.Get("client_id"),
		RedirectURI:         r.Form.Get("redirect_uri"),
		Scope:               r.Form.Get("scope"),
		State:               r.Form.Get("state"),
		CodeChallenge:       r.Form.Get("code_challenge"),
		CodeChallengeMethod: r.Form.Get("code_challenge_method"),
	}
	if p.ResponseType != responseTypeCode {
		return p, fmt.Errorf("response_type must be %q", responseTypeCode)
	}
	if p.ClientID == "" {
		return p, errors.New("client_id is required")
	}
	if p.RedirectURI == "" {
		return p, errors.New("redirect_uri is required")
	}
	if p.CodeChallenge == "" {
		return p, errors.New("code_challenge is required (PKCE)")
	}
	if p.CodeChallengeMethod == "" {
		p.CodeChallengeMethod = "plain" // will be rejected below
	}
	if p.CodeChallengeMethod != pkceMethodS256 {
		return p, fmt.Errorf("code_challenge_method must be %s (got %q)", pkceMethodS256, p.CodeChallengeMethod)
	}
	return p, nil
}

// lookupClientForRedirect validates that the client exists and the supplied
// redirect_uri is among the URIs it registered. This is the single place we
// gate redirects — bad client_id or bad redirect_uri must NEVER 302 the
// browser to an attacker-controlled URL.
func (s *Server) lookupClientForRedirect(clientID, redirectURI string) (*ClientRegistration, error) {
	s.mu.Lock()
	c, ok := s.clients[clientID]
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("unknown client_id")
	}
	for _, u := range c.RedirectURIs {
		if u == redirectURI {
			return c, nil
		}
	}
	return nil, fmt.Errorf("redirect_uri %q is not registered for this client", redirectURI)
}

func (s *Server) handleAuthorizeShow(w http.ResponseWriter, r *http.Request) {
	p, err := parseAuthorize(r)
	if err != nil {
		http.Error(w, "invalid_request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.lookupClientForRedirect(p.ClientID, p.RedirectURI); err != nil {
		// Bad client / bad redirect_uri — render error inline, NEVER
		// 302 to the attacker-supplied URL.
		http.Error(w, "invalid_request: "+err.Error(), http.StatusBadRequest)
		return
	}
	data := loginPageData{
		Title:               "edookit-mcp",
		LoginHint:           s.cfg.LoginUsername,
		ClientID:            p.ClientID,
		RedirectURI:         p.RedirectURI,
		Scope:               p.Scope,
		State:               p.State,
		CodeChallenge:       p.CodeChallenge,
		CodeChallengeMethod: p.CodeChallengeMethod,
		ResponseType:        p.ResponseType,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.loginTpl.Execute(w, data); err != nil {
		log.Printf("oauth: render login: %v", err)
	}
}

func (s *Server) handleAuthorizeSubmit(w http.ResponseWriter, r *http.Request) {
	p, err := parseAuthorize(r)
	if err != nil {
		http.Error(w, "invalid_request: "+err.Error(), http.StatusBadRequest)
		return
	}
	client, err := s.lookupClientForRedirect(p.ClientID, p.RedirectURI)
	if err != nil {
		http.Error(w, "invalid_request: "+err.Error(), http.StatusBadRequest)
		return
	}

	ip := clientIP(r)
	if ok, retry := s.throttle.check(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Round(time.Second).Seconds())))
		// %q escapes any control bytes that an attacker-controlled
		// X-Forwarded-For could otherwise inject into the log stream.
		log.Printf("oauth: login blocked for %q (retry in %s)", ip, retry.Round(time.Second)) //nolint:gosec // G706: ip is %q-escaped to neutralize CRLF injection
		http.Error(w, "too many failed login attempts — try again later", http.StatusTooManyRequests)
		return
	}

	password := r.Form.Get("password")
	username := r.Form.Get("username")
	if !s.checkLogin(username, password) {
		// Fixed delay on every failure caps raw brute-force throughput
		// regardless of the per-IP counter (the counter handles bursts).
		time.Sleep(loginFailureDelay)
		s.throttle.recordFail(ip)
		log.Printf("oauth: login failure for %q (client=%s)", ip, client.ClientID) //nolint:gosec // G706: ip is %q-escaped; client.ClientID is server-generated
		// Re-render the form with an error. Do NOT redirect anywhere,
		// and do NOT distinguish "wrong username" from "wrong password"
		// in the message — a single "invalid credentials" stays opaque.
		data := loginPageData{
			Title:               "edookit-mcp",
			LoginHint:           s.cfg.LoginUsername,
			ClientID:            p.ClientID,
			RedirectURI:         p.RedirectURI,
			Scope:               p.Scope,
			State:               p.State,
			CodeChallenge:       p.CodeChallenge,
			CodeChallengeMethod: p.CodeChallengeMethod,
			ResponseType:        p.ResponseType,
			Error:               "Invalid credentials",
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		if err := s.loginTpl.Execute(w, data); err != nil {
			log.Printf("oauth: render login: %v", err)
		}
		return
	}

	// Login OK — clear any prior failure counter for this IP so a user
	// who fat-fingered then succeeded isn't penalized later, mint an
	// authorization code, 302.
	s.throttle.recordSuccess(ip)
	code, err := randomToken(32)
	if err != nil {
		http.Error(w, "server_error: could not mint code", http.StatusInternalServerError)
		return
	}
	now := s.now()
	s.mu.Lock()
	s.codes[code] = &authCode{
		clientID:    client.ClientID,
		redirectURI: p.RedirectURI,
		challenge:   p.CodeChallenge,
		method:      p.CodeChallengeMethod,
		scope:       p.Scope,
		sub:         s.subject(username),
		expiresAt:   now.Add(s.cfg.CodeTTL),
	}
	s.mu.Unlock()

	u, _ := url.Parse(p.RedirectURI)
	q := u.Query()
	q.Set("code", code)
	if p.State != "" {
		q.Set("state", p.State)
	}
	u.RawQuery = q.Encode()
	// OAuth best practice: don't let intermediaries cache the
	// code-bearing redirect.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// checkLogin compares the supplied credentials against the configured ones in
// constant time. An empty configured username means "ignore the username
// field, accept any value" (single-user mode).
func (s *Server) checkLogin(username, password string) bool {
	if s.cfg.LoginUsername != "" {
		if !constantTimeEqual(username, s.cfg.LoginUsername) {
			// Run the password check anyway so timing doesn't leak
			// which field was wrong.
			_ = constantTimeEqual(password, s.cfg.LoginPassword)
			return false
		}
	}
	return constantTimeEqual(password, s.cfg.LoginPassword)
}

// subject returns the JWT `sub` claim for a successful login. We prefer the
// configured login username; if blank, fall back to whatever the form
// submitted (still validated as the password), and finally to a fixed token
// so `sub` is never empty.
func (s *Server) subject(formUsername string) string {
	switch {
	case s.cfg.LoginUsername != "":
		return s.cfg.LoginUsername
	case formUsername != "":
		return formUsername
	default:
		return "edookit-mcp-user"
	}
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "could not parse form")
		return
	}
	switch r.Form.Get("grant_type") {
	case grantAuthorizationCode:
		s.handleTokenAuthCode(w, r)
	case grantRefreshToken:
		s.handleTokenRefresh(w, r)
	default:
		writeJSONError(w, http.StatusBadRequest, "unsupported_grant_type",
			fmt.Sprintf("grant_type %q is not supported (use authorization_code or refresh_token)", r.Form.Get("grant_type")))
	}
}

func (s *Server) handleTokenAuthCode(w http.ResponseWriter, r *http.Request) {
	code := r.Form.Get("code")
	redirectURI := r.Form.Get("redirect_uri")
	clientID := r.Form.Get("client_id")
	codeVerifier := r.Form.Get("code_verifier")
	if code == "" || redirectURI == "" || clientID == "" || codeVerifier == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"code, redirect_uri, client_id, code_verifier are all required")
		return
	}

	// Atomically remove the code (single-use per OAuth 2.1).
	s.mu.Lock()
	ac, ok := s.codes[code]
	if ok {
		delete(s.codes, code)
	}
	s.mu.Unlock()
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "unknown or already-used code")
		return
	}
	if s.now().After(ac.expiresAt) {
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "code expired")
		return
	}
	if ac.clientID != clientID {
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "client_id does not match the code")
		return
	}
	if ac.redirectURI != redirectURI {
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the code")
		return
	}
	if err := pkceVerify(codeVerifier, ac.challenge, ac.method); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}

	access, err := s.issueJWT(ac.sub, ac.scope)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "server_error", "could not mint access token")
		return
	}
	resp := map[string]any{
		"access_token": access,
		"token_type":   tokenTypeBearer,
		"expires_in":   int(s.cfg.AccessTTL.Seconds()),
		"scope":        ac.scope,
	}
	if scopeHas(ac.scope, scopeOfflineAccess) {
		rt, err := randomToken(48)
		if err != nil {
			// Surface this rather than silently swallowing — if the
			// client asked for offline_access and we said yes in the
			// authorize step, returning 200 without a refresh_token
			// is a contract break that's hard to debug on the client.
			writeJSONError(w, http.StatusInternalServerError, "server_error", "could not mint refresh token")
			return
		}
		s.mu.Lock()
		s.refresh[rt] = &refreshRecord{
			clientID:  ac.clientID,
			sub:       ac.sub,
			scope:     ac.scope,
			expiresAt: s.now().Add(s.cfg.RefreshTTL),
		}
		s.mu.Unlock()
		resp["refresh_token"] = rt
	}
	writeJSON(w, http.StatusOK, resp)
}

// scopeHas reports whether the space-delimited `scope` string contains the
// given token as an exact element. strings.Contains would match e.g.
// `x_offline_access_y` which is not the same scope.
func scopeHas(scope, want string) bool {
	for _, s := range strings.Fields(scope) {
		if s == want {
			return true
		}
	}
	return false
}

func (s *Server) handleTokenRefresh(w http.ResponseWriter, r *http.Request) {
	rt := r.Form.Get("refresh_token")
	clientID := r.Form.Get("client_id")
	if rt == "" || clientID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"refresh_token and client_id are required")
		return
	}
	s.mu.Lock()
	rec, ok := s.refresh[rt]
	s.mu.Unlock()
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "unknown refresh_token")
		return
	}
	if rec.clientID != clientID {
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "client_id does not match the refresh_token")
		return
	}
	if s.now().After(rec.expiresAt) {
		s.mu.Lock()
		delete(s.refresh, rt)
		s.mu.Unlock()
		writeJSONError(w, http.StatusBadRequest, "invalid_grant", "refresh_token expired")
		return
	}
	access, err := s.issueJWT(rec.sub, rec.scope)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "server_error", "could not mint access token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access,
		"token_type":   tokenTypeBearer,
		"expires_in":   int(s.cfg.AccessTTL.Seconds()),
		"scope":        rec.scope,
	})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

func randomToken(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// constantTimeEqual compares two strings in (close to) constant time using
// crypto/subtle. Note: ConstantTimeCompare itself returns 0 on
// length mismatch — that's a known small leak shared by the stdlib helper
// and any naive constant-time compare. It is acceptable here because the
// expected secret length is fixed by deployment config (env var), not by
// an external attacker observation.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
