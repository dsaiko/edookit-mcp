package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	// Embed the IANA tzdata so time.LoadLocation works on hosts without
	// /usr/share/zoneinfo (Windows binaries, slim containers).
	_ "time/tzdata"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/dsaiko/edookit-mcp/internal/client"
	"github.com/dsaiko/edookit-mcp/internal/tools"
)

// Build-time metadata, populated by GoReleaser via -ldflags. Placeholder
// values in dev builds.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// serverInfo is the build metadata returned by edookit_server_info.
type serverInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

func buildServerInfo() serverInfo {
	return serverInfo{Version: version, Commit: commit, BuildTime: date}
}

func main() {
	loginTest := flag.Bool("login-test", false, "perform OIDC login once and exit (smoke test)")
	dumpHTML := flag.Bool("dump-html", false, "navigate to EDOOKIT_URL, dump body HTML, exit (selector debugging)")
	clearCookies := flag.Bool("clear-cookies", false, "delete the cached session cookies and exit")
	testMessages := flag.Bool("test-messages", false, "list a few inbox + sent messages and exit (smoke test for the tools)")
	dumpMessage := flag.String("dump-message", "", "(dev) fetch the full body of the given message ID (e.g. m-290491 or 290491) and dump the raw JSON response across all plausible endpoints — used to reverse-engineer the full-message + attachments API shape")
	getMessage := flag.String("get-message", "", "(dev) call tools.GetMessage for the given ID and print the resulting FullMessage JSON (smoke test for the parser)")
	showVersion := flag.Bool("version", false, "print version and commit, then exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("edookit-mcp %s (commit %s, built %s)\n", version, commit, date)
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
		BaseURL:           getenvRequired("EDOOKIT_URL"),
		Username:          getenvRequired("EDOOKIT_USER"),
		Password:          getenvRequired("EDOOKIT_PASS"),
		HeadlessLogin:     getenvBool("EDOOKIT_HEADLESS_LOGIN", true),
		AllowInsecureHTTP: getenvBool("EDOOKIT_ALLOW_INSECURE_HTTP", false),
		CookieCachePath:   cookieCachePath(),
		Timezone:          loadTimezone(),
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
	if *getMessage != "" {
		runGetMessage(cli, *getMessage)
		return
	}
	if *dumpMessage != "" {
		runDumpMessage(cli, *dumpMessage)
		return
	}

	s := server.NewMCPServer(
		"edookit-mcp",
		version,
		server.WithToolCapabilities(true),
	)

	registerInboxTool(s, cli)
	registerSentTool(s, cli)
	registerGetMessageTool(s, cli)
	registerDownloadAttachmentsTool(s, cli)
	registerViewAttachmentTool(s, cli)
	registerListCoursesTool(s, cli)
	registerServerInfoTool(s)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("serve stdio: %v", err)
	}
}

func registerInboxTool(s *server.MCPServer, cli *client.Client) {
	s.AddTool(
		mcp.NewTool("edookit_list_inbox",
			mcp.WithDescription("List received messages from the **Edookit school information system** "+
				"(Komunikace → Přijaté). Edookit is a Czech educational platform used by "+
				"schools to communicate with parents and students. Use this tool when the "+
				"user asks about school messages — anything from teachers, the school "+
				"office, the head teacher (třídní učitel), the principal (ředitel), or "+
				"about school topics like grades, attendance, parent-teacher meetings, "+
				"trips, exams. This is NOT a general email inbox — for Gmail / Outlook / "+
				"Slack DMs use those dedicated tools instead. Returns a JSON object with "+
				"two keys: `messages` is an array of message objects (id, date, sender, "+
				"subject, body_preview ~200 chars, attachments count) in newest-first "+
				"order; `parse_warnings` (optional) lists any rows the server returned "+
				"that couldn't be parsed — usually means Edookit's row HTML changed. An "+
				"empty messages array with no warnings means the mailbox itself is empty; "+
				"an error is returned if every fetched row failed to parse."),
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
		mcp.NewTool("edookit_list_sent",
			mcp.WithDescription("List messages the user has sent via the **Edookit school "+
				"information system** (Komunikace → Vytvořené). Edookit is a Czech "+
				"educational platform used by schools to communicate with parents and "+
				"students. Use this tool when the user asks about messages they sent to "+
				"the school — to teachers, the head teacher (třídní), the principal, or "+
				"about school topics. This is NOT a general sent-mail folder — for Gmail "+
				"/ Outlook / Slack DMs use those dedicated tools instead. Returns a JSON "+
				"object with two keys: `messages` is an array of message objects (id, "+
				"date, status like 'Publikováno', subject, body_preview, attachments "+
				"count) in newest-first order; `parse_warnings` (optional) lists any rows "+
				"the server returned that couldn't be parsed. An empty messages array "+
				"with no warnings means nothing has been sent; an error is returned if "+
				"every fetched row failed to parse."),
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

func registerGetMessageTool(s *server.MCPServer, cli *client.Client) {
	s.AddTool(
		mcp.NewTool("edookit_get_message",
			mcp.WithDescription("Fetch the full body, attachment list, and read-receipt "+
				"table of a single message from the **Edookit school information system** "+
				"(works for both received and sent messages — Edookit serves them via the "+
				"same endpoint). Use this after edookit_list_inbox or edookit_list_sent "+
				"has surfaced a message ID the user is interested in: the list tools "+
				"return only a ~200-character body preview, while this tool returns the "+
				"full message body in both plain text and HTML form, plus the metadata "+
				"needed to download attachments AND a delivery / read-receipt table "+
				"(Edookit calls it 'Doručenky'). Returns a JSON object with id, number, "+
				"subject, status (e.g. 'Publikováno'), author (sender for received, "+
				"publisher for sent — typically the user themselves), date (RFC3339), "+
				"body_text (plain text), body_html (original HTML), deleted (true if "+
				"the message was deleted by its author — Edookit then strips subject "+
				"and body, only status/author/date survive), attachments — array of "+
				"{id, name, url, date}, and recipients — array of {name, read_at (ISO "+
				"date or empty if not yet read), parents (list), parents_read_at "+
				"(aligned with parents)}. For sent messages the recipients array tells "+
				"the author who read the message and when; for received messages it "+
				"typically lists only the current user. To actually save attachment "+
				"files to disk, use edookit_download_attachments instead (this tool only "+
				"lists them)."),
			mcp.WithString("id",
				mcp.Required(),
				mcp.Description("Message identifier as returned by edookit_list_inbox / edookit_list_sent. "+
					"Accepts either the 'm-NNNNNN' UID form or the bare 'NNNNNN' number."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := req.GetString("id", "")
			if id == "" {
				return mcp.NewToolResultError("missing required parameter: id"), nil
			}
			msg, err := tools.GetMessage(ctx, cli, id)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, err := json.Marshal(msg)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("marshal: %v", err)), nil
			}
			return mcp.NewToolResultText(string(b)), nil
		},
	)
}

func registerDownloadAttachmentsTool(s *server.MCPServer, cli *client.Client) {
	s.AddTool(
		mcp.NewTool("edookit_download_attachments",
			mcp.WithDescription("Download every attachment of a single Edookit message to "+
				"a local directory and return the saved file paths. Works for both received "+
				"and sent messages. Files are written with the original filenames Edookit "+
				"reports, into the directory given by `destination_dir`. The directory is "+
				"created if it doesn't exist (mode 0700). If the same filename already "+
				"exists in the directory it is left alone (download is skipped) unless "+
				"`overwrite=true` is also passed. Returns a JSON object with message_id, "+
				"directory, and a files array of {name, path, bytes, skipped?, error?} — "+
				"a per-attachment outcome. A populated `error` on one entry means just "+
				"that file failed; the others continue. Use this when the user asks to "+
				"download, save, or open attachments — for example after edookit_get_message "+
				"surfaces an attachment list."),
			mcp.WithString("id",
				mcp.Required(),
				mcp.Description("Message identifier as returned by edookit_list_inbox / "+
					"edookit_list_sent / edookit_get_message. Accepts 'm-NNNNNN' or 'NNNNNN'."),
			),
			mcp.WithString("destination_dir",
				mcp.Description("Local filesystem directory where attachments will be saved. "+
					"Optional — defaults to <os-temp-dir>/edookit-mcp/m-<number>/, "+
					"where <number> is the numeric portion of the message id (so "+
					"id='m-289862' AND id='289862' both land in 'm-289862/'). Portable "+
					"across operating systems (/tmp/... on Linux/macOS, %TMP%\\... on "+
					"Windows) and gets garbage-collected by the OS. Pass an explicit "+
					"path for persistent storage. Accepted: an absolute path, a path "+
					"starting with ~/ (expanded to under the user's home dir), or a "+
					"bare ~ (the home dir itself). Relative paths are rejected because "+
					"the MCP server's cwd is whatever started the host application and "+
					"not a stable anchor. The directory is created if missing."),
			),
			mcp.WithBoolean("overwrite",
				mcp.Description("If true, existing files at the destination are overwritten. "+
					"Default false — existing files are kept and reported as skipped."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := req.GetString("id", "")
			if id == "" {
				return mcp.NewToolResultError("missing required parameter: id"), nil
			}
			opts := tools.DownloadOptions{
				DestDir:   req.GetString("destination_dir", ""),
				Overwrite: req.GetBool("overwrite", false),
			}
			res, err := tools.DownloadAttachments(ctx, cli, id, opts)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, err := json.Marshal(res)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("marshal: %v", err)), nil
			}
			return mcp.NewToolResultText(string(b)), nil
		},
	)
}

func registerViewAttachmentTool(s *server.MCPServer, cli *client.Client) {
	s.AddTool(
		mcp.NewTool("edookit_view_attachment",
			mcp.WithDescription("View a single attachment of an **Edookit** message *inline* in "+
				"the conversation — no file is written to disk. Use this (instead of "+
				"edookit_download_attachments) when the user wants to SEE or READ an "+
				"attachment's content directly: a photo/scan, a PDF (including scanned/"+
				"image-only ones), or a text/CSV file. Returns MCP content blocks the client "+
				"renders directly: images come back as image content (downscaled if very "+
				"large); PDFs are rendered to PNG page images (the first `max_pages` pages) "+
				"plus the whole document's extracted text, so even scanned/image-only PDFs "+
				"are shown; text-like files come back as their decoded content. Office "+
				"documents (doc/xls/ppt) and other binary types can't be shown inline — for "+
				"those (or to keep a local copy) use edookit_download_attachments. Find "+
				"attachment ids via edookit_get_message (each attachment has an `id` like "+
				"`1@191207`)."),
			mcp.WithString("id",
				mcp.Required(),
				mcp.Description("Message identifier (m-NNNNNN or NNNNNN), as returned by the list/get tools."),
			),
			mcp.WithString("attachment_id",
				mcp.Required(),
				mcp.Description("Attachment id from edookit_get_message's attachments array, e.g. \"1@191207\"."),
			),
			mcp.WithNumber("max_size_mb",
				mcp.Description("Inline size cap in MB. Default 8, hard max 25. Larger attachments "+
					"return a note pointing at edookit_download_attachments instead."),
			),
			mcp.WithNumber("max_pages",
				mcp.Description("For PDFs: how many pages to render to images. Default 5, hard max 20. "+
					"Extracted text always covers the whole document regardless."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := req.GetString("id", "")
			attID := req.GetString("attachment_id", "")
			if id == "" || attID == "" {
				return mcp.NewToolResultError("missing required parameter: both id and attachment_id are required"), nil
			}
			res, err := tools.ViewAttachment(ctx, cli, id, attID, tools.ViewOptions{
				MaxSizeMB: int(req.GetFloat("max_size_mb", 0)),
				MaxPages:  int(req.GetFloat("max_pages", 0)),
			})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			content := make([]mcp.Content, 0, len(res.Blocks))
			for _, b := range res.Blocks {
				if b.ImageB64 != "" {
					content = append(content, mcp.NewImageContent(b.ImageB64, b.ImageMime))
				} else {
					content = append(content, mcp.NewTextContent(b.Text))
				}
			}
			return &mcp.CallToolResult{Content: content}, nil
		},
	)
}

func registerListCoursesTool(s *server.MCPServer, cli *client.Client) {
	s.AddTool(
		mcp.NewTool("edookit_list_courses",
			mcp.WithDescription("List the signed-in teacher's **Edookit** courses — what the "+
				"user calls *moje třídy / moje kurzy / moje skupiny* (the courses shown in "+
				"Hodnocení → Známkování v tabulce). A course is a subject taught to a class "+
				"or group, e.g. \"AUT - 4SA\" (whole class) with its split half-groups "+
				"\"AUT 1 - 4SA\" / \"AUT 2 - 4SA\" (split_group=true). Use this for questions "+
				"like \"which classes/courses do I teach\", \"list my groups\", or as the way "+
				"to find a course_id before listing its pupils. Returns a JSON array of "+
				"{course_id, name, split_group, students?, error?}. By default (no arguments) "+
				"it returns just the course list — one cheap request. Pass `course_id` to get "+
				"one course **with its student roster** (žáci: {study_id, name, class}); a "+
				"half-group's roster is the subset of the class in that half. Pass "+
				"`include_students=true` to populate every course's roster at once (heavier — "+
				"one request per course; pupils of a class repeat under its half-groups). In "+
				"that mode a course whose roster failed to load carries a non-empty `error` "+
				"field (and no `students`), so an empty class is distinguishable from a "+
				"failed fetch — don't treat a missing roster as 'no pupils' when `error` is set."),
			mcp.WithString("course_id",
				mcp.Description("Return just this course with its student roster. Value is a "+
					"course_id from a prior no-argument call, e.g. \"myc-22909-20102\"."),
			),
			mcp.WithBoolean("include_students",
				mcp.Description("Populate every course's student roster (heavier; ignored when "+
					"course_id is set). Default false = course list only."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			courses, err := tools.ListCourses(ctx, cli, tools.CoursesOptions{
				CourseID:        req.GetString("course_id", ""),
				IncludeStudents: req.GetBool("include_students", false),
			})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, err := json.Marshal(courses)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("marshal: %v", err)), nil
			}
			return mcp.NewToolResultText(string(b)), nil
		},
	)
}

func registerServerInfoTool(s *server.MCPServer) {
	s.AddTool(
		mcp.NewTool("edookit_server_info",
			mcp.WithDescription("Return this edookit-mcp server's build metadata as JSON: "+
				"{version, commit, build_time}. Use ONLY when the user explicitly asks which "+
				"version is running or whether the server/connector is up to date — it is not "+
				"part of any normal message or attachment workflow. Takes no arguments. "+
				"Placeholder values (\"dev\"/\"none\"/\"unknown\") mean a local dev build, not a "+
				"released binary."),
		),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, err := json.Marshal(buildServerInfo())
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
	for _, w := range unread.ParseWarnings {
		log.Printf("  [parse-warning] %s", w)
	}
}

// runDumpMessage is a development-only helper used to reverse-engineer the
// full-message endpoint shape. It strips the optional "m-" prefix from the
// ID and walks a list of plausible URL patterns (derived from the SPA hash
// route #handler/window/message-edit?__index=N seen in inbox row HTML),
// printing whichever ones return JSON. Output goes to stdout (the JSON
// body) and stderr (probe progress) so the caller can redirect them
// independently. No retention — pure investigation aid.
// runGetMessage is a dev smoke target — calls tools.GetMessage with the
// given ID and prints the parsed FullMessage as indented JSON on stdout.
// Used to verify the parser end-to-end against live Edookit (the
// edookit_get_message MCP tool exposes the same call to clients).
func runGetMessage(cli *client.Client, id string) {
	ctx := context.Background()
	msg, err := tools.GetMessage(ctx, cli, id)
	if err != nil {
		log.Fatalf("get-message %s: %v", id, err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(msg); err != nil {
		log.Fatalf("encode: %v", err)
	}
}

func runDumpMessage(cli *client.Client, idArg string) {
	ctx := context.Background()
	id := strings.TrimPrefix(idArg, "m-")
	if id == "" {
		log.Fatalf("--dump-message: empty ID (use m-NNNNNN or NNNNNN)")
	}

	// Likely endpoint patterns. SPA hash routes follow #handler/window/<X>,
	// and the JSON layer typically lives at /handler/page/<X> (we already
	// know that from how list_inbox works: SPA #handler/window/objects-for-me
	// is served by /handler/page/objects-for-me). Try a handful of plausible
	// path forms in case Edookit uses singular vs plural or message vs mail.
	paths := []string{
		"/handler/page/message-edit?__index=" + id,
		"/handler/page/message-view?__index=" + id,
		"/handler/page/message?__index=" + id,
		"/handler/window/message-edit?__index=" + id,
		"/handler/page/mail-edit?__index=" + id,
		"/handler/page/object-view?__index=" + id,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	successes := 0
	for _, p := range paths {
		log.Printf("[probe] GET %s ...", p)
		var resp map[string]any
		if err := cli.GetJSON(ctx, p, &resp); err != nil {
			log.Printf("  -> ERR: %v", err)
			continue
		}
		log.Printf("  -> OK (%d top-level keys)", len(resp))
		fmt.Printf("\n===== response for %s =====\n", p)
		_ = enc.Encode(resp)
		successes++
	}

	if successes == 0 {
		log.Fatalf("all %d candidate endpoints failed — Edookit URL scheme may have moved", len(paths))
	}
	log.Printf("done — %d/%d endpoints returned JSON", successes, len(paths))
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
