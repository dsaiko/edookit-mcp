package tools

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"

	"github.com/dsaiko/edookit-mcp/internal/client"
)

// Message is one row returned by ListInbox / ListSent. Sender is populated for
// inbox rows; Status is populated for sent rows ("Publikováno" etc.). Subjects
// and body text remain in their original Czech.
type Message struct {
	ID          string `json:"id"`     // e.g. "m-290491"
	Number      int    `json:"number"` // 290491
	Date        string `json:"date"`   // ISO 8601 local: "2026-05-21T12:31:00+02:00"
	Sender      string `json:"sender,omitempty"`
	Status      string `json:"status,omitempty"`
	Subject     string `json:"subject"`
	BodyPreview string `json:"body_preview,omitempty"`
	Attachments int    `json:"attachments"`
}

// ListResult is what ListInbox / ListSent return. Messages is the parsed
// rows; ParseWarnings records each row the server returned that we couldn't
// parse (typically schema drift in Edookit's row HTML). Surfacing the
// warnings instead of swallowing them lets the MCP caller — and through it,
// Claude and the user — distinguish "no messages match" from "the parser
// silently dropped everything". When every fetched row fails to parse the
// call returns an error instead of an empty ListResult.
type ListResult struct {
	Messages      []Message `json:"messages"`
	ParseWarnings []string  `json:"parse_warnings,omitempty"`
}

// InboxOptions controls ListInbox.
type InboxOptions struct {
	View     string // "inbox" (default) | "unread" | "starred" | "archived" | "all"
	Fulltext string // optional server-side fulltext filter
	Since    string // optional client-side date floor: "7d", "1w", "2m", "1y", or "YYYY-MM-DD"
	Limit    int    // default 50, capped at 200
}

// SentOptions controls ListSent.
type SentOptions struct {
	Fulltext string
	Since    string
	Limit    int
}

// Inbox view names — mirror the values Edookit's ?object_filter= accepts.
const (
	ViewInbox    = "inbox"
	ViewUnread   = "unread"
	ViewStarred  = "starred"
	ViewArchived = "archived"
	ViewAll      = "all"
)

const (
	defaultLimit = 50
	maxLimit     = 200
	pageSize     = 100 // server-fixed; ?onPage= is ignored
	maxPages     = 5   // safety cap (5 × 100 = 500 rows max)
)

var validInboxViews = map[string]bool{
	ViewInbox:    true,
	ViewUnread:   true,
	ViewStarred:  true,
	ViewArchived: true,
	ViewAll:      true,
}

// ListInbox fetches received messages (Komunikace → Přijaté).
func ListInbox(ctx context.Context, cli *client.Client, opts InboxOptions) (ListResult, error) {
	view := opts.View
	if view == "" {
		view = ViewInbox
	}
	if !validInboxViews[view] {
		return ListResult{}, fmt.Errorf("invalid view %q (want one of %s/%s/%s/%s/%s)",
			view, ViewInbox, ViewUnread, ViewStarred, ViewArchived, ViewAll)
	}

	q := url.Values{}
	q.Set("object_type_general", "object_type_message")
	q.Set("object_filter", view)
	if opts.Fulltext != "" {
		q.Set("fulltext", opts.Fulltext)
	}

	return fetchAndParse(ctx, cli, "/handler/grid/objects-for-me-data", q, opts.Since, normalizeLimit(opts.Limit), false)
}

// ListSent fetches messages the user has sent (Komunikace → Vytvořené).
func ListSent(ctx context.Context, cli *client.Client, opts SentOptions) (ListResult, error) {
	q := url.Values{}
	q.Set("object_type_general", "object_type_message")
	if opts.Fulltext != "" {
		q.Set("fulltext", opts.Fulltext)
	}
	return fetchAndParse(ctx, cli, "/handler/grid/created-objects-data", q, opts.Since, normalizeLimit(opts.Limit), true)
}

func normalizeLimit(n int) int {
	if n <= 0 {
		return defaultLimit
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}

// gridResponse is the subset of /handler/grid/* response we care about.
type gridResponse struct {
	Components struct {
		Workspace []struct {
			Data [][]string `json:"data"`
		} `json:"workspace"`
	} `json:"components"`
}

func fetchAndParse(ctx context.Context, cli *client.Client, path string, baseQuery url.Values, since string, limit int, isSent bool) (ListResult, error) {
	loc := cli.Timezone()
	sinceTime, err := parseSince(since, loc)
	if err != nil {
		return ListResult{}, fmt.Errorf("invalid since %q: %w", since, err)
	}

	result := ListResult{Messages: make([]Message, 0, limit)}
	var rowsFetched int

	for page := 1; page <= maxPages; page++ {
		q := cloneValues(baseQuery)
		q.Set("page", strconv.Itoa(page))

		var resp gridResponse
		if err := cli.GetJSON(ctx, path+"?"+q.Encode(), &resp); err != nil {
			return ListResult{}, fmt.Errorf("fetch page %d: %w", page, err)
		}
		if len(resp.Components.Workspace) == 0 {
			break
		}
		rows := resp.Components.Workspace[0].Data
		if len(rows) == 0 {
			break
		}
		rowsFetched += len(rows)

		for rowIdx, row := range rows {
			if len(row) < 3 {
				// rowIdx+1 is the 1-based position within the current page; that's
				// what shows up in Edookit's UI if you scroll through the grid.
				w := fmt.Sprintf("row %d on page %d has only %d cells (expected >=3)", rowIdx+1, page, len(row))
				log.Printf("[tools] skipping %s", w)
				result.ParseWarnings = append(result.ParseWarnings, w)
				continue
			}
			msg, perr := parseRow(row[0], row[2], isSent, loc)
			if perr != nil {
				// Surface schema drift both in logs AND in the result envelope
				// so a downstream caller (the MCP client → Claude → the user)
				// can tell "parser broke" from "mailbox empty".
				log.Printf("[tools] skipping malformed row: %v", perr)
				result.ParseWarnings = append(result.ParseWarnings, perr.Error())
				continue
			}
			if !sinceTime.IsZero() {
				if t, terr := time.Parse(time.RFC3339, msg.Date); terr == nil && t.Before(sinceTime) {
					return finalizeResult(result, rowsFetched)
				}
			}
			result.Messages = append(result.Messages, msg)
			if len(result.Messages) >= limit {
				return finalizeResult(result, rowsFetched)
			}
		}
		if len(rows) < pageSize {
			break // last page (partial)
		}
	}
	return finalizeResult(result, rowsFetched)
}

// finalizeResult is the last gate before fetchAndParse returns. If the server
// gave us rows but every one failed to parse, that's a hard failure (schema
// drift, server returning a totally different shape) — surface as an error
// rather than a silent empty mailbox. A partial loss (some parsed, some
// warned) still returns the partial Messages.
func finalizeResult(r ListResult, rowsFetched int) (ListResult, error) {
	if rowsFetched > 0 && len(r.Messages) == 0 && len(r.ParseWarnings) > 0 {
		return ListResult{}, fmt.Errorf("fetched %d row(s) but none parsed — schema may have drifted (first warning: %s)",
			rowsFetched, r.ParseWarnings[0])
	}
	return r, nil
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append(out[k], vs...)
	}
	return out
}

var czechDateRe = regexp.MustCompile(`(\d{1,2})\.(\d{1,2})\.(\d{4}) (\d{1,2}):(\d{1,2})`)

// relativeDateRe matches Edookit's relative day labels for recent rows: it
// renders "Dnes HH:MM" (today) / "Včera HH:MM" (yesterday) instead of an
// absolute DD.MM.YYYY date for messages from the last day or two. The day word
// sits in its own colored <span> inside the date <b>, e.g.
// `<b><span style="color:#77bb00">Dnes</span> 23:55</b>`.
var relativeDateRe = regexp.MustCompile(`(?i)(dnes|včera)\s+(\d{1,2}):(\d{1,2})`)

// parseRow extracts structured fields from one row of the grid response. The
// row's third cell is an HTML blob; the first cell is the UID. Required
// fields (date, subject, and sender-or-status depending on isSent) must all
// be present — if any are missing, parseRow returns an error naming the
// missing fields so callers can log and skip the row instead of silently
// emitting a malformed Message (which would also slip past the `since`
// filter, since its RFC3339 parse fails for empty Date and falls through).
//
// loc is the school's wall-clock timezone. Edookit row dates come without
// offset suffix ("21.05.2026 12:31"), so we have to anchor them to an
// explicit Location before formatting as RFC3339 — otherwise running the
// MCP on a host outside the school's TZ would emit the wrong UTC offsets.
func parseRow(uid, rowHTML string, isSent bool, loc *time.Location) (Message, error) {
	msg := Message{ID: uid}
	if uid == "" {
		return msg, errors.New("empty row UID")
	}
	if n, _ := strconv.Atoi(strings.TrimPrefix(uid, "m-")); n > 0 {
		msg.Number = n
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + rowHTML + "</div>"))
	if err != nil {
		return msg, fmt.Errorf("parse row html: %w", err)
	}
	small := doc.Find("small").First()

	var missing []string

	// Date: usually "DD.MM.YYYY HH:MM" somewhere in the <small> text, but for
	// messages from the last day or two Edookit renders a relative label
	// ("Dnes HH:MM" / "Včera HH:MM") instead — resolve those against the
	// school's wall-clock today.
	smallText := small.Text()
	if m := czechDateRe.FindStringSubmatch(smallText); len(m) == 6 {
		d, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		h, _ := strconv.Atoi(m[4])
		mi, _ := strconv.Atoi(m[5])
		msg.Date = time.Date(y, time.Month(mo), d, h, mi, 0, 0, loc).Format(time.RFC3339)
	} else if rm := relativeDateRe.FindStringSubmatch(smallText); len(rm) == 4 {
		h, _ := strconv.Atoi(rm[2])
		mi, _ := strconv.Atoi(rm[3])
		day := time.Now().In(loc)
		if strings.EqualFold(rm[1], "včera") {
			day = day.AddDate(0, 0, -1)
		}
		msg.Date = time.Date(day.Year(), day.Month(), day.Day(), h, mi, 0, 0, loc).Format(time.RFC3339)
	} else {
		missing = append(missing, "date")
	}

	// First direct-child span of <small> is either the sender (inbox) or the
	// status (sent). We use Children, not Find, because relative-date rows nest
	// a "Dnes"/"Včera" <span> inside the date <b> — a descendant span that
	// Find would grab first, clobbering the real sender/status.
	if first := strings.TrimSpace(small.Children().Filter("span").First().Text()); first != "" {
		// Sender lines have collapsed whitespace ("Eva (KAL)  (učitel 4SC)") —
		// normalize to single spaces for nicer downstream output.
		first = strings.Join(strings.Fields(first), " ")
		if isSent {
			msg.Status = first
		} else {
			msg.Sender = first
		}
	} else if isSent {
		missing = append(missing, "status")
	} else {
		missing = append(missing, "sender")
	}

	// Subject is the <b> inside one of the row's main-area <a> tags.
	doc.Find("div a").EachWithBreak(func(_ int, a *goquery.Selection) bool {
		if b := a.Find("b").First(); b.Length() > 0 {
			if t := strings.TrimSpace(b.Text()); t != "" {
				msg.Subject = t
				return false
			}
		}
		return true
	})
	if msg.Subject == "" {
		missing = append(missing, "subject")
	}

	// Attachment count: text "Přílohy" followed by "<b>(N)</b>" nearby.
	msg.Attachments = parseAttachmentCount(doc)

	// Body preview: text node between the subject </div> and the first <br>.
	msg.BodyPreview = parseBodyPreview(doc)

	if len(missing) > 0 {
		return msg, fmt.Errorf("row %s missing required field(s): %s", uid, strings.Join(missing, ", "))
	}
	return msg, nil
}

// parseAttachmentCount finds "Přílohy" followed by "(N)" in the row DOM.
// Returns 0 if no attachments span is present.
func parseAttachmentCount(doc *goquery.Document) int {
	var count int
	doc.Find("span").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if strings.TrimSpace(s.Text()) != "Přílohy" {
			return true
		}
		// Walk forward to find the next <b> whose text matches "(N)".
		s.NextAll().Find("b").EachWithBreak(func(_ int, b *goquery.Selection) bool {
			t := strings.TrimSpace(b.Text())
			if strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
				if n, err := strconv.Atoi(t[1 : len(t)-1]); err == nil {
					count = n
					return false
				}
			}
			return true
		})
		return false
	})
	return count
}

const bodyPreviewMaxRunes = 200

// parseBodyPreview extracts the body preview text from a row's parsed DOM.
// The preview lives as content between the subject <div> and the first <br>
// separating it from the action toolbar. Walks the parsed DOM rather than
// regex-matching: this preserves inline tags (<a>, <i>, <b>) in the body,
// decodes HTML entities natively via the html parser, and truncates by rune
// count so multi-byte UTF-8 (Czech diacritics) doesn't break mid-character.
//
// Takes the *goquery.Document parseRow already built rather than reparsing
// the raw HTML — with limit=200 and pagination, reparsing per row was
// doubling the HTML parse cost.
func parseBodyPreview(doc *goquery.Document) string {
	root := doc.Find("body > div").First()
	if root.Length() == 0 {
		return ""
	}

	var (
		sb            strings.Builder
		sawSubjectDiv bool
		done          bool
	)
	root.Contents().EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if done {
			return false
		}
		n := s.Get(0)
		if n == nil {
			return true
		}

		switch n.Type {
		case html.ElementNode:
			switch n.Data {
			case "div":
				// The first top-level <div> is the subject + attachments
				// container; everything before it is metadata (the <small>
				// with date/sender) and everything after it up to the first
				// <br> is the body preview.
				if !sawSubjectDiv {
					sawSubjectDiv = true
					return true
				}
				// A subsequent <div> (action toolbar, cleaner) — body ended.
				done = true
				return false
			case "br":
				if sawSubjectDiv {
					done = true
					return false
				}
			default:
				// Inline element (a, b, i, span, …) inside the body region.
				if sawSubjectDiv {
					sb.WriteString(s.Text())
				}
			}
		case html.TextNode:
			if sawSubjectDiv {
				sb.WriteString(n.Data)
			}
		}
		return true
	})

	// strings.Fields splits on any Unicode whitespace including U+00A0 (NBSP
	// produced by the html parser from &nbsp;), so we never need to special-case
	// HTML entities ourselves.
	body := strings.Join(strings.Fields(sb.String()), " ")
	return truncateRunes(body, bodyPreviewMaxRunes)
}

// truncateRunes returns s truncated to at most maxRunes runes, suffixed with
// an ellipsis if anything was cut. Counting in runes (not bytes) keeps
// multi-byte UTF-8 sequences from being sliced mid-character.
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// parseSince accepts "7d", "1w", "2m", "1y", or an ISO date (YYYY-MM-DD or
// RFC3339). Empty string means "no floor".
//
// For bare YYYY-MM-DD the date is interpreted in loc (the school's TZ) so
// "2026-05-01" lines up with how Edookit would render that wall-clock day.
// For RFC3339 the input's own offset is preserved (e.g. "...Z" stays UTC).
// Relative durations (7d/1w/2m/1y) are computed off time.Now() in the host's
// TZ — semantically "exactly N days ago to the minute". All three cases
// produce instants and the downstream time.Before/After comparison is
// instant-based, so the result is correct regardless of which TZ each side
// carries.
func parseSince(s string, loc *time.Location) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if len(s) >= 2 {
		unit := s[len(s)-1:]
		if val, err := strconv.Atoi(s[:len(s)-1]); err == nil && val > 0 {
			now := time.Now()
			switch unit {
			case "d":
				return now.AddDate(0, 0, -val), nil
			case "w":
				return now.AddDate(0, 0, -val*7), nil
			case "m":
				return now.AddDate(0, -val, 0), nil
			case "y":
				return now.AddDate(-val, 0, 0), nil
			}
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("expected '7d', '1w', '2m', '1y', or ISO date (YYYY-MM-DD / RFC3339)")
}
