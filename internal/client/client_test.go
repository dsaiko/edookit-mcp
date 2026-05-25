package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
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
// chromedp never runs.
func newClientForTest(t *testing.T, srv *httptest.Server, configure ...func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		BaseURL:  srv.URL,
		Username: "u",
		Password: "p",
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cli, err := New(Config{BaseURL: tc.in, Username: "u", Password: "p"})
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
	if !strings.Contains(err.Error(), "bounced off-host") {
		t.Errorf("error = %q, want it to mention off-host bounce", err.Error())
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
