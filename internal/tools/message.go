package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"

	"github.com/dsaiko/edookit-mcp/internal/client"
)

// FullMessage is what GetMessage returns: a single message with its full
// body and attachment list. Used by both the received and sent inboxes;
// Edookit serves both via the same endpoint and the parser produces the
// same shape for both. Compared to the row-level Message returned by
// ListInbox / ListSent, this carries the full body text (not just a
// ~200-char preview) and the attachment file list (not just a count).
type FullMessage struct {
	ID          string       `json:"id"`                  // "m-289862"
	Number      int          `json:"number"`              // 289862
	Subject     string       `json:"subject"`             // "NOVINKY: …"
	Status      string       `json:"status,omitempty"`    // "Publikováno", "Nepublikováno", …
	Author      string       `json:"author,omitempty"`    // sender (for received) or publisher (for sent — typically the user themselves)
	Date        string       `json:"date,omitempty"`      // RFC3339, school timezone
	BodyHTML    string       `json:"body_html,omitempty"` // original message body HTML (sanitized by Edookit's editor)
	BodyText    string       `json:"body_text,omitempty"` // plain-text rendering of BodyHTML (entities decoded, whitespace collapsed, line breaks preserved)
	Attachments []Attachment `json:"attachments"`
}

// Attachment is one file linked from a message. URL is fully qualified and
// can be GET'd by the same authenticated session that fetched the message;
// the file is delivered as the response body with a Content-Disposition
// filename matching Name.
type Attachment struct {
	ID   string `json:"id"`             // Edookit's internal id, e.g. "1@191968"
	Name string `json:"name"`           // original filename, e.g. "schedule.pdf"
	URL  string `json:"url"`            // absolute download URL
	Date string `json:"date,omitempty"` // RFC3339 upload timestamp if Edookit provided one
}

// GetMessage fetches and parses a single message by its row UID. Accepts
// either "m-NNNNNN" or "NNNNNN" form; the m- prefix is the same UID format
// ListInbox / ListSent return in Message.ID.
func GetMessage(ctx context.Context, cli *client.Client, idOrUID string) (*FullMessage, error) {
	num, err := normalizeMessageID(idOrUID)
	if err != nil {
		return nil, err
	}

	var raw messageEditResponse
	path := "/handler/page/message-edit?__index=" + strconv.Itoa(num)
	if err := cli.GetJSON(ctx, path, &raw); err != nil {
		return nil, fmt.Errorf("fetch message %s: %w", idOrUID, err)
	}

	return parseFullMessage(num, &raw, cli.Timezone())
}

// normalizeMessageID strips the optional "m-" prefix from a row UID and
// parses the remainder as a positive int. Returns the numeric ID Edookit's
// __index= query parameter expects.
func normalizeMessageID(s string) (int, error) {
	stripped := strings.TrimPrefix(strings.TrimSpace(s), "m-")
	n, err := strconv.Atoi(stripped)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid message id %q (expected m-NNNN or NNNN)", s)
	}
	return n, nil
}

// Workspace component DOMTargets we care about — Edookit reuses these
// identifiers for both received and sent messages and across multiple
// generations of the API, so they're stable enough to pin against.
const (
	domTargetFormMessage = "__lc_Form_Message"
	domTargetFileviewer  = "__lc_Fileviewer_Slave_datatemplate_message"
)

// messageEditResponse is the subset of /handler/page/message-edit?__index=N
// we care about. The wire shape has many more fields (form-state hidden
// inputs, organization metadata, dialog handlers, etc.) which we ignore.
//
// Layout discovered via reverse-engineering — see make smoke-message:
//   - components.workspace[*] is an array of UI panels
//   - the panel with DOMTarget="__lc_Form_Message" carries the message
//     fields under data.__form_panel_main[]
//   - each labeled sub-panel has items[] where items[0] holds the value;
//     we look up by items[].name ("name" = subject, "object_status" =
//     status html, "description__editor" = body)
//   - the panel with DOMTarget="__lc_Fileviewer_Slave_datatemplate_message"
//     carries attachments under data.data[]
type messageEditResponse struct {
	Authenticated *bool                 `json:"authenticated"`
	Components    messageEditComponents `json:"components"`
}

type messageEditComponents struct {
	Workspace []messageEditWorkspaceComponent `json:"workspace"`
}

type messageEditWorkspaceComponent struct {
	DOMTarget string                   `json:"DOMTarget"`
	Data      messageEditWorkspaceData `json:"data"`
}

// messageEditWorkspaceData has two shapes depending on which DOMTarget
// owns the component. The form-message panel carries __form_panel_main;
// the fileviewer panel carries data (a nested array of attachments).
// Both fields are pointers so a missing one decodes as nil instead of an
// empty slice we can't distinguish from "field absent".
type messageEditWorkspaceData struct {
	FormPanelMain []messageEditPanel      `json:"__form_panel_main,omitempty"`
	Data          []messageEditAttachment `json:"data,omitempty"`
}

type messageEditPanel struct {
	Label  string                 `json:"label,omitempty"`
	Hidden bool                   `json:"hidden,omitempty"`
	Items  []messageEditPanelItem `json:"items"`
}

type messageEditPanelItem struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Val       any    `json:"val,omitempty"`       // string for most types; can be other shapes for selectbox/multiselect, hence `any`
	ReadValue string `json:"readValue,omitempty"` // for simple_editor: the rendered HTML; .val is double-escaped (form-submit form)
}

type messageEditAttachment struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Link    string `json:"link"`
	Date    int64  `json:"date"` // unix seconds
	Trashed bool   `json:"trashed,omitempty"`
}

// parseFullMessage extracts the structured FullMessage from the raw JSON
// response. Returns an error if the workspace doesn't contain a recognizable
// message form (e.g. the API moved / the ID was for a different object kind).
func parseFullMessage(num int, raw *messageEditResponse, loc *time.Location) (*FullMessage, error) {
	if raw.Authenticated != nil && !*raw.Authenticated {
		// Should be caught by client.getJSON's retry path, but be defensive
		// in case a future Edookit response shape surfaces this here.
		return nil, errors.New("server reported authenticated=false")
	}

	var form *messageEditWorkspaceComponent
	var fileviewer *messageEditWorkspaceComponent
	for i := range raw.Components.Workspace {
		w := &raw.Components.Workspace[i]
		switch w.DOMTarget {
		case domTargetFormMessage:
			form = w
		case domTargetFileviewer:
			fileviewer = w
		}
	}
	if form == nil {
		return nil, fmt.Errorf("message-edit response has no __lc_Form_Message workspace component (got %d components)", len(raw.Components.Workspace))
	}

	msg := &FullMessage{
		ID:          fmt.Sprintf("m-%d", num),
		Number:      num,
		Attachments: []Attachment{}, // never null in JSON output, even when empty
	}

	subject, _ := findFormItem(form.Data.FormPanelMain, "name")
	if subject != nil {
		msg.Subject = strings.TrimSpace(asString(subject.Val))
	}

	statusItem, _ := findFormItem(form.Data.FormPanelMain, "object_status")
	if statusItem != nil {
		msg.Status, msg.Author, msg.Date = parseStatusHTML(asString(statusItem.Val), loc)
	}

	bodyItem, _ := findFormItem(form.Data.FormPanelMain, "description__editor")
	if bodyItem != nil {
		// Prefer readValue (single-escaped HTML, ready to render). The .val
		// field carries the form-submit form which double-escapes entities.
		msg.BodyHTML = bodyItem.ReadValue
		if msg.BodyHTML == "" {
			msg.BodyHTML = asString(bodyItem.Val)
		}
		msg.BodyText = htmlToText(msg.BodyHTML)
	}

	if fileviewer != nil {
		for _, a := range fileviewer.Data.Data {
			if a.Trashed {
				continue // server is hinting "this used to be attached, ignore"
			}
			msg.Attachments = append(msg.Attachments, Attachment{
				ID:   a.ID,
				Name: a.Name,
				URL:  a.Link,
				Date: unixToRFC3339(a.Date, loc),
			})
		}
	}

	return msg, nil
}

// findFormItem walks the labeled and unlabeled panels of __form_panel_main
// looking for the first item with the given name. Returns (nil, -1) if
// none. Used to locate "name", "object_status", "description__editor"
// reliably regardless of where Edookit decides to place them in the form
// layout.
func findFormItem(panels []messageEditPanel, itemName string) (item *messageEditPanelItem, panelIdx int) {
	for pi := range panels {
		for ii := range panels[pi].Items {
			if panels[pi].Items[ii].Name == itemName {
				return &panels[pi].Items[ii], pi
			}
		}
	}
	return nil, -1
}

// asString safely converts an Edookit field value to string. Some `val`
// fields are typed (e.g. multiselect arrays), so we accept any and only
// stringify the obvious scalar cases. Unknown types yield "" — caller
// decides whether that's an error.
func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		// JSON numbers always decode as float64 in any; preserve integer form
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	default:
		return ""
	}
}

// statusDateRe matches the date pattern Edookit embeds in the object_status
// HTML for received messages: "Od DD.MM.YYYY HH:MM" right after the status
// word. Sent messages don't carry the "Od …" inline date — for those we fall
// back to parsing the short date the bold-span carries ("Po DD.MM. HH:MM",
// which lacks a year). When neither is parseable, msg.Date stays empty and
// callers must use the date from the list response (which is precise).
var statusDateRe = regexp.MustCompile(`Od\s+(\d{1,2})\.(\d{1,2})\.(\d{4})\s+(\d{1,2}):(\d{1,2})`)

// parseStatusHTML extracts the three fields embedded in the Stav: HTML:
//
//	<span style="color:#...">Publikováno</span>
//	[ Od DD.MM.YYYY HH:MM ]                       ← received messages only
//	<span style="color:#...">, </span>
//	<span style="font-size:75%"><b>Author Name</b>, Po DD.MM. HH:MM</span>
//
// Returns (status, author, date_rfc3339). Any field that can't be parsed
// is returned empty — the caller leaves the corresponding FullMessage field
// unset rather than erroring out.
func parseStatusHTML(s string, loc *time.Location) (status, author, dateRFC string) {
	if s == "" {
		return "", "", ""
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + s + "</div>"))
	if err != nil {
		return "", "", ""
	}
	root := doc.Find("body > div").First()
	if root.Length() == 0 {
		return "", "", ""
	}

	// First top-level <span> = status word.
	if first := root.Find("span").First(); first.Length() > 0 {
		status = strings.TrimSpace(first.Text())
	}
	// Inside the last <span> the <b> tag holds the author.
	if b := root.Find("span b").First(); b.Length() > 0 {
		author = strings.Join(strings.Fields(b.Text()), " ")
	}
	// Inline "Od DD.MM.YYYY HH:MM" — present for received messages.
	if m := statusDateRe.FindStringSubmatch(root.Text()); len(m) == 6 {
		d, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		h, _ := strconv.Atoi(m[4])
		mi, _ := strconv.Atoi(m[5])
		dateRFC = time.Date(y, time.Month(mo), d, h, mi, 0, 0, loc).Format(time.RFC3339)
	}
	return status, author, dateRFC
}

// htmlToText renders the editor HTML to plain text suitable for inclusion in
// a JSON response: tags stripped, entities decoded, paragraph and <br>
// breaks preserved as newlines, surrounding whitespace trimmed. Goquery does
// the parsing; we walk the DOM rather than regex-stripping so entities and
// nested inline elements come out clean.
func htmlToText(rawHTML string) string {
	if rawHTML == "" {
		return ""
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + rawHTML + "</div>"))
	if err != nil {
		return ""
	}
	root := doc.Find("body > div").First()
	if root.Length() == 0 {
		return ""
	}

	var sb strings.Builder
	walkHTMLForText(root.Get(0), &sb)
	return collapseTextLines(sb.String())
}

// walkHTMLForText walks the DOM appending text content to sb, treating
// <br> and block-level tags (<p>, <div>, <li>) as newline boundaries so
// the caller can later collapse the result into clean lines.
func walkHTMLForText(n *html.Node, sb *strings.Builder) {
	if n == nil {
		return
	}
	switch n.Type {
	case html.ElementNode:
		appendElementText(n, sb)
	case html.TextNode:
		sb.WriteString(n.Data)
	default:
		walkChildrenForText(n, sb)
	}
}

// appendElementText handles the per-element cases (block tags emit
// newline boundaries; everything else is transparent). Kept separate from
// walkHTMLForText so the parent function stays under gocyclo's limit.
func appendElementText(n *html.Node, sb *strings.Builder) {
	switch n.Data {
	case "br":
		sb.WriteByte('\n')
		return
	case "p", "div", "li":
		if sb.Len() > 0 && !strings.HasSuffix(sb.String(), "\n") {
			sb.WriteByte('\n')
		}
		walkChildrenForText(n, sb)
		if !strings.HasSuffix(sb.String(), "\n") {
			sb.WriteByte('\n')
		}
		return
	}
	walkChildrenForText(n, sb)
}

func walkChildrenForText(n *html.Node, sb *strings.Builder) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkHTMLForText(c, sb)
	}
}

// collapseTextLines collapses internal whitespace on each line, dedupes
// consecutive blank lines (so <p><br/></p>-induced doubles don't appear)
// and trims trailing blanks. The leading "blank state" starts true so a
// stray blank line at the very top is suppressed too.
func collapseTextLines(raw string) string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	prevBlank := true
	for _, l := range lines {
		l = strings.TrimSpace(strings.Join(strings.Fields(l), " "))
		if l == "" {
			if prevBlank {
				continue
			}
			prevBlank = true
			out = append(out, "")
			continue
		}
		prevBlank = false
		out = append(out, l)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// unixToRFC3339 converts a unix timestamp (seconds) to an RFC3339 string in
// loc. Returns "" when ts is 0 (which is how Edookit represents "no date").
func unixToRFC3339(ts int64, loc *time.Location) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).In(loc).Format(time.RFC3339)
}
