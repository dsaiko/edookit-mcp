package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	// Embed the IANA tzdata so time.LoadLocation works on hosts without
	// /usr/share/zoneinfo (Windows binaries, slim containers).
	_ "time/tzdata"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/dsaiko/edookit-mcp/internal/client"
	"github.com/dsaiko/edookit-mcp/internal/tools"
)

// Build-time metadata, populated by GoReleaser via -ldflags. Empty in dev builds.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	loginTest := flag.Bool("login-test", false, "perform OIDC login once and exit (smoke test)")
	dumpHTML := flag.Bool("dump-html", false, "navigate to EDOOKIT_URL, dump body HTML, exit (selector debugging)")
	clearCookies := flag.Bool("clear-cookies", false, "delete the cached session cookies and exit")
	testMessages := flag.Bool("test-messages", false, "list a few inbox + sent messages and exit (smoke test for the tools)")
	showVersion := flag.Bool("version", false, "print version and commit, then exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("edookit-mcp %s (commit %s)\n", version, commit)
		return
	}
	if *clearCookies {
		runClearCookies()
		return
	}
	if *dumpHTML {
		runDumpHTML(getenvRequired("EDOOKIT_URL"), getenvBool("EDOOKIT_HEADLESS_LOGIN", true))
		return
	}

	cli, err := client.New(client.Config{
		BaseURL:         getenvRequired("EDOOKIT_URL"),
		Username:        getenvRequired("EDOOKIT_USER"),
		Password:        getenvRequired("EDOOKIT_PASS"),
		HeadlessLogin:   getenvBool("EDOOKIT_HEADLESS_LOGIN", true),
		CookieCachePath: cookieCachePath(),
		Timezone:        loadTimezone(),
	})
	if err != nil {
		log.Fatalf("init client: %v", err)
	}

	if *loginTest {
		runLoginTest(cli)
		return
	}
	if *testMessages {
		runTestMessages(cli)
		return
	}

	s := server.NewMCPServer(
		"edookit-mcp",
		version,
		server.WithToolCapabilities(true),
	)

	registerInboxTool(s, cli)
	registerSentTool(s, cli)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("serve stdio: %v", err)
	}
}

func registerInboxTool(s *server.MCPServer, cli *client.Client) {
	s.AddTool(
		mcp.NewTool("list_inbox",
			mcp.WithDescription("List messages in the Edookit inbox (Komunikace → Přijaté). "+
				"Returns the most recent messages first. Each result has id, date, sender, "+
				"subject, body_preview (first ~200 chars), and attachments count."),
			mcp.WithString("view",
				mcp.Description("Which subset to list: 'inbox' (default), 'unread' (Nepřečtené), "+
					"'starred' (S hvězdičkou), 'archived' (Archiv), 'all' (Vše)."),
			),
			mcp.WithString("fulltext",
				mcp.Description("Optional server-side full-text search across senders, subjects, and bodies."),
			),
			mcp.WithString("since",
				mcp.Description("Optional client-side date floor. Accepts '7d', '1w', '2m', '1y', "+
					"or an ISO date 'YYYY-MM-DD'. Messages older than this are excluded."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Max messages to return. Default 50, max 200. Paginates internally if needed."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			opts := tools.InboxOptions{
				View:     req.GetString("view", ""),
				Fulltext: req.GetString("fulltext", ""),
				Since:    req.GetString("since", ""),
				Limit:    int(req.GetFloat("limit", 0)),
			}
			msgs, err := tools.ListInbox(ctx, cli, opts)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, err := json.Marshal(msgs)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("marshal: %v", err)), nil
			}
			return mcp.NewToolResultText(string(b)), nil
		},
	)
}

func registerSentTool(s *server.MCPServer, cli *client.Client) {
	s.AddTool(
		mcp.NewTool("list_sent",
			mcp.WithDescription("List messages the user has sent (Komunikace → Vytvořené). "+
				"Returns the most recent first. Each result has id, date, status (e.g. 'Publikováno'), "+
				"subject, body_preview, and attachments count."),
			mcp.WithString("fulltext",
				mcp.Description("Optional server-side full-text search across subjects and bodies."),
			),
			mcp.WithString("since",
				mcp.Description("Optional client-side date floor. Accepts '7d', '1w', '2m', '1y', "+
					"or an ISO date 'YYYY-MM-DD'. Messages older than this are excluded."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Max messages to return. Default 50, max 200. Paginates internally if needed."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			opts := tools.SentOptions{
				Fulltext: req.GetString("fulltext", ""),
				Since:    req.GetString("since", ""),
				Limit:    int(req.GetFloat("limit", 0)),
			}
			msgs, err := tools.ListSent(ctx, cli, opts)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, err := json.Marshal(msgs)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("marshal: %v", err)), nil
			}
			return mcp.NewToolResultText(string(b)), nil
		},
	)
}

func runDumpHTML(baseURL string, headless bool) {
	ctx := context.Background()
	log.Printf("dumping landing HTML from %s (headless=%t)...", baseURL, headless)
	html, err := client.DumpLandingHTML(ctx, baseURL, headless)
	if err != nil {
		log.Fatalf("dump-html failed: %v", err)
	}
	if _, err := os.Stdout.WriteString(html); err != nil {
		log.Fatalf("write stdout: %v", err)
	}
}

func runLoginTest(cli *client.Client) {
	ctx := context.Background()
	log.Printf("ensuring login session (chromium launches only if cache is cold)...")
	if err := cli.EnsureLoggedIn(ctx); err != nil {
		log.Fatalf("login failed: %v", err)
	}
	cookies := cli.SessionCookies()
	log.Printf("session ready — %d cookie(s) available for target host", len(cookies))

	var probe map[string]any
	if err := cli.GetJSON(ctx, "/handler/page/dashboard", &probe); err != nil {
		log.Fatalf("dashboard probe failed: %v", err)
	}
	auth, _ := probe["authenticated"].(bool)
	if !auth {
		// GetJSON should have already re-authed on authenticated=false, so
		// getting here means warmup is genuinely broken. Fail loudly so CI
		// and humans can't miss it.
		log.Fatalf("/handler/page/dashboard returned authenticated=false despite successful login — warmup is broken")
	}
	log.Printf("authenticated session verified via /handler/page/dashboard")
}

func runTestMessages(cli *client.Client) {
	ctx := context.Background()

	log.Printf("=== INBOX (3 most recent) ===")
	inbox, err := tools.ListInbox(ctx, cli, tools.InboxOptions{Limit: 3})
	if err != nil {
		log.Fatalf("list inbox: %v", err)
	}
	for _, m := range inbox.Messages {
		log.Printf("  [%s] %s | %s | %q | attachments=%d",
			m.ID, m.Date, m.Sender, m.Subject, m.Attachments)
		if m.BodyPreview != "" {
			log.Printf("    %s", m.BodyPreview)
		}
	}
	for _, w := range inbox.ParseWarnings {
		log.Printf("  [parse-warning] %s", w)
	}

	log.Printf("=== SENT (3 most recent) ===")
	sent, err := tools.ListSent(ctx, cli, tools.SentOptions{Limit: 3})
	if err != nil {
		log.Fatalf("list sent: %v", err)
	}
	for _, m := range sent.Messages {
		log.Printf("  [%s] %s | %s | %q | attachments=%d",
			m.ID, m.Date, m.Status, m.Subject, m.Attachments)
		if m.BodyPreview != "" {
			log.Printf("    %s", m.BodyPreview)
		}
	}
	for _, w := range sent.ParseWarnings {
		log.Printf("  [parse-warning] %s", w)
	}

	log.Printf("=== INBOX UNREAD ===")
	unread, err := tools.ListInbox(ctx, cli, tools.InboxOptions{View: "unread", Limit: 5})
	if err != nil {
		log.Fatalf("list unread: %v", err)
	}
	log.Printf("%d unread message(s)", len(unread.Messages))
	for _, m := range unread.Messages {
		log.Printf("  [%s] %s | %s | %q", m.ID, m.Date, m.Sender, m.Subject)
	}
}

// cookieCachePath returns the path where session cookies should be persisted:
// EDOOKIT_NO_COOKIE_CACHE=true disables caching entirely (returns "");
// EDOOKIT_COOKIE_CACHE=<path> overrides the default; otherwise we use
// client.DefaultCookieCachePath().
func cookieCachePath() string {
	// getenvBool Fatalfs on invalid values like "yes" or "1.5", so a typo in
	// .env surfaces immediately instead of silently leaving the cache on.
	if getenvBool("EDOOKIT_NO_COOKIE_CACHE", false) {
		return ""
	}
	if v := os.Getenv("EDOOKIT_COOKIE_CACHE"); v != "" {
		return v
	}
	p, err := client.DefaultCookieCachePath()
	if err != nil {
		log.Printf("warning: cannot determine default cookie cache path (%v); persistence disabled", err)
		return ""
	}
	return p
}

// loadTimezone resolves EDOOKIT_TIMEZONE (defaulting to Europe/Prague) into
// a *time.Location. Edookit row dates are rendered in the school's wall-clock
// time with no offset suffix, so we have to anchor parsing to an explicit
// Location — otherwise the MCP would emit wrong UTC offsets when running on
// a host outside the school's timezone. tzdata is embedded via `time/tzdata`
// so this works on hosts without /usr/share/zoneinfo (Windows, slim images).
func loadTimezone() *time.Location {
	name := os.Getenv("EDOOKIT_TIMEZONE")
	if name == "" {
		name = "Europe/Prague"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		log.Fatalf("invalid EDOOKIT_TIMEZONE: %v", err)
	}
	return loc
}

func runClearCookies() {
	path := cookieCachePath()
	if path == "" {
		log.Printf("cookie cache is disabled — nothing to clear")
		return
	}
	switch err := os.Remove(path); {
	case err == nil:
		log.Printf("removed %s", path)
	case os.IsNotExist(err):
		log.Printf("no cached cookies at %s", path)
	default:
		log.Fatalf("remove %s: %v", path, err)
	}
}

func getenvRequired(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}

func getenvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Fatalf("env var %s is not a valid bool: %v", key, err)
	}
	return b
}
