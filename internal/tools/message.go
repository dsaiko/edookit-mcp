package tools

import (
	"bytes"
	"context"
	"encoding/json"
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
	Deleted     bool         `json:"deleted,omitempty"`   // true when Edookit reports the message as author-deleted (Status starts with "Smazané"); Subject + BodyHTML/Text are stripped server-side in that case, only Status / Author / Date carry info
	Attachments []Attachment `json:"attachments"`
	Recipients  []Recipient  `json:"recipients,omitempty"` // delivery / read-receipt table (Komunikace → Doručenky) — present on sent messages; for received messages typically lists only the current user
}

// Recipient is one row of the message's delivery / read-receipt table
// (Edookit calls it "Doručenky"). For sent messages this tells the
// author who received the message and whether they (and their parents)
// have opened it yet. For received messages the row usually just
// represents the current user.
type Recipient struct {
	Name          string   `json:"name"`                      // "Fajkus Eliáš"
	ReadAt        string   `json:"read_at,omitempty"`         // ISO date "2026-05-21" when the recipient first read the message; "" if not yet read
	Parents       []string `json:"parents,omitempty"`         // ["Fajkus Martin", "Fajkusová Soňa"]; empty for staff recipients (no parents listed)
	ParentsReadAt []string `json:"parents_read_at,omitempty"` // aligned with Parents; per parent either ISO date "2026-05-21" or "" if not read
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
	domTargetAcceptance  = "__lc_Grid_Acceptance"
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

// messageEditWorkspaceData carries the `data` field of a workspace
// component. The field has three observed shapes in the wild:
//
//   - form-message component: object with __form_panel_main + other
//     sub-panels we ignore. Populates FormPanelMain.
//   - fileviewer component: object with `data` (attachments array) +
//     other metadata we ignore. Populates Data.
//   - acceptance-grid component: bare JSON array of rows where each
//     row is itself an array of strings — Edookit's tabular shape for
//     the read-receipts table. Populates Acceptance.
//
// Custom UnmarshalJSON dispatches on the first byte: '{' decodes into
// the typed struct fields; '[' decodes into Acceptance directly; any
// other shape (null, scalar) is silently no-op'd so a future component
// we don't model can't break the whole parse.
//
// parseFullMessage looks up components by DOMTarget and only reads the
// field appropriate to that component, so the unused fields staying
// zero-valued is harmless.
type messageEditWorkspaceData struct {
	FormPanelMain []messageEditPanel      `json:"__form_panel_main,omitempty"`
	Data          []messageEditAttachment `json:"data,omitempty"`
	Acceptance    [][]string              `json:"-"`
}

func (d *messageEditWorkspaceData) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil
	}
	switch b[0] {
	case '{':
		// Alias type breaks the recursion into our own UnmarshalJSON.
		type alias messageEditWorkspaceData
		return json.Unmarshal(b, (*alias)(d))
	case '[':
		return json.Unmarshal(b, &d.Acceptance)
	default:
		// null / scalar / unexpected shape — workspace component we
		// don't model. Leave d zero-valued.
		return nil
	}
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

	form, fileviewer, acceptance := pickComponents(raw.Components.Workspace)
	if form == nil {
		return nil, fmt.Errorf("message-edit response has no __lc_Form_Message workspace component (got %d components)", len(raw.Components.Workspace))
	}

	msg := &FullMessage{
		ID:          fmt.Sprintf("m-%d", num),
		Number:      num,
		Attachments: []Attachment{}, // never null in JSON output, even when empty
	}
	populateMessageFields(msg, form.Data.FormPanelMain, loc)

	// Detect author-deleted messages: Edookit keeps the metadata (status,
	// author, date) but strips subject + body server-side, and the status
	// label changes to "Smazané autorem DD.MM.YYYY HH:MM". The schema-
	// drift guard below would otherwise misreport this as "form schema
	// drifted" — but it's a legitimate state we should surface.
	if strings.HasPrefix(msg.Status, "Smazané") {
		msg.Deleted = true
	}

	// Schema-drift guard: if Edookit renames or removes BOTH the
	// "name" / "description__editor" items AND object_status (i.e. the
	// whole form is empty of human-readable content), parseFullMessage
	// would otherwise return a successful FullMessage with nothing in
	// it. Loud failure here matches the row-parser's policy (see
	// fetchAndParse: rows-fetched-but-none-parsed becomes an error) so
	// genuine drift gets flagged as drift. Note: author-deletion leaves
	// status non-empty, so this won't false-positive on those.
	if msg.Status == "" && msg.Subject == "" && msg.BodyText == "" && msg.BodyHTML == "" {
		return nil, fmt.Errorf("message %d: parsed message has empty status AND subject AND body — Edookit form schema may have drifted (expected items 'object_status' / 'name' / 'description__editor' in __form_panel_main)", num)
	}

	if fileviewer != nil {
		msg.Attachments = collectAttachments(fileviewer.Data.Data, loc)
	}
	if acceptance != nil {
		msg.Recipients = collectRecipients(acceptance.Data.Acceptance)
	}

	return msg, nil
}

// pickComponents scans the workspace array once and returns the three
// components parseFullMessage knows how to extract. form is required;
// the others may legitimately be absent (a no-attachments message
// won't have fileviewer; a stripped-down API response may lack the
// acceptance grid). Order in the response is not guaranteed, hence
// the scan-and-classify.
func pickComponents(workspace []messageEditWorkspaceComponent) (form, fileviewer, acceptance *messageEditWorkspaceComponent) {
	for i := range workspace {
		w := &workspace[i]
		switch w.DOMTarget {
		case domTargetFormMessage:
			form = w
		case domTargetFileviewer:
			fileviewer = w
		case domTargetAcceptance:
			acceptance = w
		}
	}
	return form, fileviewer, acceptance
}

// populateMessageFields extracts subject / status / author / date / body
// from the form panels and assigns them onto msg. Items that don't appear
// leave the corresponding field at its zero value — the schema-drift
// guard in parseFullMessage decides whether that's tolerable.
func populateMessageFields(msg *FullMessage, panels []messageEditPanel, loc *time.Location) {
	if subject, _ := findFormItem(panels, "name"); subject != nil {
		msg.Subject = strings.TrimSpace(asString(subject.Val))
	}
	if statusItem, _ := findFormItem(panels, "object_status"); statusItem != nil {
		msg.Status, msg.Author, msg.Date = parseStatusHTML(asString(statusItem.Val), loc)
	}
	if bodyItem, _ := findFormItem(panels, "description__editor"); bodyItem != nil {
		// Prefer readValue (single-escaped HTML, ready to render). The .val
		// field carries the form-submit form which double-escapes entities.
		msg.BodyHTML = bodyItem.ReadValue
		if msg.BodyHTML == "" {
			msg.BodyHTML = asString(bodyItem.Val)
		}
		msg.BodyText = htmlToText(msg.BodyHTML)
	}
}

// collectAttachments filters out trashed entries and converts each
// remaining messageEditAttachment to the public Attachment shape (unix
// timestamp rendered as RFC3339 in loc).
func collectAttachments(raw []messageEditAttachment, loc *time.Location) []Attachment {
	out := make([]Attachment, 0, len(raw))
	for _, a := range raw {
		if a.Trashed {
			continue // server is hinting "this used to be attached, ignore"
		}
		out = append(out, Attachment{
			ID:   a.ID,
			Name: a.Name,
			URL:  a.Link,
			Date: unixToRFC3339(a.Date, loc),
		})
	}
	return out
}

// collectRecipients converts the raw Acceptance grid rows into the
// public Recipient shape. Edookit's row layout (observed from
// /handler/page/message-edit) is a 5-tuple:
//
//	[ id, person_name, first_seen, parents (br-joined), parents_first_seen (br-joined) ]
//
// Where first_seen / parents_first_seen are either "DD.MM.YYYY", an
// empty string, or "Ne" (Czech "no" — recipient/parent hasn't read).
// Parents and parents_first_seen are joined with "<br>" because
// Edookit's UI renders them as multi-line cells.
//
// Edookit collapses parents_first_seen to a single value when all
// parents share the status (a single "Ne" badge for two unread
// parents, vs. "Ne<br>21.05.2026" for one unread and one read). We
// normalize that by repeating the last value to match len(Parents) so
// consumers always get aligned arrays.
//
// Rows that don't have at least 5 cells are skipped (defensive — same
// approach as the row-list parser does for short rows).
func collectRecipients(rows [][]string) []Recipient {
	out := make([]Recipient, 0, len(rows))
	for _, row := range rows {
		if len(row) < 5 {
			continue
		}
		parents := splitBR(row[3])
		parentsRead := mapSlice(splitBR(row[4]), czechDateToISO)
		out = append(out, Recipient{
			Name:          strings.TrimSpace(row[1]),
			ReadAt:        czechDateToISO(row[2]),
			Parents:       parents,
			ParentsReadAt: alignParentsRead(parentsRead, len(parents)),
		})
	}
	return out
}

// alignParentsRead normalizes the parents-read indicator to match the
// number of parents. Edookit's wire format collapses a uniform "Ne /
// Ne" to a single "Ne", which would otherwise leave len(ParentsReadAt)
// < len(Parents) and confuse downstream consumers. When the indicator
// is empty (staff recipient with no parents listed) it stays empty.
// When it's longer than Parents (shouldn't happen, but be defensive)
// it's truncated.
func alignParentsRead(read []string, nParents int) []string {
	if nParents == 0 {
		return nil
	}
	switch {
	case len(read) == 0:
		// Edookit sent neither "Ne" nor a date for any parent — fill
		// with empties so the array length still matches Parents.
		out := make([]string, nParents)
		return out
	case len(read) < nParents:
		// Single (or fewer) status applies to all parents — replicate
		// the last value to fill in.
		out := make([]string, nParents)
		for i := range out {
			if i < len(read) {
				out[i] = read[i]
			} else {
				out[i] = read[len(read)-1]
			}
		}
		return out
	case len(read) > nParents:
		return read[:nParents]
	default:
		return read
	}
}

// czechDateToISO converts a Czech "DD.MM.YYYY" string to ISO "YYYY-MM-DD".
// Returns "" for an empty input, for "Ne" (Czech for "no", which Edookit
// uses to mean "not read"), or for anything that doesn't parse. The
// output is date-only because the Acceptance grid carries day-precision
// only — promoting it to RFC3339 by faking 00:00 would be misleading.
func czechDateToISO(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "Ne" {
		return ""
	}
	// Tolerate stray internal spaces ("21. 5. 2026") the way the previous
	// per-part trimming did, then parse strictly so impossible dates
	// ("00.05.2026", "32.13.2026") are rejected rather than normalized.
	t, err := time.Parse("2.1.2006", strings.ReplaceAll(s, " ", ""))
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// splitBR splits a "<br>"-joined cell into its components. Empty input
// returns an empty slice (nil) rather than [""] so JSON omitempty drops
// the field entirely for staff recipients with no parents listed.
func splitBR(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "<br>")
}

// mapSlice applies fn to every element of in. Used to transform parents'
// per-parent read indicators from Czech dates / "Ne" into ISO dates / "".
func mapSlice[T any](in []T, fn func(T) T) []T {
	if len(in) == 0 {
		return nil
	}
	out := make([]T, len(in))
	for i, v := range in {
		out[i] = fn(v)
	}
	return out
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
		if iso, ok := czechDateTimeToRFC3339(m[1], m[2], m[3], m[4], m[5], loc); ok {
			dateRFC = iso
		}
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

	r := &textRenderer{}
	r.walk(root.Get(0))
	return collapseTextLines(r.sb.String())
}

// textRenderer accumulates the plain-text rendering of a DOM tree. It
// tracks lastWasNewline alongside the builder so block-level tags can
// avoid emitting redundant newlines without re-stringing the buffer on
// every call (strings.Builder.String() is O(1) but the suffix check
// downstream still added noise — explicit state is clearer and harder
// to get subtly wrong as the renderer grows).
type textRenderer struct {
	sb             strings.Builder
	lastWasNewline bool
}

func (r *textRenderer) writeByte(b byte) {
	r.sb.WriteByte(b)
	r.lastWasNewline = b == '\n'
}

func (r *textRenderer) writeString(s string) {
	if s == "" {
		return
	}
	r.sb.WriteString(s)
	r.lastWasNewline = s[len(s)-1] == '\n'
}

// ensureNewline appends a '\n' unless we just wrote one or the buffer is
// empty (no point starting the output with a blank line).
func (r *textRenderer) ensureNewline() {
	if !r.lastWasNewline && r.sb.Len() > 0 {
		r.writeByte('\n')
	}
}

// walk renders the DOM rooted at n. Block-level tags (<p>, <div>, <li>)
// emit newline boundaries before and after their content; <br> emits one
// inline; everything else is transparent.
func (r *textRenderer) walk(n *html.Node) {
	if n == nil {
		return
	}
	switch n.Type {
	case html.ElementNode:
		r.appendElement(n)
	case html.TextNode:
		r.writeString(n.Data)
	default:
		r.walkChildren(n)
	}
}

func (r *textRenderer) appendElement(n *html.Node) {
	switch n.Data {
	case "br":
		r.writeByte('\n')
		return
	case "p", "div", "li":
		r.ensureNewline()
		r.walkChildren(n)
		r.ensureNewline()
		return
	}
	r.walkChildren(n)
}

func (r *textRenderer) walkChildren(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.walk(c)
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
