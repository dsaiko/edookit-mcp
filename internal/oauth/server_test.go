package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestServer wires up an in-memory Server with safe-but-tiny TTLs and a
// fixed clock so expiry tests are deterministic. Returns the server and a
// clock pointer the caller can advance.
func newTestServer(t *testing.T) (*Server, *time.Time) {
	t.Helper()
	now := time.Date(2026, 5, 29, 10, 0, 0, 0, time.UTC)
	clock := &now
	s, err := New(Config{
		PublicURL:     "https://mcp.example",
		Audience:      "https://mcp.example/mcp",
		LoginUsername: "alice",
		LoginPassword: "hunter2",
		// 32 zero bytes is fine for tests; production validates min length.
		JWTSecret:  make([]byte, 32),
		AccessTTL:  time.Hour,
		RefreshTTL: 24 * time.Hour,
		CodeTTL:    time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.clock = func() time.Time { return *clock }
	return s, clock
}

func TestJWTRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	tok, err := s.issueJWT("alice", "mcp openid")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	sub, err := s.verifyJWT(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if sub != "alice" {
		t.Errorf("sub: want alice, got %q", sub)
	}
}

func TestJWTRejectsTamperedSignature(t *testing.T) {
	s, _ := newTestServer(t)
	tok, _ := s.issueJWT("alice", "")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("tok shape: %q", tok)
	}
	// Flip a byte in the signature.
	bad := parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-1] + "A"
	if _, err := s.verifyJWT(bad); err == nil {
		t.Fatal("expected bad-signature error")
	}
}

func TestJWTRejectsWrongAudience(t *testing.T) {
	s, _ := newTestServer(t)
	// Swap the audience by directly minting via a second server with the
	// same secret but a different audience. The first server should reject.
	other, _ := New(Config{
		PublicURL:     s.cfg.PublicURL,
		Audience:      "https://other.example/mcp",
		LoginPassword: "x",
		JWTSecret:     s.cfg.JWTSecret,
		LoginUsername: "alice",
	})
	other.clock = s.clock
	tok, _ := other.issueJWT("alice", "")
	if _, err := s.verifyJWT(tok); err == nil {
		t.Fatal("expected audience mismatch")
	}
}

func TestJWTRejectsExpired(t *testing.T) {
	s, clock := newTestServer(t)
	tok, _ := s.issueJWT("alice", "")
	*clock = clock.Add(2 * time.Hour) // beyond AccessTTL
	if _, err := s.verifyJWT(tok); err == nil {
		t.Fatal("expected expired error")
	}
}

func TestPKCEVerify(t *testing.T) {
	verifier := "the-quick-brown-fox-jumps-over-the-lazy-dog-12345"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if err := pkceVerify(verifier, challenge, "S256"); err != nil {
		t.Errorf("expected match, got %v", err)
	}
	if err := pkceVerify("other-verifier", challenge, "S256"); err == nil {
		t.Error("expected mismatch")
	}
	if err := pkceVerify(verifier, challenge, "plain"); err == nil {
		t.Error("plain method must be rejected")
	}
}

// noRedirectClient builds an http.Client that returns 3xx responses as-is so
// the test can inspect the redirect Location instead of following it.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// doJSON is a context-aware POST application/json helper. It centralizes the
// req-construction boilerplate so the test bodies stay focused on what
// they're actually asserting.
func doJSON(t *testing.T, cli *http.Client, urlStr, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, urlStr, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", urlStr, err)
	}
	return resp
}

// doForm is a context-aware POST application/x-www-form-urlencoded helper.
func doForm(t *testing.T, cli *http.Client, urlStr string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, urlStr, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("post-form %s: %v", urlStr, err)
	}
	return resp
}

// doGet is a context-aware GET helper.
func doGet(t *testing.T, cli *http.Client, urlStr string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, urlStr, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", urlStr, err)
	}
	return resp
}

// TestFullFlow drives a complete DCR → /authorize → /token round trip via
// httptest and asserts the access token returned is good for /mcp.
func TestFullFlow(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	// Mount a tiny "MCP" handler behind the bearer middleware so we can
	// confirm the token actually works end-to-end.
	mux.Handle("/mcp", s.RequireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok subject="+Subject(r.Context()))
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cli := noRedirectClient()

	// 1. DCR — register a client with a unique redirect_uri.
	regBody := `{"client_name":"test","redirect_uris":["` + srv.URL + `/callback"]}`
	resp := doJSON(t, cli, srv.URL+"/oauth/register", regBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("register status %d: %s", resp.StatusCode, body)
	}
	var reg ClientRegistration
	_ = json.NewDecoder(resp.Body).Decode(&reg)
	if !strings.HasPrefix(reg.ClientID, "mcp_") {
		t.Fatalf("client_id shape: %q", reg.ClientID)
	}

	// 2. PKCE — pick a verifier, compute challenge.
	verifier := "test-verifier-test-verifier-test-verifier-test"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// 3. POST /oauth/authorize with credentials + PKCE params.
	form := url.Values{}
	form.Set("response_type", "code")
	form.Set("client_id", reg.ClientID)
	form.Set("redirect_uri", srv.URL+"/callback")
	form.Set("scope", "openid offline_access mcp")
	form.Set("state", "xyz")
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")
	form.Set("username", "alice")
	form.Set("password", "hunter2")
	resp2 := doForm(t, cli, srv.URL+"/oauth/authorize", form)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("authorize status %d: %s", resp2.StatusCode, body)
	}
	loc, err := resp2.Location()
	if err != nil {
		t.Fatalf("authorize no Location: %v", err)
	}
	if loc.Query().Get("state") != "xyz" {
		t.Errorf("state lost: %v", loc)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in redirect: %v", loc)
	}

	// 4. POST /oauth/token to exchange code for JWT.
	tokForm := url.Values{}
	tokForm.Set("grant_type", "authorization_code")
	tokForm.Set("code", code)
	tokForm.Set("redirect_uri", srv.URL+"/callback")
	tokForm.Set("client_id", reg.ClientID)
	tokForm.Set("code_verifier", verifier)
	resp3 := doForm(t, cli, srv.URL+"/oauth/token", tokForm)
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp3.Body)
		t.Fatalf("token status %d: %s", resp3.StatusCode, body)
	}
	var tokResp struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}
	_ = json.NewDecoder(resp3.Body).Decode(&tokResp)
	if tokResp.TokenType != "Bearer" {
		t.Errorf("token_type: %q", tokResp.TokenType)
	}
	if tokResp.AccessToken == "" {
		t.Fatal("no access_token")
	}
	if tokResp.RefreshToken == "" {
		t.Error("expected refresh_token because offline_access was in scope")
	}

	// 5. Hit /mcp with the token — should get 200 and Subject.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/mcp", http.NoBody)
	if err != nil {
		t.Fatalf("build mcp request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokResp.AccessToken)
	resp4, err := cli.Do(req)
	if err != nil {
		t.Fatalf("mcp call: %v", err)
	}
	defer resp4.Body.Close()
	body, _ := io.ReadAll(resp4.Body)
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("mcp status %d: %s", resp4.StatusCode, body)
	}
	if !strings.Contains(string(body), "subject=alice") {
		t.Errorf("body did not carry subject: %s", body)
	}
}

// TestAuthorizeRejectsBadRedirect ensures we never 302 to an attacker-supplied
// redirect_uri that wasn't registered.
func TestAuthorizeRejectsBadRedirect(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cli := noRedirectClient()

	// Register a client with one URL.
	regBody := `{"client_name":"x","redirect_uris":["https://good.example/cb"]}`
	resp := doJSON(t, cli, srv.URL+"/oauth/register", regBody)
	defer resp.Body.Close()
	var reg ClientRegistration
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		t.Fatalf("decode reg: %v", err)
	}

	// Now hit /authorize with a DIFFERENT redirect_uri. Must NOT 302 anywhere.
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", reg.ClientID)
	q.Set("redirect_uri", "https://evil.example/cb")
	q.Set("code_challenge", "x")
	q.Set("code_challenge_method", "S256")
	r2 := doGet(t, cli, srv.URL+"/oauth/authorize?"+q.Encode())
	defer r2.Body.Close()
	if r2.StatusCode == http.StatusFound {
		t.Fatalf("must NOT redirect to attacker's URL; got 302 -> %s", r2.Header.Get("Location"))
	}
	if r2.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", r2.StatusCode)
	}
}

func TestBearerMissing(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.RequireBearer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", http.NoBody)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rr.Code)
	}
	auth := rr.Header().Get("WWW-Authenticate")
	if !strings.Contains(auth, "resource_metadata=") {
		t.Errorf("WWW-Authenticate missing resource_metadata: %q", auth)
	}
	// The path-qualified URL is what RFC 9728 derive-clients construct;
	// emit the same so we never disagree with them.
	if !strings.Contains(auth, "/.well-known/oauth-protected-resource/mcp") {
		t.Errorf("WWW-Authenticate should point at path-qualified well-known: %q", auth)
	}
}

func TestParseBearerCaseInsensitive(t *testing.T) {
	for _, tc := range []struct {
		hdr      string
		wantTok  string
		wantPass bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER abc", "abc", true},
		{"bEaReR    abc", "abc", true},
		{"Bearer\tabc", "abc", true},     // HTAB instead of SP
		{"\tBearer\tabc\t", "abc", true}, // surrounding/internal LWS
		{"Bearer abc def", "", false},    // token must have no internal whitespace
		{"Basic abc", "", false},
		{"abc", "", false},
		{"", "", false},
		{"Bearer ", "", false},
	} {
		tok, ok := parseBearer(tc.hdr)
		if tok != tc.wantTok || ok != tc.wantPass {
			t.Errorf("parseBearer(%q) = (%q, %v); want (%q, %v)", tc.hdr, tok, ok, tc.wantTok, tc.wantPass)
		}
	}
}

func TestPathQualifiedResourceMetadataServed(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cli := noRedirectClient()
	// Audience = https://mcp.example/mcp ⇒ path-qualified URL must serve
	// the same metadata document as the bare well-known.
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		resp := doGet(t, cli, srv.URL+path)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: want 200, got %d (%s)", path, resp.StatusCode, body)
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Errorf("%s: bad JSON: %v", path, err)
			continue
		}
		if doc["resource"] != "https://mcp.example/mcp" {
			t.Errorf("%s: resource = %v", path, doc["resource"])
		}
	}
}

func TestValidateRedirectURI(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"https any host", "https://chatgpt.com/cb", false},
		{"https with port", "https://example.com:8443/cb", false},
		{"http localhost", "http://localhost:8080/cb", false},
		{"http 127.0.0.1", "http://127.0.0.1:8080/cb", false},
		{"http 127.4.5.6", "http://127.4.5.6/cb", false}, // any 127/8
		{"http IPv6 loopback", "http://[::1]:8080/cb", false},
		{"http public host", "http://example.com/cb", true},
		{"javascript scheme", "javascript:alert(1)", true},
		{"data scheme", "data:text/html,abc", true},
		{"file scheme", "file:///etc/passwd", true},
		{"missing host", "https:///cb", true},
		{"with fragment", "https://example.com/cb#x", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRedirectURI(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateRedirectURI(%q) err=%v; wantErr=%v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestRegisterRejectsBadRedirectScheme(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cli := noRedirectClient()
	// `javascript:` URIs are valid url.Parse but unsafe as redirect targets.
	resp := doJSON(t, cli, srv.URL+"/oauth/register",
		`{"client_name":"x","redirect_uris":["javascript:alert(1)"]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for javascript: URI, got %d", resp.StatusCode)
	}
}

func TestRegisterRejectsOversizedBody(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cli := noRedirectClient()
	// Build a body well over the 16KB cap.
	junk := strings.Repeat("A", maxRegisterBodyBytes*2)
	body := `{"client_name":"` + junk + `","redirect_uris":["https://example.com/cb"]}`
	resp := doJSON(t, cli, srv.URL+"/oauth/register", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("want 413, got %d", resp.StatusCode)
	}
}

func TestRegisterEvictsOldestAtCap(t *testing.T) {
	s, clock := newTestServer(t)
	// Fill the map to the cap.
	for i := range maxClients {
		*clock = clock.Add(time.Millisecond)
		s.mu.Lock()
		s.clients[fmt.Sprintf("mcp_filler_%d", i)] = &ClientRegistration{
			ClientID: fmt.Sprintf("mcp_filler_%d", i),
			IssuedAt: *clock,
		}
		s.mu.Unlock()
	}
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// One more registration should succeed AND drop the oldest.
	*clock = clock.Add(time.Second)
	resp := doJSON(t, noRedirectClient(), srv.URL+"/oauth/register",
		`{"client_name":"latest","redirect_uris":["https://example.com/cb"]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d", resp.StatusCode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) > maxClients {
		t.Errorf("clients map exceeded cap: %d > %d", len(s.clients), maxClients)
	}
	if _, exists := s.clients["mcp_filler_0"]; exists {
		t.Error("oldest filler was not evicted")
	}
}

func TestLoginThrottleBlocksAfterNFailures(t *testing.T) {
	s, _ := newTestServer(t)
	// Make the failure delay zero so the test isn't slow.
	s.throttle.failureDelay = 0
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Register a client first.
	cli := noRedirectClient()
	resp := doJSON(t, cli, srv.URL+"/oauth/register",
		`{"client_name":"x","redirect_uris":["https://example.com/cb"]}`)
	defer resp.Body.Close()
	var reg ClientRegistration
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		t.Fatalf("decode reg: %v", err)
	}

	authURL := srv.URL + "/oauth/authorize"
	doFail := func() int {
		form := url.Values{}
		form.Set("response_type", "code")
		form.Set("client_id", reg.ClientID)
		form.Set("redirect_uri", "https://example.com/cb")
		form.Set("code_challenge", "x")
		form.Set("code_challenge_method", "S256")
		form.Set("username", "alice")
		form.Set("password", "WRONG")
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, authURL, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", "203.0.113.42")
		r, err := cli.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		_ = r.Body.Close()
		return r.StatusCode
	}

	// First loginFailureMax-1 failures still return 401 (wrong creds).
	for i := 1; i < loginFailureMax; i++ {
		if got := doFail(); got != http.StatusUnauthorized {
			t.Errorf("attempt %d: want 401, got %d", i, got)
		}
	}
	// The Nth failure trips the block; the (N+1)th attempt should be 429.
	_ = doFail()
	if got := doFail(); got != http.StatusTooManyRequests {
		t.Errorf("after %d failures: want 429, got %d", loginFailureMax+1, got)
	}
}

// TestClientIPHonoursTrustedProxyOnly is the regression test for the XFF
// spoof Codex flagged on the first cut: when the request comes from a
// non-loopback peer (direct exposure), X-Forwarded-For must be ignored
// entirely. When it does come from loopback (our Apache proxy), we must
// pick the rightmost XFF entry — the one the proxy appended — never the
// leftmost (which is whatever the client typed).
func TestClientIPHonoursTrustedProxyOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{
			name:       "direct connection, XFF ignored",
			remoteAddr: "203.0.113.99:50000",
			xff:        "1.2.3.4",
			want:       "203.0.113.99",
		},
		{
			name:       "loopback, no XFF, falls back to peer",
			remoteAddr: "127.0.0.1:50000",
			xff:        "",
			want:       "127.0.0.1",
		},
		{
			name:       "loopback, single XFF entry",
			remoteAddr: "127.0.0.1:50000",
			xff:        "198.51.100.7",
			want:       "198.51.100.7",
		},
		{
			name:       "loopback, attacker spoofs leftmost",
			remoteAddr: "127.0.0.1:50000",
			xff:        "1.2.3.4, 198.51.100.7", // Apache appended 198.51.100.7
			want:       "198.51.100.7",
		},
		{
			name:       "loopback, attacker spoofs multiple",
			remoteAddr: "127.0.0.1:50000",
			xff:        "1.2.3.4, 5.6.7.8, 198.51.100.7",
			want:       "198.51.100.7",
		},
		{
			name:       "IPv6 loopback, rightmost wins",
			remoteAddr: "[::1]:50000",
			xff:        "evil, 198.51.100.7",
			want:       "198.51.100.7",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(req); got != tc.want {
				t.Errorf("clientIP = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestScopeHasExactMatch(t *testing.T) {
	for _, tc := range []struct {
		scope, want string
		expect      bool
	}{
		{"openid offline_access mcp", "offline_access", true},
		{"openid mcp", "offline_access", false},
		{"x_offline_access_y", "offline_access", false},
		{"", "offline_access", false},
		{"offline_access", "offline_access", true},
	} {
		if got := scopeHas(tc.scope, tc.want); got != tc.expect {
			t.Errorf("scopeHas(%q, %q) = %v; want %v", tc.scope, tc.want, got, tc.expect)
		}
	}
}
