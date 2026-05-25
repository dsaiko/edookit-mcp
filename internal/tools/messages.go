package tools

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

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
func ListInbox(ctx context.Context, cli *client.Client, opts InboxOptions) ([]Message, error) {
	view := opts.View
	if view == "" {
		view = ViewInbox
	}
	if !validInboxViews[view] {
		return nil, fmt.Errorf("invalid view %q (want one of %s/%s/%s/%s/%s)",
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
func ListSent(ctx context.Context, cli *client.Client, opts SentOptions) ([]Message, error) {
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

func fetchAndParse(ctx context.Context, cli *client.Client, path string, baseQuery url.Values, since string, limit int, isSent bool) ([]Message, error) {
	sinceTime, err := parseSince(since)
	if err != nil {
		return nil, fmt.Errorf("invalid since %q: %w", since, err)
	}

	results := make([]Message, 0, limit)

	for page := 1; page <= maxPages; page++ {
		q := cloneValues(baseQuery)
		q.Set("page", strconv.Itoa(page))

		var resp gridResponse
		if err := cli.GetJSON(ctx, path+"?"+q.Encode(), &resp); err != nil {
			return nil, fmt.Errorf("fetch page %d: %w", page, err)
		}
		if len(resp.Components.Workspace) == 0 {
			break
		}
		rows := resp.Components.Workspace[0].Data
		if len(rows) == 0 {
			break
		}

		for _, row := range rows {
			if len(row) < 3 {
				continue
			}
			msg, perr := parseRow(row[0], row[2], isSent)
			if perr != nil {
				continue // skip malformed rows rather than failing the whole list
			}
			if !sinceTime.IsZero() {
				if t, terr := time.Parse(time.RFC3339, msg.Date); terr == nil && t.Before(sinceTime) {
					return results, nil // hit the date floor; stop
				}
			}
			results = append(results, msg)
			if len(results) >= limit {
				return results, nil
			}
		}
		if len(rows) < pageSize {
			break // last page (partial)
		}
	}
	return results, nil
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append(out[k], vs...)
	}
	return out
}

var czechDateRe = regexp.MustCompile(`(\d{1,2})\.(\d{1,2})\.(\d{4}) (\d{1,2}):(\d{1,2})`)

// parseRow extracts structured fields from one row of the grid response.
// The row's third cell is an HTML blob; the first cell is the UID.
func parseRow(uid, rowHTML string, isSent bool) (Message, error) {
	msg := Message{ID: uid}
	if n, _ := strconv.Atoi(strings.TrimPrefix(uid, "m-")); n > 0 {
		msg.Number = n
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + rowHTML + "</div>"))
	if err != nil {
		return msg, fmt.Errorf("parse row html: %w", err)
	}
	small := doc.Find("small").First()

	// Date: DD.MM.YYYY HH:MM somewhere in the <small> text.
	if m := czechDateRe.FindStringSubmatch(small.Text()); len(m) == 6 {
		d, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		h, _ := strconv.Atoi(m[4])
		mi, _ := strconv.Atoi(m[5])
		msg.Date = time.Date(y, time.Month(mo), d, h, mi, 0, 0, time.Local).Format(time.RFC3339)
	}

	// First span in <small> is either the sender (inbox) or the status (sent).
	if first := strings.TrimSpace(small.Find("span").First().Text()); first != "" {
		// Sender lines have collapsed whitespace ("Eva (KAL)  (učitel 4SC)") —
		// normalize to single spaces for nicer downstream output.
		first = strings.Join(strings.Fields(first), " ")
		if isSent {
			msg.Status = first
		} else {
			msg.Sender = first
		}
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

	// Attachment count: text "Přílohy" followed by "<b>(N)</b>" nearby.
	msg.Attachments = parseAttachmentCount(doc)

	// Body preview: text node between the subject </div> and the first <br>.
	msg.BodyPreview = parseBodyPreview(rowHTML)

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

var bodyPreviewRe = regexp.MustCompile(`(?s)</div>([^<]+)<br>`)

// parseBodyPreview extracts the body preview text (truncated to ~200 chars)
// from the row HTML. The preview sits as a bare text node between the subject
// </div> and the first <br> separating it from the action toolbar.
func parseBodyPreview(rowHTML string) string {
	m := bodyPreviewRe.FindStringSubmatch(rowHTML)
	if len(m) != 2 {
		return ""
	}
	body := strings.ReplaceAll(m[1], "&nbsp;", " ")
	body = strings.Join(strings.Fields(body), " ")
	const previewMax = 200
	if len(body) > previewMax {
		// Cut at a rune boundary to avoid invalid UTF-8 mid-sequence.
		cut := previewMax
		for cut > 0 && (body[cut]&0xC0) == 0x80 {
			cut--
		}
		body = body[:cut] + "…"
	}
	return body
}

// parseSince accepts "7d", "1w", "2m", "1y", or an ISO date (YYYY-MM-DD or RFC3339).
// Empty string means "no floor". Returned time is in local TZ.
func parseSince(s string) (time.Time, error) {
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
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("expected '7d', '1w', '2m', '1y', or ISO date (YYYY-MM-DD / RFC3339)")
}
