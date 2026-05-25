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
	BaseURL  string // e.g. https://ssst-login.edookit.net
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
// cookie file exists and is within CookieMaxAge, it is loaded eagerly and
// the client starts in the logged-in state — no chromium launch needed
// until the session expires.
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

	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("cookiejar: %w", err)
		}
		httpClient = &http.Client{Jar: jar, Timeout: 20 * time.Second}
	} else if httpClient.Jar == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("cookiejar: %w", err)
		}
		httpClient.Jar = jar
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
		if err := c.warmupSession(ctx); err == nil {
			c.loggedIn = true
			return nil
		} else {
			log.Printf("[client] cached session invalid (%v); falling back to fresh login", err)
		}
	}

	cookies, err := loginViaBrowser(ctx, browserLoginConfig{
		BaseURL:   c.cfg.BaseURL,
		Username:  c.cfg.Username,
		Password:  c.cfg.Password,
		Headless:  c.cfg.HeadlessLogin,
		Timeout:   c.cfg.LoginTimeout,
		UserAgent: defaultUserAgent,
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

// invalidateSession marks the session as logged out so the next call re-authenticates.
func (c *Client) invalidateSession() {
	c.mu.Lock()
	c.loggedIn = false
	c.mu.Unlock()
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

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
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
