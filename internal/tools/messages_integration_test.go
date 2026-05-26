package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dsaiko/edookit-mcp/internal/client"
)

// gridFixture is the minimal JSON shape our parser reads. We pre-build response
// bodies in this shape and let the test server return them per path/query.
type gridFixture struct {
	Components struct {
		Workspace []struct {
			Data [][]string `json:"data"`
		} `json:"workspace"`
	} `json:"components"`
}

func buildRow(uid, dateCzech, sender, subject, body string, attachments int) []string {
	html := `<small><b>` + dateCzech + `</b>, <span style="color:#212121;font-weight:bold">` + sender + `</span></small>` +
		`<div><a href="x"><span class="ico50 menu_icon"></span></a>` +
		`<a href="x"><b>` + subject + `</b></a>`
	if attachments > 0 {
		html += `<span><span>Přílohy</span></span> <span><span><b>(` + strconv.Itoa(attachments) + `)</b></span></span>`
	}
	html += `</div>` + body + `<br>`
	return []string{uid, uid, html}
}

func buildSentRow(uid, dateCzech, status, subject, body string, attachments int) []string {
	html := `<small><span style="color:#77bb00">` + status + `</span>, <b>` + dateCzech + `</b></small>` +
		`<div><a href="x"><span class="ico50 menu_icon"></span></a>` +
		`<a href="x"><b>` + subject + `</b></a>`
	if attachments > 0 {
		html += `<span><span>Přílohy</span></span> <span><span><b>(` + strconv.Itoa(attachments) + `)</b></span></span>`
	}
	html += `</div>` + body + `<br>`
	return []string{uid, uid, html}
}

// fakeServer returns an httptest.Server that satisfies the bits of the Edookit
// surface this package touches: GET / (warmup) and GET /handler/grid/* (data).
// `handler` is invoked for grid requests and is expected to return the rows to
// serve for that particular request.
func fakeServer(t *testing.T, handler func(t *testing.T, r *http.Request) [][]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><body>warmup ok</body></html>`))
	})
	mux.HandleFunc("/handler/grid/", func(w http.ResponseWriter, r *http.Request) {
		rows := handler(t, r)
		fix := gridFixture{}
		fix.Components.Workspace = append(fix.Components.Workspace, struct {
			Data [][]string `json:"data"`
		}{Data: rows})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fix)
	})
	return httptest.NewServer(mux)
}

// newTestClient builds a Client pointed at srv with a pre-populated cookie jar
// so ensureLoggedIn takes the cached-cookies path (warmup only, no chromedp).
func newTestClient(t *testing.T, srv *httptest.Server) *client.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	u, _ := url.Parse(srv.URL)
	jar.SetCookies(u, []*http.Cookie{{
		Name:  "PHPSESSID",
		Value: "test-session",
		Path:  "/",
	}})

	cli, err := client.New(client.Config{
		BaseURL:    srv.URL,
		Username:   "u",
		Password:   "p",
		HTTPClient: &http.Client{Jar: jar, Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return cli
}

func TestListInbox_BasicHappyPath(t *testing.T) {
	t.Parallel()

	srv := fakeServer(t, func(t *testing.T, r *http.Request) [][]string {
		t.Helper()
		// Verify the request URL is exactly what we expect.
		if !strings.HasPrefix(r.URL.Path, "/handler/grid/objects-for-me-data") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		q := r.URL.Query()
		if got := q.Get("object_type_general"); got != "object_type_message" {
			t.Errorf("object_type_general = %q", got)
		}
		if got := q.Get("object_filter"); got != "inbox" {
			t.Errorf("object_filter = %q, want 'inbox' (default)", got)
		}
		if got := q.Get("page"); got != "1" {
			t.Errorf("page = %q, want '1'", got)
		}
		return [][]string{
			buildRow("m-100", "20.05.2026 10:00", "Alice (ALI)", "Subject A", "Body A", 0),
			buildRow("m-99", "19.05.2026 09:00", "Bob (BOB)", "Subject B", "Body B", 2),
		}
	})
	defer srv.Close()

	cli := newTestClient(t, srv)
	msgs, err := ListInbox(context.Background(), cli, InboxOptions{})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(msgs.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs.Messages))
	}
	if msgs.Messages[0].ID != "m-100" || msgs.Messages[0].Subject != "Subject A" {
		t.Errorf("msgs.Messages[0] = %+v", msgs.Messages[0])
	}
	if msgs.Messages[1].Attachments != 2 {
		t.Errorf("msgs.Messages[1].Attachments = %d, want 2", msgs.Messages[1].Attachments)
	}
}

func TestListInbox_InvalidViewReturnsError(t *testing.T) {
	t.Parallel()

	srv := fakeServer(t, func(t *testing.T, _ *http.Request) [][]string {
		t.Helper()
		t.Errorf("server should not be hit when view is invalid")
		return nil
	})
	defer srv.Close()

	cli := newTestClient(t, srv)
	_, err := ListInbox(context.Background(), cli, InboxOptions{View: "garbage"})
	if err == nil {
		t.Fatal("expected error for invalid view, got nil")
	}
}

func TestListInbox_ViewIsPropagated(t *testing.T) {
	t.Parallel()

	for _, view := range []string{"inbox", "unread", "starred", "archived", "all"} {
		t.Run(view, func(t *testing.T) {
			t.Parallel()
			srv := fakeServer(t, func(t *testing.T, r *http.Request) [][]string {
				t.Helper()
				if got := r.URL.Query().Get("object_filter"); got != view {
					t.Errorf("object_filter = %q, want %q", got, view)
				}
				return nil
			})
			defer srv.Close()
			cli := newTestClient(t, srv)
			if _, err := ListInbox(context.Background(), cli, InboxOptions{View: view}); err != nil {
				t.Fatalf("ListInbox: %v", err)
			}
		})
	}
}

func TestListInbox_FulltextIsPropagated(t *testing.T) {
	t.Parallel()

	srv := fakeServer(t, func(t *testing.T, r *http.Request) [][]string {
		t.Helper()
		if got := r.URL.Query().Get("fulltext"); got != "ročníkov" {
			t.Errorf("fulltext = %q, want 'ročníkov'", got)
		}
		return nil
	})
	defer srv.Close()
	cli := newTestClient(t, srv)
	if _, err := ListInbox(context.Background(), cli, InboxOptions{Fulltext: "ročníkov"}); err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
}

func TestListInbox_LimitRespected(t *testing.T) {
	t.Parallel()

	srv := fakeServer(t, func(_ *testing.T, _ *http.Request) [][]string {
		rows := make([][]string, 100)
		for i := range rows {
			rows[i] = buildRow(
				fmt.Sprintf("m-%d", 1000-i),
				"19.05.2026 12:00",
				"Sender", fmt.Sprintf("Subj %d", i), "Body", 0,
			)
		}
		return rows
	})
	defer srv.Close()

	cli := newTestClient(t, srv)
	msgs, err := ListInbox(context.Background(), cli, InboxOptions{Limit: 7})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(msgs.Messages) != 7 {
		t.Errorf("got %d messages, want 7", len(msgs.Messages))
	}
}

func TestListInbox_PaginatesWhenLimitExceedsPageSize(t *testing.T) {
	t.Parallel()

	var pagesHit []int
	srv := fakeServer(t, func(t *testing.T, r *http.Request) [][]string {
		t.Helper()
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pagesHit = append(pagesHit, p)
		// Page 1 returns 100 rows; page 2 returns 50; page 3+ returns 0.
		switch p {
		case 1:
			rows := make([][]string, 100)
			for i := range rows {
				rows[i] = buildRow(fmt.Sprintf("m-1-%d", i), "19.05.2026 12:00", "S", "Subj", "B", 0)
			}
			return rows
		case 2:
			rows := make([][]string, 50)
			for i := range rows {
				rows[i] = buildRow(fmt.Sprintf("m-2-%d", i), "18.05.2026 12:00", "S", "Subj", "B", 0)
			}
			return rows
		default:
			return nil
		}
	})
	defer srv.Close()

	cli := newTestClient(t, srv)
	msgs, err := ListInbox(context.Background(), cli, InboxOptions{Limit: 130})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(msgs.Messages) != 130 {
		t.Errorf("got %d messages, want 130", len(msgs.Messages))
	}
	// Should have hit pages 1 and 2 only (page 2 returns 50 < pageSize=100 → last page).
	if len(pagesHit) != 2 || pagesHit[0] != 1 || pagesHit[1] != 2 {
		t.Errorf("pagesHit = %v, want [1 2]", pagesHit)
	}
}

func TestListInbox_SinceStopsAtBoundary(t *testing.T) {
	t.Parallel()

	srv := fakeServer(t, func(_ *testing.T, _ *http.Request) [][]string {
		return [][]string{
			buildRow("m-3", "21.05.2026 10:00", "S", "Recent", "B", 0),
			buildRow("m-2", "15.05.2026 10:00", "S", "Recent", "B", 0),
			// Older than the floor — must be excluded from the result.
			buildRow("m-1", "01.01.2020 00:00", "S", "Old", "B", 0),
			buildRow("m-0", "01.01.2019 00:00", "S", "Even older", "B", 0),
		}
	})
	defer srv.Close()

	cli := newTestClient(t, srv)
	msgs, err := ListInbox(context.Background(), cli, InboxOptions{Since: "2026-05-10"})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(msgs.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (boundary should stop at m-1)", len(msgs.Messages))
	}
	for _, m := range msgs.Messages {
		if m.Subject == "Old" || m.Subject == "Even older" {
			t.Errorf("unexpected old message included: %q", m.Subject)
		}
	}
}

// since must skip rows older than the floor without stopping the scan: if the
// server interleaves a newer row after an older one (full-text results aren't
// guaranteed newest-first), the newer match must still be returned.
func TestListInbox_SinceSkipsOldButKeepsLaterNewRows(t *testing.T) {
	t.Parallel()

	srv := fakeServer(t, func(_ *testing.T, _ *http.Request) [][]string {
		return [][]string{
			buildRow("m-3", "21.05.2026 10:00", "S", "Recent A", "B", 0),
			// Older than the floor, positioned BEFORE a newer row.
			buildRow("m-1", "01.01.2020 00:00", "S", "Old", "B", 0),
			buildRow("m-2", "18.05.2026 10:00", "S", "Recent B", "B", 0),
		}
	})
	defer srv.Close()

	cli := newTestClient(t, srv)
	msgs, err := ListInbox(context.Background(), cli, InboxOptions{Since: "2026-05-10"})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(msgs.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (both recent rows, old one skipped)", len(msgs.Messages))
	}
	for _, m := range msgs.Messages {
		if m.Subject == "Old" {
			t.Errorf("old row should have been skipped, but was included")
		}
	}
	if msgs.Messages[1].Subject != "Recent B" {
		t.Errorf("the newer row after the old one was dropped: got %q", msgs.Messages[1].Subject)
	}
}

// A row whose date matches the digit pattern but is not a real calendar
// instant ("32.13.2026 25:99") must be rejected end-to-end: the good row is
// returned, the impossible one lands in ParseWarnings (not silently
// normalized into a wrong timestamp).
func TestListInbox_InvalidDateRowWarned(t *testing.T) {
	t.Parallel()

	srv := fakeServer(t, func(_ *testing.T, _ *http.Request) [][]string {
		return [][]string{
			buildRow("m-2", "20.05.2026 10:00", "Alice", "Good", "Body", 0),
			buildRow("m-1", "32.13.2026 25:99", "Bob", "Impossible date", "Body", 0),
		}
	})
	defer srv.Close()

	cli := newTestClient(t, srv)
	msgs, err := ListInbox(context.Background(), cli, InboxOptions{})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(msgs.Messages) != 1 || msgs.Messages[0].Subject != "Good" {
		t.Fatalf("got %d messages (%v), want just the good one", len(msgs.Messages), msgs.Messages)
	}
	if len(msgs.ParseWarnings) != 1 || !strings.Contains(msgs.ParseWarnings[0], "date") {
		t.Fatalf("ParseWarnings = %v, want one mentioning the missing date", msgs.ParseWarnings)
	}
}

func TestListInbox_EmptyResponse(t *testing.T) {
	t.Parallel()

	srv := fakeServer(t, func(_ *testing.T, _ *http.Request) [][]string {
		return [][]string{}
	})
	defer srv.Close()

	cli := newTestClient(t, srv)
	msgs, err := ListInbox(context.Background(), cli, InboxOptions{})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(msgs.Messages) != 0 {
		t.Errorf("got %d messages, want 0", len(msgs.Messages))
	}
	if len(msgs.ParseWarnings) != 0 {
		t.Errorf("got %d warnings, want 0 (server returned no rows, not malformed ones)", len(msgs.ParseWarnings))
	}
}

// Mixed-fate page: most rows parse, one is malformed. Expect Messages to
// contain the good ones and ParseWarnings to record the bad one — no error.
func TestListInbox_PartialFailureSurfacedAsWarnings(t *testing.T) {
	t.Parallel()

	srv := fakeServer(t, func(_ *testing.T, _ *http.Request) [][]string {
		return [][]string{
			buildRow("m-1", "20.05.2026 10:00", "Alice", "Subj A", "Body", 0),
			// Missing subject — parseRow returns an error → row goes into ParseWarnings.
			{"m-2", "m-2", `<small><b>20.05.2026 10:00</b>, <span>Bob</span></small><div></div>Body<br>`},
			buildRow("m-3", "19.05.2026 09:00", "Carol", "Subj C", "Body", 0),
		}
	})
	defer srv.Close()

	cli := newTestClient(t, srv)
	msgs, err := ListInbox(context.Background(), cli, InboxOptions{})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(msgs.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (the two that parsed)", len(msgs.Messages))
	}
	if len(msgs.ParseWarnings) != 1 {
		t.Fatalf("got %d warnings, want 1 (the one malformed row)", len(msgs.ParseWarnings))
	}
	if !strings.Contains(msgs.ParseWarnings[0], "m-2") || !strings.Contains(msgs.ParseWarnings[0], "subject") {
		t.Errorf("warning %q should reference the bad row UID and the missing field", msgs.ParseWarnings[0])
	}
}

// Total failure: rows fetched but EVERY one fails to parse. That's
// undistinguishable from "the parser is broken", so fetchAndParse returns
// an error rather than a silent empty result.
func TestListInbox_TotalParseFailureReturnsError(t *testing.T) {
	t.Parallel()

	srv := fakeServer(t, func(_ *testing.T, _ *http.Request) [][]string {
		return [][]string{
			{"m-1", "m-1", `<small></small><div></div><br>`}, // missing all required fields
			{"m-2", "m-2", `<small></small><div></div><br>`},
		}
	})
	defer srv.Close()

	cli := newTestClient(t, srv)
	_, err := ListInbox(context.Background(), cli, InboxOptions{})
	if err == nil {
		t.Fatal("expected error when all rows fail to parse, got nil")
	}
	if !strings.Contains(err.Error(), "schema may have drifted") {
		t.Errorf("error %q should mention schema drift", err.Error())
	}
}

func TestListSent_BasicHappyPath(t *testing.T) {
	t.Parallel()

	srv := fakeServer(t, func(t *testing.T, r *http.Request) [][]string {
		t.Helper()
		if !strings.HasPrefix(r.URL.Path, "/handler/grid/created-objects-data") {
			t.Errorf("unexpected path %q (want /handler/grid/created-objects-data)", r.URL.Path)
		}
		// Sent endpoint must NOT carry object_filter — that's an inbox-only param.
		if got := r.URL.Query().Get("object_filter"); got != "" {
			t.Errorf("object_filter should be empty for sent, got %q", got)
		}
		return [][]string{
			buildSentRow("m-500", "21.05.2026 08:00", "Publikováno", "DIGI Den", "Body", 1),
			buildSentRow("m-499", "20.05.2026 09:00", "Nepublikováno", "Draft", "Body", 0),
		}
	})
	defer srv.Close()

	cli := newTestClient(t, srv)
	msgs, err := ListSent(context.Background(), cli, SentOptions{})
	if err != nil {
		t.Fatalf("ListSent: %v", err)
	}
	if len(msgs.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs.Messages))
	}
	if msgs.Messages[0].Status != "Publikováno" || msgs.Messages[0].Sender != "" {
		t.Errorf("msgs.Messages[0] = %+v (status should be set, sender empty)", msgs.Messages[0])
	}
	if msgs.Messages[1].Status != "Nepublikováno" {
		t.Errorf("msgs.Messages[1].Status = %q", msgs.Messages[1].Status)
	}
	if msgs.Messages[0].Attachments != 1 {
		t.Errorf("msgs.Messages[0].Attachments = %d, want 1", msgs.Messages[0].Attachments)
	}
}
