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
}

// Client is a session-aware HTTP client. It performs OIDC login in a real
// browser on demand, captures the Edookit session cookie, and reuses it for
// subsequent net/http requests.
type Client struct {
	cfg     Config
	http    *http.Client
	baseURL *url.URL

	mu       sync.Mutex
	loggedIn bool
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
	if cfg.BaseURL == "" {
		return nil, errors.New("BaseURL is required")
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("username and password are required")
	}
	if cfg.CookieMaxAge == 0 {
		cfg.CookieMaxAge = 10 * time.Hour
	}
	if cfg.Timezone == nil {
		// "Europe/Prague" is the school's TZ for the schools this MCP targets.
		// time/tzdata is imported in main.go so this works on Windows / locked
		// down containers that lack /usr/share/zoneinfo.
		loc, err := time.LoadLocation("Europe/Prague")
		if err != nil {
			// Should never happen with time/tzdata embedded, but fall back to
			// the host's local TZ rather than erroring out on construction.
			log.Printf("[client] LoadLocation(Europe/Prague) failed (%v); falling back to time.Local", err)
			loc = time.Local
		}
		cfg.Timezone = loc
	}

	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}

	httpClient, err := buildHTTPClient(cfg.HTTPClient)
	if err != nil {
		return nil, err
	}

	c := &Client{cfg: cfg, http: httpClient, baseURL: u}

	if cfg.CookieCachePath != "" {
		cookies, age, err := loadCookies(cfg.CookieCachePath, cfg.BaseURL)
		switch {
		case err == nil && age < cfg.CookieMaxAge:
			// Populate the jar but leave loggedIn=false so the first
			// ensureLoggedIn call does a cheap GET / warmup to validate +
			// resurrect the PHP session before any /handler/page/* request.
			c.http.Jar.SetCookies(c.baseURL, cookies)
			log.Printf("[client] loaded %d cached cookies (age %s); will verify on first call",
				len(cookies), age.Round(time.Minute))
		case err == nil:
			log.Printf("[client] cached cookies are stale (age %s > max %s); will re-login on first request",
				age.Round(time.Minute), cfg.CookieMaxAge)
		case !errors.Is(err, os.ErrNotExist):
			log.Printf("[client] cookie cache load failed: %v", err)
		}
	}

	return c, nil
}

func (c *Client) resolve(path string) string {
	ref, _ := url.Parse(path)
	return c.baseURL.ResolveReference(ref).String()
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.resolve(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	return req, nil
}

// buildHTTPClient returns the *http.Client New should use. If the caller
// provided one, we honor it and only fill in a fresh cookie jar when none
// was supplied. If they didn't, we build the whole thing with a 20s timeout
// and our own jar. Either way the returned client has a non-nil Jar.
func buildHTTPClient(provided *http.Client) (*http.Client, error) {
	if provided != nil {
		if provided.Jar == nil {
			jar, err := cookiejar.New(nil)
			if err != nil {
				return nil, fmt.Errorf("cookiejar: %w", err)
			}
			provided.Jar = jar
		}
		return provided, nil
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookiejar: %w", err)
	}
	return &http.Client{Jar: jar, Timeout: 20 * time.Second}, nil
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

	cookies, err := loginViaBrowser(ctx, browserLoginConfig{
		BaseURL:  c.cfg.BaseURL,
		Username: c.cfg.Username,
		Password: c.cfg.Password,
		Headless: c.cfg.HeadlessLogin,
		Timeout:  c.cfg.LoginTimeout,
		// Deliberately no UserAgent — chromium uses its native Chrome UA, so
		// Plus4U / reCAPTCHA bot heuristics see a real-looking browser.
		// defaultUserAgent stays on the net/http client for non-browser requests.
	})
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
// The jar itself is NOT swapped — concurrent tool calls in c.http.Do may be
// reading from it via Jar.Cookies(), and reassigning c.http.Jar would race
// with those reads. Instead we expire each cookie in place via SetCookies
// with MaxAge=-1; cookiejar.Jar is documented as safe for concurrent access,
// and a Set during a concurrent Get is properly serialized inside the jar.
func (c *Client) invalidateSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loggedIn = false
	c.clearJarCookies()
}

// clearJarCookies expires every cookie the jar currently holds for baseURL.
// Names are read from the jar (Jar.Cookies strips Domain/Path, but the
// removal entry's Domain/Path are inferred from the URL we pass, which is
// the same URL used to store them — so the removal IDs match).
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
