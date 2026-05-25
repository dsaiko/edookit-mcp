package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// loginStep pairs a chromedp action with a human-readable label so we can
// report which step failed when the OIDC flow doesn't complete.
type loginStep struct {
	name   string
	action chromedp.Action
}

// makeLoginEventListener builds the chromedp.ListenTarget callback used during
// browser login. Surfaces browser-side warnings/errors and intercepts OIDC
// auth requests to strip prompt=none for our client_id.
func makeLoginEventListener(ctx context.Context) func(any) {
	return func(ev any) {
		switch ev := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			if ev.Type != "warning" && ev.Type != "error" {
				return
			}
			parts := make([]string, 0, len(ev.Args))
			for _, a := range ev.Args {
				parts = append(parts, string(a.Value))
			}
			log.Printf("[browser-console.%s] %s", ev.Type, strings.Join(parts, " "))
		case *runtime.EventExceptionThrown:
			log.Printf("[browser-exception] %s", ev.ExceptionDetails.Text)
		case *fetch.EventRequestPaused:
			handleFetchIntercept(ctx, ev)
		}
	}
}

// handleFetchIntercept rewrites the outer OIDC auth request to remove
// prompt=none, which Plus4U's client lib hardcodes for silent SSO. Nested
// IdM SPA auth requests (different client_id) pass through untouched.
func handleFetchIntercept(ctx context.Context, ev *fetch.EventRequestPaused) {
	origURL := ev.Request.URL
	newURL := stripPromptNoneForClient(origURL, plus4uClientID)
	c := chromedp.FromContext(ctx)
	execCtx := cdp.WithExecutor(ctx, c.Target)

	req := fetch.ContinueRequest(ev.RequestID)
	if newURL != origURL {
		log.Printf("[fetch-intercept] stripped prompt=none from outer auth (client=%s)", plus4uClientID)
		req = req.WithURL(newURL)
	}
	go func() {
		if err := req.Do(execCtx); err != nil {
			log.Printf("[fetch-intercept] continue failed: %v", err)
		}
	}()
}

// captureLocation reads the current page URL for inclusion in error messages.
// Returns "?" if the read fails (the original error already gives the cause).
func captureLocation(ctx context.Context) string {
	var loc string
	if err := chromedp.Run(ctx, chromedp.Location(&loc)); err != nil {
		return "?"
	}
	return loc
}

// browserLoginConfig controls how the headless browser performs OIDC login.
type browserLoginConfig struct {
	BaseURL  string        // your school's Edookit URL, e.g. https://your-school-login.edookit.net
	Username string        // Plus4U identity (email or login name)
	Password string        // Plus4U password
	Headless bool          // false = visible browser (debugging); true = production
	Timeout  time.Duration // overall login timeout (default 90s)
}

// loginViaBrowser drives the full OIDC code flow in a real chromium instance
// and returns the cookies that target site set in the browser's cookie store.
// Only cookies matching the BaseURL's host are returned — Plus4U session cookies
// (uoid.*) are left behind, since net/http only needs the Edookit PHP session.
func loginViaBrowser(ctx context.Context, cfg browserLoginConfig) ([]*http.Cookie, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 90 * time.Second
	}

	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}

	// Keep chromium's default sandbox enabled — disabling it would weaken
	// browser isolation on end-user machines. Container/CI environments that
	// need --no-sandbox can drop it via a wrapper script if/when necessary.
	opts := append([]chromedp.ExecAllocatorOption{},
		chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.Flag("headless", cfg.Headless),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-gpu", true),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	timeoutCtx, cancelTimeout := context.WithTimeout(browserCtx, cfg.Timeout)
	defer cancelTimeout()

	// Listen for events: surface browser-side errors (silent JS catches in
	// Plus4U's gateway lib otherwise swallow these), and intercept the OIDC
	// auth request to strip Plus4U's hardcoded prompt=none.
	chromedp.ListenTarget(timeoutCtx, makeLoginEventListener(timeoutCtx))

	// Enable network event reporting + fetch interception for OIDC auth URLs.
	if err := chromedp.Run(timeoutCtx,
		network.Enable(),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{
			{URLPattern: "https://uuidentity.plus4u.net/*/oidc/auth*"},
		}),
	); err != nil {
		return nil, fmt.Errorf("enable network/fetch domains: %w", err)
	}

	// Selector for the "Přihlásit přes Plus4U" button on Edookit's landing
	// page. It is a <div class="plus4ULoginButton"> whose onclick triggers
	// Plus4U's OIDC client library to redirect to uuidentity.plus4u.net.
	const plus4uButton = `.plus4ULoginButton`

	steps := []loginStep{
		{
			name:   "navigate to landing",
			action: chromedp.Navigate(cfg.BaseURL),
		},
		{
			name:   "wait for Plus4U button",
			action: chromedp.WaitVisible(plus4uButton, chromedp.ByQuery),
		},
		{
			name: "wait for OIDC client library to load",
			action: chromedp.ActionFunc(func(ctx context.Context) error {
				return waitForJSCondition(ctx,
					`typeof idmLoginClick === 'function' && typeof libraryImportPromise !== 'undefined'`,
					30*time.Second)
			}),
		},
		{
			// Trigger Plus4U OIDC flow. The lib emits its auth request with
			// prompt=none for silent SSO — we strip that in the fetch
			// interceptor above so the provider shows the login form.
			name:   "trigger Plus4U login",
			action: chromedp.Evaluate(`idmLoginClick()`, nil),
		},
		{
			name: "wait for redirect to Plus4U identity",
			action: chromedp.ActionFunc(func(ctx context.Context) error {
				return waitForHost(ctx, "uuidentity.plus4u.net", 30*time.Second)
			}),
		},
		{
			name:   "wait for username input on Plus4U page",
			action: chromedp.WaitVisible(`input[autocomplete="username"]`, chromedp.ByQuery),
		},
		{
			name: "fill username",
			action: chromedp.SendKeys(`input[autocomplete="username"]`,
				cfg.Username, chromedp.ByQuery),
		},
		{
			name: "fill password",
			action: chromedp.SendKeys(`input[autocomplete="current-password"]`,
				cfg.Password, chromedp.ByQuery),
		},
		{
			name:   "submit credentials",
			action: chromedp.Submit(`input[autocomplete="current-password"]`, chromedp.ByQuery),
		},
		{
			name: "wait for redirect back to Edookit",
			action: chromedp.ActionFunc(func(ctx context.Context) error {
				return waitForHost(ctx, base.Host, 60*time.Second)
			}),
		},
	}

	var rawCookies []*network.Cookie
	for _, step := range steps {
		log.Printf("[login] %s ...", step.name)
		start := time.Now()
		if err := chromedp.Run(timeoutCtx, step.action); err != nil {
			currentURL := captureLocation(timeoutCtx)
			return nil, fmt.Errorf("step %q failed after %s (page: %s): %w",
				step.name, time.Since(start).Round(time.Millisecond), currentURL, err)
		}
		log.Printf("[login] %s done in %s", step.name, time.Since(start).Round(time.Millisecond))
	}

	if err := chromedp.Run(timeoutCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		cookies, err := network.GetCookies().Do(ctx)
		if err != nil {
			return err
		}
		rawCookies = cookies
		return nil
	})); err != nil {
		return nil, fmt.Errorf("read cookies: %w", err)
	}

	out := make([]*http.Cookie, 0, len(rawCookies))
	for _, c := range rawCookies {
		if !hostMatchesCookie(base.Host, c.Domain) {
			continue
		}
		out = append(out, &http.Cookie{ //nolint:gosec // G124: attributes are proxied verbatim from the browser; we can't tighten them without breaking server-issued cookies
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.Secure,
			HttpOnly: c.HTTPOnly,
			SameSite: chromeSameSiteToHTTP(c.SameSite),
		})
	}
	if len(out) == 0 {
		return nil, errors.New("browser login: no cookies captured for target host")
	}
	return out, nil
}

// DumpLandingHTML launches chromium against BaseURL, waits for the page to
// render, and returns the outer HTML of <body>. Intended for selector
// debugging — call from a smoke target, never from production code paths.
func DumpLandingHTML(ctx context.Context, baseURL string, headless bool) (string, error) {
	opts := append([]chromedp.ExecAllocatorOption{},
		chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.Flag("headless", headless),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	timeoutCtx, cancelTimeout := context.WithTimeout(browserCtx, 30*time.Second)
	defer cancelTimeout()

	var html string
	err := chromedp.Run(timeoutCtx,
		chromedp.Navigate(baseURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		// Give SPA a moment to render after `body` first appears.
		chromedp.Sleep(3*time.Second),
		chromedp.OuterHTML("body", &html, chromedp.ByQuery),
	)
	if err != nil {
		return "", fmt.Errorf("dump landing html: %w", err)
	}
	return html, nil
}

// waitForJSCondition polls a JavaScript boolean expression in the current page
// until it evaluates to true or the timeout elapses. Useful for waiting on
// dynamically-loaded SPA state that has no DOM signal we can WaitVisible for.
func waitForJSCondition(ctx context.Context, expr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var ok bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok)); err == nil && ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for JS condition: %s", expr)
}

// waitForHost polls the current page URL until the host matches the target,
// signaling the OIDC redirect chain has completed. Polling beats relying on
// chromedp.WaitVisible against an unknown post-login element selector.
func waitForHost(ctx context.Context, wantHost string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var current string
		if err := chromedp.Run(ctx, chromedp.Location(&current)); err != nil {
			return err
		}
		if u, err := url.Parse(current); err == nil && u.Host == wantHost {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for redirect to %s", wantHost)
}

// hostMatchesCookie reports whether a cookie's Domain attribute applies to host.
// Chrome reports domains with or without a leading dot; both are valid.
func hostMatchesCookie(host, cookieDomain string) bool {
	d := strings.TrimPrefix(cookieDomain, ".")
	return host == d || strings.HasSuffix(host, "."+d)
}

// plus4uClientID is Edookit's OIDC client identifier on the Plus4U provider.
// Discovered in the embedded UU5.Environment block of the landing page —
// see uu_app_oidc_providers_oidcg02_client_id. Stable across sessions.
const plus4uClientID = "721ac68f5be74347aea24ef1ccf9d472"

// stripPromptNoneForClient removes prompt=none from a URL, but only when the
// request's client_id matches the given target. The IdM SPA on Plus4U fires
// its own nested silent renewal with a different client_id; that one must
// keep prompt=none or the IdM SPA's session-restore path breaks.
func stripPromptNoneForClient(raw, clientID string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("prompt") != "none" || q.Get("client_id") != clientID {
		return raw
	}
	q.Del("prompt")
	u.RawQuery = q.Encode()
	return u.String()
}

// chromeSameSiteToHTTP maps chromedp's SameSite enum onto net/http's. An empty
// or unknown value falls back to Lax, which matches modern browser default.
func chromeSameSiteToHTTP(s network.CookieSameSite) http.SameSite {
	switch s {
	case network.CookieSameSiteStrict:
		return http.SameSiteStrictMode
	case network.CookieSameSiteNone:
		return http.SameSiteNoneMode
	case network.CookieSameSiteLax:
		return http.SameSiteLaxMode
	default:
		return http.SameSiteLaxMode
	}
}
