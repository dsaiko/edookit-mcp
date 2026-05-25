package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/dsaiko/edookit-mcp/internal/client"
	"github.com/dsaiko/edookit-mcp/internal/tools"
)

func main() {
	loginTest := flag.Bool("login-test", false, "perform OIDC login once and exit (smoke test)")
	dumpHTML := flag.Bool("dump-html", false, "navigate to EDOOKIT_URL, dump body HTML, exit (selector debugging)")
	flag.Parse()

	if *dumpHTML {
		runDumpHTML(getenvRequired("EDOOKIT_URL"), getenvBool("EDOOKIT_HEADLESS_LOGIN", true))
		return
	}

	cli, err := client.New(client.Config{
		BaseURL:       getenvRequired("EDOOKIT_URL"),
		Username:      getenvRequired("EDOOKIT_USER"),
		Password:      getenvRequired("EDOOKIT_PASS"),
		HeadlessLogin: getenvBool("EDOOKIT_HEADLESS_LOGIN", true),
	})
	if err != nil {
		log.Fatalf("init client: %v", err)
	}

	if *loginTest {
		runLoginTest(cli)
		return
	}

	s := server.NewMCPServer(
		"edookit-mcp",
		"0.1.0",
		server.WithToolCapabilities(true),
	)

	s.AddTool(
		mcp.NewTool("get_grades",
			mcp.WithDescription("Fetch the user's grades from Edookit, optionally filtered by period."),
			mcp.WithString("period",
				mcp.Description("Period identifier as used by the site, e.g. '2025-spring'. Optional."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			period := req.GetString("period", "")
			grades, err := tools.GetGrades(ctx, cli, period)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, err := json.Marshal(grades)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("marshal: %v", err)), nil
			}
			return mcp.NewToolResultText(string(b)), nil
		},
	)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("serve stdio: %v", err)
	}
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
	log.Printf("running OIDC login (this launches chromium)...")
	if err := cli.EnsureLoggedIn(ctx); err != nil {
		log.Fatalf("login failed: %v", err)
	}
	cookies := cli.SessionCookies()
	log.Printf("login OK — %d cookie(s) captured for target host:", len(cookies))
	for _, c := range cookies {
		log.Printf("  %s (len=%d, secure=%t, httpOnly=%t, sameSite=%d)",
			c.Name, len(c.Value), c.Secure, c.HttpOnly, c.SameSite)
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
