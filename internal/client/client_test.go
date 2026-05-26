package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- helpers ----------

// newClientForTest wires a Client to point at srv.URL, with whatever
// per-test overrides callers want via opts. Default config is sensible for
// most tests: a no-op LoginFunc that returns a single PHPSESSID cookie so
// chromedp never runs, and retries are disabled (MaxAttempts=1) so tests
// that simulate transient 5xx don't get slowed down by ~1.5s of backoff.
// Retry-behavior tests opt in by setting MaxAttempts explicitly.
func newClientForTest(t *testing.T, srv *httptest.Server, configure ...func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		BaseURL:     srv.URL,
		Username:    "u",
		Password:    "p",
		MaxAttempts: 1,
		LoginFunc: func(_ context.Context) ([]*http.Cookie, error) {
			return []*http.Cookie{{Name: "PHPSESSID", Value: "test-session", Path: "/"}}, nil
		},
	}
	for _, fn := range configure {
		fn(&cfg)
	}
	cli, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return cli
}

// preloadJar seeds the client's jar with cookies for baseURL — same as if
// the cookie cache had been loaded successfully.
func preloadJar(t *testing.T, cli *Client, cookies ...*http.Cookie) {
	t.Helper()
	cli.http.Jar.SetCookies(cli.baseURL, cookies)
}

// fakeServer wraps the caller's mux with a stock warmup handler at /. All
// tests that go through this helper assume warmup succeeds; tests that need
// a failing warmup build their own mux + httptest.NewServer directly.
func fakeServer(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("warmup ok"))
	})
	return httptest.NewServer(mux)
}

// ---------- New / Config ----------

func TestNew_DefaultsTimezoneToEuropePrague(t *testing.T) {
	t.Parallel()
	cli, err := New(Config{
		BaseURL:  "https://example.test",
		Username: "u", Password: "p",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cli.Timezone().String() != "Europe/Prague" {
		t.Errorf("Timezone() = %q, want Europe/Prague", cli.Timezone().String())
	}
}

func TestNew_NormalizesDefaultPortInBaseURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string // expected u.Host on the parsed baseURL
	}{
		{name: "https with :443 stripped", in: "https://school.test:443", want: "school.test"},
		{name: "http with :80 stripped", in: "http://school.test:80", want: "school.test"},
		{name: "https without port unchanged", in: "https://school.test", want: "school.test"},
		{name: "custom port preserved", in: "https://school.test:8443", want: "school.test:8443"},
		{name: "http with :443 preserved (not default)", in: "http://school.test:443", want: "school.test:443"},
		// IPv6 literals: stripping the default port must keep the brackets so
		// the Host stays a valid URL authority.
		{name: "https IPv6 with :443 stripped keeps brackets", in: "https://[::1]:443", want: "[::1]"},
		{name: "http IPv6 with :80 stripped keeps brackets", in: "http://[::1]:80", want: "[::1]"},
		{name: "IPv6 custom port preserved", in: "https://[::1]:8443", want: "[::1]:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// AllowInsecureHTTP so the http:// port cases exercise port
			// normalization rather than tripping the insecure-scheme guard.
			cli, err := New(Config{BaseURL: tc.in, Username: "u", Password: "p", AllowInsecureHTTP: true})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if cli.baseURL.Host != tc.want {
				t.Errorf("baseURL.Host = %q, want %q", cli.baseURL.Host, tc.want)
			}
		})
	}
}

func TestNew_PreservesProvidedTimezone(t *testing.T) {
	t.Parallel()
	cli, err := New(Config{
		BaseURL:  "https://example.test",
		Username: "u", Password: "p",
		Timezone: time.UTC,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cli.Timezone() != time.UTC {
		t.Errorf("Timezone() = %v, want time.UTC", cli.Timezone())
	}
}

func TestNew_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "no Username", cfg: Config{BaseURL: "https://x", Password: "p"}},
		{name: "no Password", cfg: Config{BaseURL: "https://x", Username: "u"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tc.cfg); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestNew_RejectsBadBaseURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		baseURL string
		wantMsg string
	}{
		{name: "empty", baseURL: "", wantMsg: "required"},
		{name: "parse error", baseURL: "://bad-url", wantMsg: "parse"},
		{name: "schemeless host", baseURL: "school.edookit.net", wantMsg: "http or https"},
		{name: "unsupported scheme", baseURL: "ftp://school.edookit.net", wantMsg: "http or https"},
		{name: "hostless authority (port only)", baseURL: "https://:443", wantMsg: "no host"},
		{name: "missing host with trailing slash", baseURL: "https:///foo", wantMsg: "no host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(Config{BaseURL: tc.baseURL, Username: "u", Password: "p"})
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.baseURL)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q should mention %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestNew_InsecureHTTPPolicy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		baseURL   string
		allow     bool
		wantError bool
	}{
		{name: "https non-loopback ok", baseURL: "https://school.edookit.net", wantError: false},
		{name: "http non-loopback rejected", baseURL: "http://school.edookit.net", wantError: true},
		{name: "http non-loopback allowed by flag", baseURL: "http://school.edookit.net", allow: true, wantError: false},
		{name: "http localhost ok", baseURL: "http://localhost:8080", wantError: false},
		{name: "http 127.0.0.1 ok", baseURL: "http://127.0.0.1:8080", wantError: false},
		{name: "http ::1 ok", baseURL: "http://[::1]:8080", wantError: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(Config{BaseURL: tc.baseURL, Username: "u", Password: "p", AllowInsecureHTTP: tc.allow})
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error for %q (allow=%v), got nil", tc.baseURL, tc.allow)
				}
				if !strings.Contains(err.Error(), "insecure http") {
					t.Errorf("error %q should mention insecure http", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q (allow=%v): %v", tc.baseURL, tc.allow, err)
			}
		})
	}
}

func TestNew_PreservesCustomHTTPClient(t *testing.T) {
	t.Parallel()
	jar, _ := cookiejar.New(nil)
	custom := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	cli, err := New(Config{
		BaseURL:  "https://example.test",
		Username: "u", Password: "p",
		HTTPClient: custom,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cli.http != custom {
		t.Error("custom HTTPClient was replaced; should have been preserved")
	}
	if cli.http.Jar != jar {
		t.Error("custom jar was replaced; should have been preserved")
	}
}

func TestNew_AddsJarToProvidedClientWithoutOne(t *testing.T) {
	t.Parallel()
	custom := &http.Client{Timeout: 5 * time.Second} // no Jar
	cli, err := New(Config{
		BaseURL:  "https://example.test",
		Username: "u", Password: "p",
		HTTPClient: custom,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cli.http.Jar == nil {
		t.Error("expected jar to be filled in on HTTPClient that lacked one")
	}
}

// ---------- warmupSession ----------

func TestWarmupSession_HappyPath(t *testing.T) {
	t.Parallel()
	srv := fakeServer(t, http.NewServeMux())
	defer srv.Close()

	cli := newClientForTest(t, srv)
	if err := cli.warmupSession(context.Background()); err != nil {
		t.Fatalf("warmupSession: %v", err)
	}
}

func TestWarmupSession_HTTPErrorFails(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := newClientForTest(t, srv)
	err := cli.warmupSession(context.Background())
	if err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %q, want it to mention HTTP 500", err.Error())
	}
}

func TestWarmupSession_OffHostBounceFails(t *testing.T) {
	t.Parallel()

	// "Plus4U" — the foreign host warmup would bounce to. httptest binds
	// to 127.0.0.1, so we swap the host portion of its URL to "localhost"
	// before handing it to Redirect — both resolve to loopback, but they
	// are lexically distinct hostnames so the client's host check sees a
	// real bounce (matching what happens in production, where Plus4U is on
	// a different DNS name from Edookit).
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("login page"))
	}))
	defer foreign.Close()
	foreignURL := strings.Replace(foreign.URL, "127.0.0.1", "localhost", 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, foreignURL, http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := newClientForTest(t, srv)
	err := cli.warmupSession(context.Background())
	if err == nil {
		t.Fatal("expected error on off-host bounce, got nil")
	}
	if !strings.Contains(err.Error(), "bounced off-origin") {
		t.Errorf("error = %q, want it to mention off-origin bounce", err.Error())
	}
}

// ---------- GetJSON ----------

func TestGetJSON_HappyPath(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/handler/page/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authenticated": true, "currentUser": "Alice"}`))
	})
	srv := fakeServer(t, mux)
	defer srv.Close()

	cli := newClientForTest(t, srv)
	var got struct {
		Authenticated bool   `json:"authenticated"`
		CurrentUser   string `json:"currentUser"`
	}
	if err := cli.GetJSON(context.Background(), "/handler/page/dashboard", &got); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if !got.Authenticated || got.CurrentUser != "Alice" {
		t.Errorf("got %+v", got)
	}
}

// authenticated:false on the first hit triggers invalidate+retry; LoginFunc
// returns a fresh cookie; the server's second hit returns true.
func TestGetJSON_AuthenticatedFalseRetries(t *testing.T) {
	t.Parallel()

	var (
		calls   atomic.Int32
		loginCt atomic.Int32
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/handler/page/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"authenticated": false}`))
			return
		}
		_, _ = w.Write([]byte(`{"authenticated": true, "currentUser": "Alice"}`))
	})
	srv := fakeServer(t, mux)
	defer srv.Close()

	cli := newClientForTest(t, srv, func(c *Config) {
		c.LoginFunc = func(_ context.Context) ([]*http.Cookie, error) {
			loginCt.Add(1)
			return []*http.Cookie{{Name: "PHPSESSID", Value: "fresh", Path: "/"}}, nil
		}
	})
	// Preload as if the cookie cache was hit: ensureLoggedIn takes the
	// warmup-only fast path on first call, so LoginFunc isn't called yet.
	preloadJar(t, cli, &http.Cookie{Name: "PHPSESSID", Value: "preloaded", Path: "/"})

	var got struct{ Authenticated bool }
	if err := cli.GetJSON(context.Background(), "/handler/page/dashboard", &got); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if !got.Authenticated {
		t.Error("expected authenticated=true on retry")
	}
	if calls.Load() != 2 {
		t.Errorf("server hit %d times, want 2", calls.Load())
	}
	if loginCt.Load() != 1 {
		t.Errorf("LoginFunc called %d times, want 1 (only during retry after auth:false)", loginCt.Load())
	}
}

// Persistently broken session: every API call says authenticated:false.
// Retry triggers re-login, second call still false, second invalidate, third
// call is fenced by the !retry guard → error.
func TestGetJSON_PersistentAuthFalseGivesUp(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/handler/page/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authenticated": false}`))
	})
	srv := fakeServer(t, mux)
	defer srv.Close()

	cli := newClientForTest(t, srv)
	var got map[string]any
	err := cli.GetJSON(context.Background(), "/handler/page/dashboard", &got)
	if err == nil {
		t.Fatal("expected error after retry also returned authenticated=false, got nil")
	}
	if !strings.Contains(err.Error(), "re-login failed") {
		t.Errorf("error %q should mention re-login failure", err.Error())
	}
}

func TestGetJSON_OffHostBounceTriggersRelogin(t *testing.T) {
	t.Parallel()

	// Same 127.0.0.1 ↔ localhost swap trick as TestWarmupSession_OffHostBounceFails:
	// httptest binds to 127.0.0.1 so we need a lexically different hostname
	// for the bounce check (which compares URL.Hostname()) to fire.
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("plus4u login page"))
	}))
	defer foreign.Close()
	foreignURL := strings.Replace(foreign.URL, "127.0.0.1", "localhost", 1)

	var (
		apiCalls atomic.Int32
		loginCt  atomic.Int32
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/handler/page/dashboard", func(w http.ResponseWriter, r *http.Request) {
		n := apiCalls.Add(1)
		if n == 1 {
			http.Redirect(w, r, foreignURL, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authenticated": true}`))
	})
	srv := fakeServer(t, mux)
	defer srv.Close()

	cli := newClientForTest(t, srv, func(c *Config) {
		c.LoginFunc = func(_ context.Context) ([]*http.Cookie, error) {
			loginCt.Add(1)
			return []*http.Cookie{{Name: "PHPSESSID", Value: "fresh", Path: "/"}}, nil
		}
	})
	preloadJar(t, cli, &http.Cookie{Name: "PHPSESSID", Value: "preloaded", Path: "/"})

	var got struct{ Authenticated bool }
	if err := cli.GetJSON(context.Background(), "/handler/page/dashboard", &got); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if loginCt.Load() != 1 {
		t.Errorf("LoginFunc called %d times, want 1 (off-host bounce should re-login)", loginCt.Load())
	}
}

func TestGetJSON_HTTPErrorFails(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/handler/page/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	})
	srv := fakeServer(t, mux)
	defer srv.Close()

	cli := newClientForTest(t, srv)
	var got map[string]any
	err := cli.GetJSON(context.Background(), "/handler/page/dashboard", &got)
	if err == nil {
		t.Fatal("expected error on HTTP 503")
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("error %q should mention HTTP 503", err.Error())
	}
}

// ---------- invalidateSession ----------

func TestInvalidateSession_ClearsCookiesAndKeepsJar(t *testing.T) {
	t.Parallel()
	srv := fakeServer(t, http.NewServeMux())
	defer srv.Close()

	cli := newClientForTest(t, srv)
	preloadJar(t, cli, &http.Cookie{Name: "PHPSESSID", Value: "x", Path: "/"})
	if got := cli.http.Jar.Cookies(cli.baseURL); len(got) != 1 {
		t.Fatalf("preload: %d cookies, want 1", len(got))
	}
	jarBefore := cli.http.Jar

	cli.loggedIn = true
	cli.invalidateSession()

	if cli.http.Jar != jarBefore {
		t.Error("invalidateSession swapped the Jar field; should clear in place")
	}
	if got := cli.http.Jar.Cookies(cli.baseURL); len(got) != 0 {
		t.Errorf("after invalidate: %d cookies, want 0 (%v)", len(got), got)
	}
	if cli.loggedIn {
		t.Error("loggedIn should be false after invalidate")
	}
}

// ---------- ensureLoggedIn ----------

func TestEnsureLoggedIn_FastPathReusesCookies(t *testing.T) {
	t.Parallel()
	var warmupHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		warmupHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	loginCt := atomic.Int32{}
	cli := newClientForTest(t, srv, func(c *Config) {
		c.LoginFunc = func(_ context.Context) ([]*http.Cookie, error) {
			loginCt.Add(1)
			return []*http.Cookie{{Name: "PHPSESSID", Value: "fresh", Path: "/"}}, nil
		}
	})
	preloadJar(t, cli, &http.Cookie{Name: "PHPSESSID", Value: "preloaded", Path: "/"})

	if err := cli.EnsureLoggedIn(context.Background()); err != nil {
		t.Fatalf("EnsureLoggedIn: %v", err)
	}
	if loginCt.Load() != 0 {
		t.Errorf("LoginFunc called %d times, want 0 (fast path should use cached cookies)", loginCt.Load())
	}
	if warmupHits.Load() != 1 {
		t.Errorf("warmup hit %d times, want 1", warmupHits.Load())
	}
}

func TestEnsureLoggedIn_FallsBackOnWarmupFailure(t *testing.T) {
	t.Parallel()

	var rootHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		// First hit (cached-cookie warmup) fails; subsequent ones succeed —
		// so the chromedp-equivalent fallback can succeed on its own warmup.
		if rootHits.Add(1) == 1 {
			http.Error(w, "first warmup fails", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	loginCt := atomic.Int32{}
	cli := newClientForTest(t, srv, func(c *Config) {
		c.LoginFunc = func(_ context.Context) ([]*http.Cookie, error) {
			loginCt.Add(1)
			return []*http.Cookie{{Name: "PHPSESSID", Value: "fresh", Path: "/"}}, nil
		}
	})
	preloadJar(t, cli, &http.Cookie{Name: "PHPSESSID", Value: "stale", Path: "/"})

	if err := cli.EnsureLoggedIn(context.Background()); err != nil {
		t.Fatalf("EnsureLoggedIn: %v", err)
	}
	if loginCt.Load() != 1 {
		t.Errorf("LoginFunc called %d times, want 1 (warmup failed → fresh login)", loginCt.Load())
	}
}

// ---------- cookie cache integration ----------

func TestCookieCache_LoadAndPersist(t *testing.T) {
	t.Parallel()

	cachePath := t.TempDir() + "/cookies.json"

	// Single server URL shared across both Client instances — otherwise the
	// cookies' captured BaseURL wouldn't match the second client's baseURL
	// and the load would (correctly) reject as stale.
	srv := fakeServer(t, http.NewServeMux())
	defer srv.Close()

	// First Client logs in (via the injected LoginFunc) and persists cookies.
	cli1 := newClientForTest(t, srv, func(c *Config) {
		c.CookieCachePath = cachePath
		c.LoginFunc = func(_ context.Context) ([]*http.Cookie, error) {
			return []*http.Cookie{{Name: "PHPSESSID", Value: "from-login", Path: "/"}}, nil
		}
	})
	if err := cli1.EnsureLoggedIn(context.Background()); err != nil {
		t.Fatalf("first EnsureLoggedIn: %v", err)
	}

	// Second Client (same cachePath, same server) should load the cookies
	// eagerly and warm up without calling LoginFunc.
	cli2, err := New(Config{
		BaseURL:         srv.URL,
		Username:        "u",
		Password:        "p",
		CookieCachePath: cachePath,
		LoginFunc: func(_ context.Context) ([]*http.Cookie, error) {
			t.Errorf("LoginFunc should NOT be called — cache hit + warmup should suffice")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	if err := cli2.EnsureLoggedIn(context.Background()); err != nil {
		t.Fatalf("second EnsureLoggedIn: %v", err)
	}
	cookies := cli2.http.Jar.Cookies(cli2.baseURL)
	if len(cookies) == 0 {
		t.Error("expected cached cookies loaded into jar; got none")
	}
}

func TestCookieCache_StaleCookiesIgnored(t *testing.T) {
	t.Parallel()

	cachePath := t.TempDir() + "/cookies.json"
	// Write a cache file with old timestamp.
	old := cookieFile{
		CapturedAt: time.Now().Add(-100 * time.Hour),
		BaseURL:    "https://stale.test",
		Cookies:    []*http.Cookie{{Name: "PHPSESSID", Value: "x"}},
	}
	data, _ := json.Marshal(old)
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	srv := fakeServer(t, http.NewServeMux())
	defer srv.Close()

	loginCt := atomic.Int32{}
	cli, err := New(Config{
		BaseURL:         srv.URL,
		Username:        "u",
		Password:        "p",
		CookieCachePath: cachePath,
		LoginFunc: func(_ context.Context) ([]*http.Cookie, error) {
			loginCt.Add(1)
			return []*http.Cookie{{Name: "PHPSESSID", Value: "fresh", Path: "/"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Jar should NOT be preloaded — cache was stale (different BaseURL,
	// and CapturedAt is past the 10h default cap).
	if len(cli.http.Jar.Cookies(cli.baseURL)) != 0 {
		t.Error("expected jar empty since cache mismatched baseURL")
	}
	if err := cli.EnsureLoggedIn(context.Background()); err != nil {
		t.Fatalf("EnsureLoggedIn: %v", err)
	}
	if loginCt.Load() != 1 {
		t.Errorf("LoginFunc called %d times, want 1 (stale cache → full login)", loginCt.Load())
	}
}

// ---------- concurrency ----------

// Multiple goroutines calling GetJSON while one fires invalidateSession.
// `go test -race` must report no data race on the jar or loggedIn flag.
func TestConcurrentRequests_NoRaceUnderInvalidation(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/handler/page/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authenticated": true}`))
	})
	srv := fakeServer(t, mux)
	defer srv.Close()

	cli := newClientForTest(t, srv)
	preloadJar(t, cli, &http.Cookie{Name: "PHPSESSID", Value: "x", Path: "/"})
	// Mark logged in so reads-only goroutines don't all serialize on a single
	// ensureLoggedIn — we want the jar to be hit from many goroutines.
	cli.loggedIn = true

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// readers
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var resp map[string]any
				_ = cli.GetJSON(context.Background(), "/handler/page/dashboard", &resp)
			}
		}()
	}

	// invalidator — periodically clears the jar
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			cli.mu.Lock()
			cli.loggedIn = true
			cli.mu.Unlock()
			cli.invalidateSession()
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	// Reaching here without `go test -race` complaining is the assertion.
}

// ---------- off-origin preflight ----------

// An absolute off-origin URL must be refused before any request leaves the
// process, for every request type (JSON / HTML doc / binary download), not
// just GetTo. Uses a server that would record a hit if one slipped through.
// Kept serial (no t.Parallel): the subtests share the evil-hit counter that's
// asserted after they all run, so they must execute before this function
// returns.
func TestRequests_RefuseOffOriginURLBeforeDispatch(t *testing.T) {
	var evil int32
	evilSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&evil, 1)
		_, _ = w.Write([]byte("should never be reached"))
	}))
	defer evilSrv.Close()

	srv := fakeServer(t, http.NewServeMux())
	defer srv.Close()
	cli := newClientForTest(t, srv)

	// Absolute URL pointing at a different origin than baseURL.
	offOrigin := evilSrv.URL + "/handler/grid/objects-for-me-data"

	t.Run("getJSON", func(t *testing.T) {
		var out map[string]any
		err := cli.GetJSON(context.Background(), offOrigin, &out)
		if err == nil || !strings.Contains(err.Error(), "off-origin") {
			t.Errorf("GetJSON err = %v, want an off-origin refusal", err)
		}
	})
	t.Run("getDoc", func(t *testing.T) {
		_, err := cli.GetDoc(context.Background(), offOrigin)
		if err == nil || !strings.Contains(err.Error(), "off-origin") {
			t.Errorf("GetDoc err = %v, want an off-origin refusal", err)
		}
	})
	t.Run("getTo", func(t *testing.T) {
		_, err := cli.GetTo(context.Background(), offOrigin, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "off-origin") {
			t.Errorf("GetTo err = %v, want an off-origin refusal", err)
		}
	})

	if n := atomic.LoadInt32(&evil); n != 0 {
		t.Errorf("off-origin server was hit %d time(s); preflight should block all dispatch", n)
	}
}

// ---------- sameOrigin ----------

func TestSameOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "identical", a: "https://x.test", b: "https://x.test", want: true},
		{name: "default port stripped vs explicit", a: "https://x.test", b: "https://x.test:443", want: true},
		{name: "http with :80 vs no port", a: "http://x.test:80", b: "http://x.test", want: true},
		{name: "different scheme", a: "https://x.test", b: "http://x.test", want: false},
		{name: "different hostname", a: "https://x.test", b: "https://y.test", want: false},
		{name: "same host, different non-default ports", a: "https://x.test:8443", b: "https://x.test:9443", want: false},
		{name: "non-default port vs default", a: "https://x.test:8443", b: "https://x.test", want: false},
		{name: "ipv4 vs hostname even on loopback", a: "http://127.0.0.1:8000", b: "http://localhost:8000", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ua, err := url.Parse(tc.a)
			if err != nil {
				t.Fatalf("parse a: %v", err)
			}
			ub, err := url.Parse(tc.b)
			if err != nil {
				t.Fatalf("parse b: %v", err)
			}
			if got := sameOrigin(ua, ub); got != tc.want {
				t.Errorf("sameOrigin(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// ---------- retry on transient failures ----------

// Two 503s then a 200 — the retry layer should swallow both 503s and the
// caller sees only the eventual success.
func TestDo_RetriesTransientStatus(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/handler/page/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n <= 2 {
			http.Error(w, "upstream timeout", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authenticated": true, "ok": true}`))
	})
	srv := fakeServer(t, mux)
	defer srv.Close()

	cli := newClientForTest(t, srv, func(c *Config) {
		c.MaxAttempts = 3
		c.RetryBaseDelay = time.Millisecond // keep the test fast
	})

	var got struct {
		Authenticated bool `json:"authenticated"`
		OK            bool `json:"ok"`
	}
	if err := cli.GetJSON(context.Background(), "/handler/page/dashboard", &got); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if !got.OK {
		t.Errorf("got %+v, want ok=true", got)
	}
	if hits.Load() != 3 {
		t.Errorf("got %d hits, want exactly 3 (503, 503, 200)", hits.Load())
	}
}

// Persistent 503 — every attempt fails. Expect the error to mention the
// attempt count + status so it's debuggable.
func TestDo_FailsAfterAllAttemptsExhausted(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/handler/page/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "still down", http.StatusServiceUnavailable)
	})
	srv := fakeServer(t, mux)
	defer srv.Close()

	cli := newClientForTest(t, srv, func(c *Config) {
		c.MaxAttempts = 3
		c.RetryBaseDelay = time.Millisecond
	})

	var got map[string]any
	err := cli.GetJSON(context.Background(), "/handler/page/dashboard", &got)
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	if !strings.Contains(err.Error(), "3 attempt") {
		t.Errorf("error %q should mention the attempt count", err.Error())
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q should mention the underlying HTTP 503", err.Error())
	}
	if hits.Load() != 3 {
		t.Errorf("got %d hits, want 3 (one per attempt)", hits.Load())
	}
}

// A 500 should NOT be retried — those are deterministic application bugs
// from Edookit, not transient infrastructure hiccups. Retrying them would
// just hide the bug and waste time.
func TestDo_DoesNotRetryDeterministic5xx(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/handler/page/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := fakeServer(t, mux)
	defer srv.Close()

	cli := newClientForTest(t, srv, func(c *Config) {
		c.MaxAttempts = 3
		c.RetryBaseDelay = time.Millisecond
	})

	var got map[string]any
	err := cli.GetJSON(context.Background(), "/handler/page/dashboard", &got)
	if err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
	if hits.Load() != 1 {
		t.Errorf("got %d hits, want 1 (500 must not retry)", hits.Load())
	}
}

// Context cancellation during the backoff window must short-circuit the
// retry loop and surface the cancellation, not the prior HTTP error.
func TestDo_HonorsContextCancellation(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/handler/page/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	srv := fakeServer(t, mux)
	defer srv.Close()

	cli := newClientForTest(t, srv, func(c *Config) {
		c.MaxAttempts = 5
		c.RetryBaseDelay = 500 * time.Millisecond // long enough to cancel inside
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var got map[string]any
	err := cli.GetJSON(ctx, "/handler/page/dashboard", &got)
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want a context cancellation error", err)
	}
}
