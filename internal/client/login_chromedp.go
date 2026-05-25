package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
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
// browser login. Surfaces browser-side warnings/errors, intercepts OIDC auth
// requests to strip prompt=none, and auto-dismisses any alert/confirm dialogs
// the page tries to open (Plus4U's lib surfaces non-fatal init errors via
// alert() which would otherwise block the entire flow in headful mode).
//
// The client_id is captured at runtime from the landing page and shared via
// atomic.Value so this listener — which runs in a chromedp event goroutine —
// can read it without a race against the step that writes it.
func makeLoginEventListener(ctx context.Context, clientIDStore *atomic.Value) func(any) {
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
			clientID, _ := clientIDStore.Load().(string)
			handleFetchIntercept(ctx, ev, clientID)
		case *page.EventJavascriptDialogOpening:
			// Plus4U's init code surfaces non-fatal errors (e.g.
			// "Asynchronous method getMetadata must be invoked prior to
			// synchronous invocation") via alert(). Headless chrome
			// auto-dismisses; headful mode blocks the flow on a modal that
			// no user is going to click. Accept any dialog so the flow
			// behaves identically in both modes. The error itself is
			// already logged by the browser-exception/console handlers.
			log.Printf("[browser-dialog %s] %s (auto-dismissing)", ev.Type, ev.Message)
			c := chromedp.FromContext(ctx)
			execCtx := cdp.WithExecutor(ctx, c.Target)
			go func() {
				if err := page.HandleJavaScriptDialog(true).Do(execCtx); err != nil {
					log.Printf("[browser-dialog] dismiss failed: %v", err)
				}
			}()
		}
	}
}

// handleFetchIntercept rewrites the outer OIDC auth request to remove
// prompt=none, which Plus4U's client lib hardcodes for silent SSO. Nested
// IdM SPA auth requests (different client_id) pass through untouched. If
// clientID is empty (extraction step hasn't run yet, or failed) every
// intercepted request passes through unmodified — same behavior as
// "we don't know which one is ours, so don't touch any of them".
func handleFetchIntercept(ctx context.Context, ev *fetch.EventRequestPaused, clientID string) {
	origURL := ev.Request.URL
	newURL := origURL
	if clientID != "" {
		newURL = stripPromptNoneForClient(origURL, clientID)
	}
	c := chromedp.FromContext(ctx)
	execCtx := cdp.WithExecutor(ctx, c.Target)

	req := fetch.ContinueRequest(ev.RequestID)
	if newURL != origURL {
		log.Printf("[fetch-intercept] stripped prompt=none from outer auth (client=%s)", clientID)
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
	// auth request to strip Plus4U's hardcoded prompt=none. The fetch
	// interceptor only acts on requests whose client_id matches the value we
	// extract from the landing page below — so it stays empty until the
	// "extract OIDC client_id" step runs, which is fine since the auth
	// request only fires AFTER the Plus4U button click, which is well after
	// extraction. Plus4U's nested IdM SPA silent renewal uses a different
	// client_id and is correctly left alone.
	var clientIDStore atomic.Value
	clientIDStore.Store("") // initialize so Load never returns nil
	chromedp.ListenTarget(timeoutCtx, makeLoginEventListener(timeoutCtx, &clientIDStore))

	// Enable network/fetch/page domains. Page is needed so the dialog event
	// handler in the listener receives JavaScript-dialog-opening events.
	if err := chromedp.Run(timeoutCtx,
		network.Enable(),
		page.Enable(),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{
			{URLPattern: "https://uuidentity.plus4u.net/*/oidc/auth*"},
		}),
	); err != nil {
		return nil, fmt.Errorf("enable network/fetch/page domains: %w", err)
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
			// Wait for everything we need before the click:
			//   1. idmLoginClick defined — the onclick handler we're going to invoke
			//   2. libraryImportPromise defined — Plus4U's OIDC lib is at least started
			//   3. UU5.Environment.uu_app_oidc_providers_oidcg02_client_id populated —
			//      the value we need to extract. uu5loader can reassign UU5.Environment
			//      partway through init, so we check the actual field is non-empty
			//      rather than assuming "lib loaded" implies "config populated".
			name: "wait for OIDC client library + environment to populate",
			action: chromedp.ActionFunc(func(ctx context.Context) error {
				return waitForJSCondition(ctx,
					`typeof idmLoginClick === 'function' && typeof libraryImportPromise !== 'undefined' && !!(window.UU5 && window.UU5.Environment && window.UU5.Environment.uu_app_oidc_providers_oidcg02_client_id)`,
					30*time.Second)
			}),
		},
		{
			// Pull the per-tenant OIDC client_id from the landing page's
			// UU5.Environment block. Each Edookit tenant has its own value,
			// so the fetch interceptor below can target THIS school's auth
			// request and leave the IdM SPA's nested silent renewal (a
			// different client_id) alone.
			name: "extract OIDC client_id from landing page",
			action: chromedp.ActionFunc(func(ctx context.Context) error {
				const js = `window.UU5.Environment.uu_app_oidc_providers_oidcg02_client_id`
				var got string
				if err := chromedp.Run(ctx, chromedp.Evaluate(js, &got)); err != nil {
					return fmt.Errorf("evaluate client_id: %w", err)
				}
				if got == "" {
					// Belt-and-braces: the readiness step above gated on a
					// non-empty value, but defend against a race where the
					// field is reset between the gate and our read.
					return errors.New("OIDC client_id empty (UU5.Environment may have been reset between readiness check and read)")
				}
				clientIDStore.Store(got)
				log.Printf("[login] OIDC client_id: %s", got)
				return nil
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
	// Keep chromium's default sandbox enabled, same as loginViaBrowser. This is
	// a debugging-only path but still drives a real browser against a
	// user-provided URL — there's no reason to weaken isolation here.
	opts := append([]chromedp.ExecAllocatorOption{},
		chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.Flag("headless", headless),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-gpu", true),
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

// stripPromptNoneForClient removes prompt=none from a URL, but only when the
// request's client_id matches the given target. The IdM SPA on Plus4U fires
// its own nested silent renewal with a different client_id; that one must
// keep prompt=none or the IdM SPA's session-restore path breaks. The matching
// client_id is captured at runtime from the landing page's UU5.Environment
// block (uu_app_oidc_providers_oidcg02_client_id) — it is per-tenant.
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
