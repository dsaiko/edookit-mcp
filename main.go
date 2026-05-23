package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/dsaiko/edookit-mcp/internal/client"
	"github.com/dsaiko/edookit-mcp/internal/tools"
)

func main() {
	cli, err := client.New(client.Config{
		BaseURL:   getenvRequired("EDOOKIT_URL"),
		Username:  getenvRequired("EDOOKIT_USER"),
		Password:  getenvRequired("EDOOKIT_PASS"),
		LoginPath: getenv("EDOOKIT_LOGIN_PATH", "/login"),
		UserField: getenv("EDOOKIT_USER_FIELD", "username"),
		PassField: getenv("EDOOKIT_PASS_FIELD", "password"),
		CSRFField: os.Getenv("EDOOKIT_CSRF_FIELD"), // optional
	})
	if err != nil {
		log.Fatalf("init client: %v", err)
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

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvRequired(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}
