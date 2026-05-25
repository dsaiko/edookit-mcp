package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const defaultUserAgent = "edookit-mcp/0.1 (+https://github.com/dsaiko/edookit-mcp)"

// Config controls how Client authenticates against Edookit.
//
// Edookit federates login through Plus4U OIDC (uuidentity.plus4u.net), which is
// rendered by a JS SPA and protected by reCAPTCHA. There is no static form to
// POST to, so authentication is performed in a real chromium instance via
// chromedp; the resulting session cookie is then handed off to net/http for
// all subsequent reads/writes.
type Config struct {
	BaseURL  string // your school's Edookit URL, e.g. https://your-school-login.edookit.net
	Username string // Plus4U identity (email or login name)
	Password string

	// HeadlessLogin controls whether the chromium instance used during login
	// is invisible. Default is true; set to false to watch the flow during
	// debugging (requires a desktop session).
	HeadlessLogin bool

	// LoginTimeout caps the entire login flow. Default 90s.
	LoginTimeout time.Duration

	// CookieCachePath is where session cookies are persisted between runs so
	// chromium doesn't have to launch on every startup. Empty disables
	// persistence. Default (when New is called via DefaultCookieCachePath):
	// <UserCacheDir>/edookit-mcp/cookies.json.
	CookieCachePath string

	// CookieMaxAge bounds how long cached cookies are trusted before a fresh
	// login is forced, regardless of the cookies' own Expires attribute.
	// Default 10h, sized for Edookit's ~12h session window.
	CookieMaxAge time.Duration

	// Timezone is the school's wall-clock timezone. Edookit row dates are
	// rendered in tenant-local time ("21.05.2026 12:31") with no offset
	// suffix, so we need an explicit Location to interpret them correctly —
	// otherwise the MCP would emit wrong RFC3339 offsets when running on a
	// host outside the school's timezone (e.g. a cloud VM in UTC).
	// Default: Europe/Prague.
	Timezone *time.Location

	HTTPClient *http.Client

	// LoginFunc, if non-nil, replaces the default chromedp-driven OIDC login
	// with a caller-supplied function. Production callers leave this nil —
	// it exists so tests can exercise ensureLoggedIn's retry/invalidation
	// paths without bringing up a real browser. Returned cookies are
	// installed into the same jar the rest of the client uses.
	LoginFunc func(ctx context.Context) ([]*http.Cookie, error)
}

// Client is a session-aware HTTP client. It performs OIDC login in a real
// browser on demand, captures the Edookit session cookie, and reuses it for
// subsequent net/http requests.
type Client struct {
	cfg     Config
	http    *http.Client
	baseURL *url.URL

	// jar is the swappable wrapper held by c.http.Jar when we built the
	// client ourselves (or filled in the jar on a caller-provided client).
	// nil when the caller supplied an http.Client with their own jar — in
	// that case invalidateSession falls back to best-effort cookie deletion
	// markers, which only cover Path="/" cookies.
	jar *swappableJar

	mu       sync.Mutex
	loggedIn bool
}

// swappableJar wraps an inner cookiejar.Jar behind an atomic.Pointer so the
// whole jar can be replaced atomically by invalidateSession — concurrent
// Cookies / SetCookies calls from c.http.Do in other goroutines see either
// the old jar or the new one, never a torn state.
//
// Why a full swap rather than expiring individual cookies: cookiejar.Jar
// only exposes cookies through Cookies(url), which is filtered by Path. Any
// cookie scoped to a path other than "/" (e.g. Edookit could install a
// /handler-scoped auth cookie) cannot be enumerated through that API, so
// path-scoped credentials would survive a name-based clear and keep being
// sent on subsequent requests after invalidateSession.
type swappableJar struct {
	inner atomic.Pointer[cookiejar.Jar]
}

func newSwappableJar() (*swappableJar, error) {
	j, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	s := &swappableJar{}
	s.inner.Store(j)
	return s, nil
}

func (s *swappableJar) Cookies(u *url.URL) []*http.Cookie {
	return s.inner.Load().Cookies(u)
}

func (s *swappableJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	s.inner.Load().SetCookies(u, cookies)
}

// reset replaces the inner jar with a fresh empty one. Any in-flight call
// holding a reference to the previous inner via Load() continues to work
// against the old jar (which becomes garbage once those calls return).
func (s *swappableJar) reset() error {
	j, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	s.inner.Store(j)
	return nil
}

// New constructs a Client from the given config. It returns an error if
// required fields (BaseURL, Username, Password) are missing. If a cached
// cookie file exists and is within CookieMaxAge, the cookies are preloaded
// into the jar but the client deliberately starts as `loggedIn=false`: the
// first call to ensureLoggedIn performs a cheap GET / warmup that verifies
// the cached session is still valid and resurrects the PHP session before
// any /handler/page/* hit. Only when warmup fails does chromium relaunch
// for a full OIDC login.
func New(cfg Config) (*Client, error) {
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("username and password are required")
	}
	if cfg.CookieMaxAge == 0 {
		cfg.CookieMaxAge = 10 * time.Hour
	}
	if cfg.Timezone == nil {
		cfg.Timezone = defaultTimezone()
	}

	u, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}

	httpClient, jar, err := buildHTTPClient(cfg.HTTPClient)
	if err != nil {
		return nil, err
	}

	c := &Client{cfg: cfg, http: httpClient, baseURL: u, jar: jar}

	if cfg.CookieCachePath != "" {
		c.preloadCookies()
	}

	return c, nil
}

// parseBaseURL validates the configured BaseURL. url.Parse accepts schemeless
// inputs like "school.edookit.net" by stashing them in Path; the failure would
// surface much later in an HTTP/chromedp call with a less useful message.
// Reject those (and missing/invalid hosts) here while we still have context.
func parseBaseURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("BaseURL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("BaseURL %q must use http or https scheme (e.g. https://your-school-login.edookit.net)", raw)
	}
	// u.Host != "" is not enough: an authority like ":443" leaves Host set
	// to ":443" but Hostname() empty, which would fail much later when the
	// HTTP transport tries to dial. Reject it here.
	if u.Hostname() == "" {
		return nil, fmt.Errorf("BaseURL %q has no host", raw)
	}
	return u, nil
}

// defaultTimezone returns Europe/Prague (the TZ for the schools this MCP
// targets). time/tzdata is imported in main.go so this works on Windows /
// locked-down containers that lack /usr/share/zoneinfo. Falls back to
// time.Local on the (effectively impossible) embedded-tzdata failure rather
// than erroring out on construction.
func defaultTimezone() *time.Location {
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		log.Printf("[client] LoadLocation(Europe/Prague) failed (%v); falling back to time.Local", err)
		return time.Local
	}
	return loc
}

// preloadCookies loads cached cookies into the jar if present and fresh.
// Leaves loggedIn=false either way so the first ensureLoggedIn call does a
// cheap GET / warmup that validates and resurrects the PHP session before
// any /handler/page/* request.
func (c *Client) preloadCookies() {
	cookies, age, err := loadCookies(c.cfg.CookieCachePath, c.cfg.BaseURL)
	switch {
	case err == nil && age < c.cfg.CookieMaxAge:
		c.http.Jar.SetCookies(c.baseURL, cookies)
		log.Printf("[client] loaded %d cached cookies (age %s); will verify on first call",
			len(cookies), age.Round(time.Minute))
	case err == nil:
		log.Printf("[client] cached cookies are stale (age %s > max %s); will re-login on first request",
			age.Round(time.Minute), c.cfg.CookieMaxAge)
	case !errors.Is(err, os.ErrNotExist):
		log.Printf("[client] cookie cache load failed: %v", err)
	}
}

// resolve turns a relative or absolute path into a full URL by resolving it
// against baseURL. A url.Parse failure on the input (e.g. caller passed a
// malformed query-string-as-path) is propagated rather than swallowed — a
// nil ref into ResolveReference panics, and silently constructing baseURL
// would dispatch the wrong endpoint.
func (c *Client) resolve(path string) (string, error) {
	ref, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("parse request path %q: %w", path, err)
	}
	return c.baseURL.ResolveReference(ref).String(), nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	abs, err := c.resolve(path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, abs, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	return req, nil
}

// login runs whichever login mechanism the Config chose. By default it
// drives chromedp through the OIDC flow; tests can override with
// Config.LoginFunc to skip the real browser.
func (c *Client) login(ctx context.Context) ([]*http.Cookie, error) {
	if c.cfg.LoginFunc != nil {
		return c.cfg.LoginFunc(ctx)
	}
	return loginViaBrowser(ctx, browserLoginConfig{
		BaseURL:  c.cfg.BaseURL,
		Username: c.cfg.Username,
		Password: c.cfg.Password,
		Headless: c.cfg.HeadlessLogin,
		Timeout:  c.cfg.LoginTimeout,
		// Deliberately no UserAgent — chromium uses its native Chrome UA, so
		// Plus4U / reCAPTCHA bot heuristics see a real-looking browser.
		// defaultUserAgent stays on the net/http client for non-browser requests.
	})
}

// buildHTTPClient returns the *http.Client New should use along with a
// swappableJar handle when we control the jar (so invalidateSession can do
// an atomic full-jar reset). A caller-provided client with its own Jar is
// honored as-is and the returned swappableJar is nil — in that case
// invalidateSession falls back to per-cookie deletion markers, which only
// cover Path="/" cookies.
func buildHTTPClient(provided *http.Client) (*http.Client, *swappableJar, error) {
	if provided != nil && provided.Jar != nil {
		return provided, nil, nil
	}
	jar, err := newSwappableJar()
	if err != nil {
		return nil, nil, fmt.Errorf("cookiejar: %w", err)
	}
	if provided != nil {
		provided.Jar = jar
		return provided, jar, nil
	}
	return &http.Client{Jar: jar, Timeout: 20 * time.Second}, jar, nil
}

// EnsureLoggedIn forces a login if we don't already have a session. Normally
// callers don't need this — GetDoc/GetJSON authenticate lazily. Exposed for
// smoke tests and eager-login flows.
func (c *Client) EnsureLoggedIn(ctx context.Context) error {
	return c.ensureLoggedIn(ctx)
}

// SessionCookies returns the cookies currently held for the target host.
// Intended for diagnostics; do not log these in production.
func (c *Client) SessionCookies() []*http.Cookie {
	return c.http.Jar.Cookies(c.baseURL)
}

// Timezone returns the Location callers should use when interpreting
// Edookit's wall-clock timestamps (row dates have no offset suffix).
// Configured via Config.Timezone — default Europe/Prague.
func (c *Client) Timezone() *time.Location {
	return c.cfg.Timezone
}

// warmupSession performs a GET / which Edookit needs to "resurrect" a PHP
// session from the persistent X-EdooAuthToken / X-Auth-Id cookies. Subsequent
// /handler/page/* calls return HTTP 200 with authenticated:false until this
// has happened. Also detects a dead session (bounce to Plus4U) so callers can
// trigger a fresh chromedp login.
func (c *Client) warmupSession(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("warmup GET /: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.Request.URL.Host != c.baseURL.Host {
		return fmt.Errorf("warmup bounced off-host to %s (session expired)", resp.Request.URL.Host)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("warmup got HTTP %d", resp.StatusCode)
	}
	return nil
}

// ensureLoggedIn brings the client into the authenticated state. If cookies
// are already in the jar (loaded from cache), a single warmup GET / verifies
// them — that's typically ~100ms. If the jar is empty or warmup fails, a full
// chromedp login is performed (~4s) and then warmed up.
func (c *Client) ensureLoggedIn(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loggedIn {
		return nil
	}

	// Fast path: cookies already loaded (from cache). Warm them up; if the
	// session is still alive, we're done without launching chromium.
	if len(c.http.Jar.Cookies(c.baseURL)) > 0 {
		warmupErr := c.warmupSession(ctx)
		if warmupErr == nil {
			c.loggedIn = true
			return nil
		}
		log.Printf("[client] cached session invalid (%v); falling back to fresh login", warmupErr)
	}

	cookies, err := c.login(ctx)
	if err != nil {
		return fmt.Errorf("oidc login: %w", err)
	}

	c.http.Jar.SetCookies(c.baseURL, cookies)

	// Warm up so /handler/page/* calls find a valid PHP session.
	if err := c.warmupSession(ctx); err != nil {
		return fmt.Errorf("warmup after login failed: %w", err)
	}

	c.loggedIn = true

	if c.cfg.CookieCachePath != "" {
		// Persist the post-login cookies (PHPSESSID will rotate again on each
		// subsequent request, but X-EdooAuthToken / X-Auth-Id stay constant
		// and are what actually authenticate the next session).
		if err := saveCookies(c.cfg.CookieCachePath, c.cfg.BaseURL, c.http.Jar.Cookies(c.baseURL)); err != nil {
			log.Printf("[client] failed to cache cookies (non-fatal): %v", err)
		} else {
			log.Printf("[client] cached cookies to %s", c.cfg.CookieCachePath)
		}
	}
	return nil
}

// invalidateSession marks the session as logged out AND drops the cached
// auth cookies so the next ensureLoggedIn skips the warmup-only fast path
// and runs a full chromedp login. Just flipping loggedIn=false isn't enough
// — warmup can still succeed (server hands out a new PHPSESSID) with auth
// tokens the backend has already invalidated, which would yield an infinite
// no-op retry loop on authenticated=false responses.
//
// When we control the jar (the common case) we swap the inner cookiejar via
// swappableJar.reset(); concurrent c.http.Do calls reading the old inner
// continue safely against the now-orphaned jar. When a caller supplied
// their own jar we fall back to per-cookie deletion markers via
// clearJarCookies — best effort, and known to miss cookies scoped to a path
// other than "/".
func (c *Client) invalidateSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loggedIn = false
	if c.jar != nil {
		if err := c.jar.reset(); err != nil {
			log.Printf("[client] swappable jar reset failed (%v); falling back to in-place clear", err)
			c.clearJarCookies()
		}
		return
	}
	c.clearJarCookies()
}

// clearJarCookies is the fallback invalidation path when we don't own the
// jar. It expires every cookie the jar reports for baseURL by writing
// deletion markers. Limitations: Jar.Cookies(c.baseURL) only returns
// cookies whose Path matches "/", so any cookies scoped to a deeper path
// won't be enumerated and won't be cleared. The hardcoded Path="/" on the
// markers reflects that same limitation — there's no Set-Cookie response
// header for us to learn other paths from.
func (c *Client) clearJarCookies() {
	existing := c.http.Jar.Cookies(c.baseURL)
	if len(existing) == 0 {
		return
	}
	toRemove := make([]*http.Cookie, len(existing))
	for i, ck := range existing {
		toRemove[i] = &http.Cookie{Name: ck.Name, Path: "/", MaxAge: -1} //nolint:gosec // G124: deletion markers; cookiejar drops entries with MaxAge<0 regardless of secure-flag attributes
	}
	c.http.Jar.SetCookies(c.baseURL, toRemove)
}

// GetDoc fetches a path as a parsed HTML document, re-authenticating once if
// the session expired (detected by the site bouncing us to the login host).
//
// Use GetJSON instead when calling Edookit's internal JSON APIs (the SPA's
// XHR endpoints); GetDoc is reserved for the rare server-rendered HTML page.
func (c *Client) GetDoc(ctx context.Context, path string) (*goquery.Document, error) {
	return c.getDoc(ctx, path, true)
}

// GetJSON fetches a path and decodes the JSON response into out. out must be a
// pointer to the value being populated (the same contract as json.Unmarshal).
// Re-authenticates once on session expiry, same as GetDoc.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	return c.getJSON(ctx, path, out, true)
}

// authEnvelope is the subset of every /handler/page/* and /handler/grid/*
// response we read to detect a server-side session expiry that did NOT cause
// an off-host bounce — when this happens the server returns HTTP 200 with a
// default page shape and authenticated:false, and callers would otherwise get
// silently empty results.
type authEnvelope struct {
	Authenticated *bool `json:"authenticated"`
}

func (c *Client) getJSON(ctx context.Context, path string, out any, retry bool) error {
	if err := c.ensureLoggedIn(ctx); err != nil {
		return err
	}

	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	// Mark as XHR so the server returns JSON rather than the SPA loader page.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Session expired: bounced off-host (to Plus4U identity).
	if resp.Request.URL.Host != c.baseURL.Host {
		if !retry {
			return errors.New("session expired and re-login failed")
		}
		c.invalidateSession()
		return c.getJSON(ctx, path, out, false)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body from %s: %w", path, err)
	}

	// Server may return HTTP 200 with authenticated:false instead of bouncing.
	// Detect that and treat it the same as session expiry.
	var env authEnvelope
	if jerr := json.Unmarshal(body, &env); jerr == nil && env.Authenticated != nil && !*env.Authenticated {
		if !retry {
			return errors.New("session reported authenticated=false and re-login failed")
		}
		c.invalidateSession()
		return c.getJSON(ctx, path, out, false)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode JSON from %s: %w", path, err)
	}
	return nil
}

func (c *Client) getDoc(ctx context.Context, path string, retry bool) (*goquery.Document, error) {
	if err := c.ensureLoggedIn(ctx); err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Session expired: Edookit bounced us off-host (to Plus4U identity). When
	// that happens, the response's final URL host differs from our base host.
	if resp.Request.URL.Host != c.baseURL.Host {
		if !retry {
			return nil, errors.New("session expired and re-login failed")
		}
		c.invalidateSession()
		return c.getDoc(ctx, path, false)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}

	return goquery.NewDocumentFromReader(resp.Body)
}
