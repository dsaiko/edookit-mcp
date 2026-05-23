package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const defaultUserAgent = "edookit-mcp/0.1 (+https://github.com/dsaiko/edookit-mcp)"

// Config controls how Client authenticates against the target site.
type Config struct {
	BaseURL  string
	Username string
	Password string

	LoginPath  string // e.g. "/login"
	UserField  string // form field name for username, e.g. "username"
	PassField  string // form field name for password, e.g. "password"
	CSRFField  string // hidden input name on the login page, e.g. "_token"; empty if none
	HTTPClient *http.Client
}

// Client is a session-aware HTTP client that performs a form-based login on
// demand and reuses the resulting cookie for subsequent requests.
type Client struct {
	cfg     Config
	http    *http.Client
	baseURL *url.URL

	mu       sync.Mutex
	loggedIn bool
}

// New constructs a Client from the given config. It returns an error if
// required fields (BaseURL, Username, Password) are missing.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("BaseURL is required")
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("username and password are required")
	}
	if cfg.LoginPath == "" {
		cfg.LoginPath = "/login"
	}
	if cfg.UserField == "" {
		cfg.UserField = "username"
	}
	if cfg.PassField == "" {
		cfg.PassField = "password"
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

	return &Client{
		cfg:     cfg,
		http:    httpClient,
		baseURL: u,
	}, nil
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

// ensureLoggedIn performs a form login if we're not already authenticated.
// Subsequent requests reuse the session cookie via the shared cookie jar.
func (c *Client) ensureLoggedIn(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loggedIn {
		return nil
	}

	form := url.Values{}

	if c.cfg.CSRFField != "" {
		token, err := c.fetchCSRFToken(ctx)
		if err != nil {
			return fmt.Errorf("fetch csrf token: %w", err)
		}
		form.Set(c.cfg.CSRFField, token)
	}

	form.Set(c.cfg.UserField, c.cfg.Username)
	form.Set(c.cfg.PassField, c.cfg.Password)

	req, err := c.newRequest(ctx, http.MethodPost, c.cfg.LoginPath, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	// Heuristic: if we still landed on the login path after redirects, credentials were rejected.
	if strings.TrimRight(resp.Request.URL.Path, "/") == strings.TrimRight(c.cfg.LoginPath, "/") {
		return errors.New("login failed: still on login page after POST (check credentials, CSRF field name, or form field names)")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("login failed: HTTP %d", resp.StatusCode)
	}

	c.loggedIn = true
	return nil
}

func (c *Client) fetchCSRFToken(ctx context.Context) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, c.cfg.LoginPath, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}
	val, ok := doc.Find(fmt.Sprintf(`input[name=%q]`, c.cfg.CSRFField)).Attr("value")
	if !ok {
		return "", fmt.Errorf("csrf field %q not found on login page", c.cfg.CSRFField)
	}
	return val, nil
}

// invalidateSession marks the session as logged out so the next call re-authenticates.
func (c *Client) invalidateSession() {
	c.mu.Lock()
	c.loggedIn = false
	c.mu.Unlock()
}

// GetDoc fetches a path as a parsed HTML document, re-authenticating once if the session expired.
func (c *Client) GetDoc(ctx context.Context, path string) (*goquery.Document, error) {
	return c.getDoc(ctx, path, true)
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

	// Session expired: site bounced us back to the login page.
	if strings.TrimRight(resp.Request.URL.Path, "/") == strings.TrimRight(c.cfg.LoginPath, "/") {
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
