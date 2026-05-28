package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"

	"github.com/dsaiko/edookit-mcp/internal/client"
)

func TestGetenvBool(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		def  bool
		want bool
	}{
		{name: "unset uses default true", set: false, def: true, want: true},
		{name: "unset uses default false", set: false, def: false, want: false},
		{name: "true overrides default false", set: true, val: "true", def: false, want: true},
		{name: "false overrides default true", set: true, val: "false", def: true, want: false},
		{name: "1 parses true", set: true, val: "1", def: false, want: true},
		{name: "0 parses false", set: true, val: "0", def: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const key = "EDOOKIT_TEST_BOOL"
			if tc.set {
				t.Setenv(key, tc.val)
			} else {
				t.Setenv(key, "")
			}
			if got := getenvBool(key, tc.def); got != tc.want {
				t.Errorf("getenvBool(%q=%q, def=%v) = %v, want %v", key, tc.val, tc.def, got, tc.want)
			}
		})
	}
}

func TestGetenvRequired_ReturnsSetValue(t *testing.T) {
	t.Setenv("EDOOKIT_TEST_REQUIRED", "hello")
	if got := getenvRequired("EDOOKIT_TEST_REQUIRED"); got != "hello" {
		t.Errorf("getenvRequired = %q, want %q", got, "hello")
	}
}

func TestLoadTimezone(t *testing.T) {
	t.Run("default is Europe/Prague", func(t *testing.T) {
		t.Setenv("EDOOKIT_TIMEZONE", "")
		if loc := loadTimezone(); loc.String() != "Europe/Prague" {
			t.Errorf("default timezone = %q, want Europe/Prague", loc)
		}
	})
	t.Run("override honored", func(t *testing.T) {
		t.Setenv("EDOOKIT_TIMEZONE", "UTC")
		if loc := loadTimezone(); loc.String() != "UTC" {
			t.Errorf("timezone = %q, want UTC", loc)
		}
	})
}

func TestCookieCachePath(t *testing.T) {
	t.Run("disabled returns empty", func(t *testing.T) {
		t.Setenv("EDOOKIT_NO_COOKIE_CACHE", "true")
		t.Setenv("EDOOKIT_COOKIE_CACHE", "/should/be/ignored")
		if got := cookieCachePath(); got != "" {
			t.Errorf("cookieCachePath = %q, want empty (caching disabled)", got)
		}
	})
	t.Run("explicit override honored", func(t *testing.T) {
		t.Setenv("EDOOKIT_NO_COOKIE_CACHE", "false")
		want := filepath.Join(t.TempDir(), "cookies.json")
		t.Setenv("EDOOKIT_COOKIE_CACHE", want)
		if got := cookieCachePath(); got != want {
			t.Errorf("cookieCachePath = %q, want %q", got, want)
		}
	})
}

// registerAllTools wires every tool onto a fresh server without panicking, and
// the resulting server is usable. This is a smoke test for the registration
// funcs (the handler bodies themselves talk to live Edookit and are exercised
// via the tools package's tests).
func TestRegisterTools_NoPanic(t *testing.T) {
	cli, err := client.New(client.Config{
		BaseURL:  "https://school.test",
		Username: "u",
		Password: "p",
		LoginFunc: func(_ context.Context) ([]*http.Cookie, error) {
			return []*http.Cookie{{Name: "PHPSESSID", Value: "x", Path: "/"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	s := server.NewMCPServer("edookit-mcp-test", "test", server.WithToolCapabilities(true))
	registerInboxTool(s, cli)
	registerSentTool(s, cli)
	registerGetMessageTool(s, cli)
	registerDownloadAttachmentsTool(s, cli)
	registerViewAttachmentTool(s, cli)
	registerListCoursesTool(s, cli)
	registerServerInfoTool(s)
}

// Smoke test for the Streamable HTTP transport: the server must answer a
// JSON-RPC `initialize` POST to `/mcp` with HTTP 200 and a JSON-RPC result.
// Exercises the wiring used by `runHTTP` / `--http`, without touching a real
// listener (httptest gives us the same http.Handler path).
func TestHTTPTransport_Initialize(t *testing.T) {
	t.Parallel()
	s := server.NewMCPServer("edookit-mcp-test", "test", server.WithToolCapabilities(true))
	ts := httptest.NewServer(server.NewStreamableHTTPServer(s))
	defer ts.Close()

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize",` +
		`"params":{"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"smoke","version":"1"}}}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mcp", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, got)
	}
	if !strings.Contains(string(got), `"result"`) {
		t.Errorf("body = %s, want a JSON-RPC result envelope", got)
	}
}

func TestBuildServerInfo(t *testing.T) {
	info := buildServerInfo()
	// In tests the ldflags aren't set, so these hold the dev placeholders —
	// the point is that the fields are wired to the build vars.
	if info.Version != version || info.Commit != commit || info.BuildTime != date {
		t.Errorf("buildServerInfo() = %+v, want it to mirror version/commit/date (%q/%q/%q)",
			info, version, commit, date)
	}
}
