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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const defaultUserAgent = "edookit-mcp/0.1 (+https://github.com/dsaiko/edookit-mcp)"

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

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

	// MaxAttempts is the total number of HTTP request attempts before
	// giving up, including the initial try. Default 3 (1 initial + 2
	// retries); set to 1 to disable retries entirely. Only transient
	// failures are retried — network errors (other than context
	// cancellation) and HTTP 408/502/503/504. HTTP 500/501/505+ are
	// treated as deterministic and propagated immediately so we don't mask
	// genuine server bugs behind silent retries.
	MaxAttempts int

	// RetryBaseDelay is the base for exponential backoff between retries.
	// Default 500ms; with the default MaxAttempts=3 the schedule is
	// 500ms → 1s (~1.5s total worst case before failure).
	RetryBaseDelay time.Duration

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
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.RetryBaseDelay == 0 {
		cfg.RetryBaseDelay = 500 * time.Millisecond
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
	if u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS {
		return nil, fmt.Errorf("BaseURL %q must use http or https scheme (e.g. https://your-school-login.edookit.net)", raw)
	}
	// u.Host != "" is not enough: an authority like ":443" leaves Host set
	// to ":443" but Hostname() empty, which would fail much later when the
	// HTTP transport tries to dial. Reject it here.
	if u.Hostname() == "" {
		return nil, fmt.Errorf("BaseURL %q has no host", raw)
	}
	// Strip the default port for the scheme so off-host comparisons
	// downstream don't trip on a "https://school.test:443" baseURL vs a
	// "https://school.test" redirect (both denote the same origin). Custom
	// ports (:8443 etc.) are preserved verbatim.
	if (u.Scheme == schemeHTTP && u.Port() == "80") || (u.Scheme == schemeHTTPS && u.Port() == "443") {
		u.Host = u.Hostname()
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

// newRequest builds an authenticated GET against path (relative or
// absolute). Every Edookit endpoint this client touches is a GET with no
// body, so neither method nor body is parameterized; if a future flow
// needs POST/PUT it should grow a sibling helper that explicitly accepts
// them (and audits retry/cookie semantics for non-idempotent verbs).
func (c *Client) newRequest(ctx context.Context, path string) (*http.Request, error) {
	abs, err := c.resolve(path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, abs, http.NoBody)
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
	// Use the normalized baseURL string rather than the raw cfg value.
	// loginViaBrowser's waitForHost step compares against the parsed
	// host verbatim — if EDOOKIT_URL had an explicit ":443"/":80" the
	// raw form would never match Chrome's canonical no-port redirects
	// and login would time out. parseBaseURL already stripped the
	// default port at construction.
	return loginViaBrowser(ctx, browserLoginConfig{
		BaseURL:  c.baseURL.String(),
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

// do dispatches req through c.http with two pieces of safety net layered
// on top of plain http.Do:
//
//  1. Cookie-jar fence: net/http calls Jar.Cookies before sending and
//     Jar.SetCookies after the response. If reset() (from a concurrent
//     invalidateSession) lands between those two calls, the response's
//     Set-Cookie headers would pollute the freshly-installed jar. We
//     snapshot the current inner at each attempt and bind it to a
//     request-scoped client copy so both jar calls target the same
//     generation; the snapshot becomes garbage when the request finishes.
//     Falls through to plain c.http.Do when we don't own the jar
//     (caller-supplied client with its own jar) — that's the caller's
//     problem to fence as they see fit.
//
//  2. Transient-failure retry: net errors (excluding context cancellation
//     / deadline) and HTTP 408/502/503/504 trigger a retry with
//     exponential backoff up to MaxAttempts total tries. Other 5xx and
//     all 4xx propagate immediately so we don't mask genuine server bugs
//     or auth failures behind silent retries. Safe because every caller
//     issues GET requests; the moment we add a non-idempotent verb this
//     needs gating on req.Method.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	attempts := c.cfg.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for i := range attempts {
		if i > 0 {
			delay := c.cfg.RetryBaseDelay << (i - 1)
			log.Printf("[client] retrying %s %s after %s (attempt %d/%d, last: %v)", //nolint:gosec // G706: req.URL.Path is built from constants in internal/tools + url.Values, not user-supplied input
				req.Method, req.URL.Path, delay, i+1, attempts, lastErr)
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(delay):
			}
		}

		// Re-snapshot the jar on each attempt so a concurrent
		// invalidateSession between retries lands us on the fresh inner.
		client := c.http
		if c.jar != nil {
			snap := *c.http
			snap.Jar = c.jar.inner.Load()
			client = &snap
		}

		resp, err := client.Do(req)
		if err != nil {
			// Context cancellation / deadline are caller intent — don't
			// burn the remaining attempts on them.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			lastErr = err
			continue
		}
		if isTransientStatus(resp.StatusCode) {
			// Drain + close so the underlying connection can be reused for
			// the retry instead of being torn down and re-dialed.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("after %d attempt(s): %w", attempts, lastErr)
}

// sameOrigin reports whether two URLs share the same origin in the
// browser sense: same scheme, same hostname, same effective port (with
// http:80 / https:443 stripped to canonical form). Used to detect when
// a request was bounced to a foreign origin (Plus4U after session
// expiry, a different port on the same host, a different scheme, etc.) —
// any of which should trigger re-login rather than be treated as a
// successful authenticated response.
//
// Plain Hostname() comparison is too loose (treats school.test:8443 and
// school.test:443 as the same site); raw Host comparison is too strict
// (treats school.test:443 and school.test as different even though they
// denote the same origin).
func sameOrigin(a, b *url.URL) bool {
	if a.Scheme != b.Scheme {
		return false
	}
	if a.Hostname() != b.Hostname() {
		return false
	}
	return effectivePort(a) == effectivePort(b)
}

// effectivePort returns the explicit port if set, otherwise the default
// for the URL's scheme. Empty string for schemes other than http/https
// (we never construct those in this client, but be defensive).
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case schemeHTTP:
		return "80"
	case schemeHTTPS:
		return "443"
	}
	return ""
}

// isTransientStatus reports whether an HTTP status code is one we expect
// to succeed on retry — load-balancer / upstream timeouts and overload
// signals. Deliberately excludes the rest of the 5xx range: HTTP 500 from
// Edookit is much more likely a deterministic application bug than a
// transient condition, and silently retrying would mask it.
func isTransientStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout:     // 504
		return true
	}
	return false
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
	req, err := c.newRequest(ctx, "/")
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("warmup GET /: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if !sameOrigin(resp.Request.URL, c.baseURL) {
		return fmt.Errorf("warmup bounced off-origin to %s (session expired)", resp.Request.URL.Host)
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

// GetTo streams the response body of GET <path> into dst. Returns the number
// of bytes written and any error. Used for binary downloads — attachments,
// images — where we don't want to buffer the whole file in memory. Path may
// be relative (resolved against baseURL) or absolute (e.g. a fully-qualified
// attachment URL from a message JSON); both work because newRequest resolves
// via url.ResolveReference. Re-authenticates once on session expiry, same
// as GetJSON / GetDoc.
func (c *Client) GetTo(ctx context.Context, path string, dst io.Writer) (int64, error) {
	return c.getTo(ctx, path, dst, true)
}

func (c *Client) getTo(ctx context.Context, path string, dst io.Writer, retry bool) (int64, error) {
	if err := c.ensureLoggedIn(ctx); err != nil {
		return 0, err
	}

	req, err := c.newRequest(ctx, path)
	if err != nil {
		return 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Session expired: bounced off-origin. Same handling as getJSON / getDoc.
	if !sameOrigin(resp.Request.URL, c.baseURL) {
		if !retry {
			return 0, errors.New("session expired and re-login failed")
		}
		c.invalidateSession()
		return c.getTo(ctx, path, dst, false)
	}

	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}

	// Session-expiry without off-origin bounce: Edookit can answer a binary
	// download URL with HTTP 200 + a text/html login page when cookies are
	// stale (instead of redirecting away from the host). Without this check
	// we would happily stream the login HTML into the destination file. No
	// legitimate Edookit download (PDF / DOCX / image / etc.) reports
	// Content-Type: text/html, so treating an HTML response on this code
	// path as session expiry is safe.
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(strings.ToLower(ct), "text/html") {
		if !retry {
			return 0, fmt.Errorf("GET %s: server returned text/html (likely login page) — re-login failed", path)
		}
		c.invalidateSession()
		return c.getTo(ctx, path, dst, false)
	}

	return io.Copy(dst, resp.Body)
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

	req, err := c.newRequest(ctx, path)
	if err != nil {
		return err
	}
	// Mark as XHR so the server returns JSON rather than the SPA loader page.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Session expired: bounced off-origin (typically Plus4U identity).
	// sameOrigin treats default-port differences as equivalent but
	// distinguishes same-host-different-port redirects, which would
	// indicate a misrouted response and shouldn't be parsed as ours.
	if !sameOrigin(resp.Request.URL, c.baseURL) {
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

	req, err := c.newRequest(ctx, path)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Session expired: bounced off-origin. See sameOrigin's comment for
	// the scheme + hostname + effective-port comparison rationale and
	// matching check in getJSON / warmupSession.
	if !sameOrigin(resp.Request.URL, c.baseURL) {
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
