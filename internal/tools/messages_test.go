package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Sample row HTML captured live from /handler/grid/objects-for-me-data on
// 2026-05-25. Trimmed to keep the test file readable but structurally
// identical to what the server actually returns.
const inboxRowKaliskovaHTML = `<small style="color:#888;float:right;text-align:right">zpráva č. 290491, <b>21.05.2026 12:31</b>, <span style="color:#212121;font-weight:bold">Kalíšková Eva (KAL)  (učitel 4SC)</span><br></small>` +
	`<div><a href="https://test.example/#handler/window/message-edit?__index=290491" class="ajax"><span class="ico50 menu_icon"></span></a>` +
	`<a href="https://test.example/#handler/window/message-edit?__index=290491" class="ajax"><span style="color:#212121"><b>Obhajoba ročníkových prací 4SC</b></span></a>` +
	`<span style="font-size:12px;vertical-align:50%"></span></div>` +
	`Ahoj Dušane,  ve schránce ve sborovně jsem ti nechala rozpis obhajob ročníkovek třídy 4SC na úterý 26.5. s časem.  Dík  E. Kal.  <br>` +
	`<div class="cleaner">&nbsp;</div>`

// Row with 9 attachments (the NOVINKY message from the screenshot).
const inboxRowNovinkyHTML = `<small style="color:#888;float:right;text-align:right">zpráva č. 289862, <b>19.05.2026 6:24</b>, <span style="color:#212121;font-weight:bold">Odborný konzultant Edookit</span><br></small>` +
	`<div><a href="..." class="ajax"><span class="ico50 menu_icon"></span></a>` +
	`<a href="..." class="ajax"><span style="color:#212121"><b>NOVINKY: Zobrazení zkušebních termínů v rozvrhu h…</b></span></a>` +
	`<span style="font-size:12px;padding-left:10px;vertical-align:50%"><span style="color:#888">Přílohy</span></span>` +
	` <span style="vertical-align:50%"><span style="color:#0c9ce1"><b>(9)</b></span></span>` +
	`<span style="font-size:12px;vertical-align:50%"></span></div>` +
	`Vážení uživatelé, co je v systému nového a proč by vám to nemělo uniknout?<br>`

// Sent row: small element starts with status span instead of sender.
const sentRowDigiHTML = `<small><span style="color:#77bb00">Publikováno</span>, zpráva č. 290462, <b>21.05.2026 7:54</b></small>` +
	`<div><a href="..." class="ajax"><span class="ico50 menu_icon"></span></a>` +
	`<a href="..." class="ajax"><span style="color:#212121"><b>DIGI Den</b></span></a>` +
	`<span style="font-size:12px;padding-left:10px;vertical-align:50%"><span style="color:#888">Přílohy</span></span>` +
	` <span style="vertical-align:50%"><span style="color:#0c9ce1"><b>(1)</b></span></span></div>` +
	`Dobrý den, posílám informace.<br>`

func TestParseRow_Inbox(t *testing.T) {
	t.Parallel()

	msg, err := parseRow("m-290491", inboxRowKaliskovaHTML, false)
	if err != nil {
		t.Fatalf("parseRow: %v", err)
	}

	if msg.ID != "m-290491" {
		t.Errorf("ID = %q, want %q", msg.ID, "m-290491")
	}
	if msg.Number != 290491 {
		t.Errorf("Number = %d, want %d", msg.Number, 290491)
	}
	if msg.Subject != "Obhajoba ročníkových prací 4SC" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if msg.Sender != "Kalíšková Eva (KAL) (učitel 4SC)" {
		t.Errorf("Sender = %q (want collapsed whitespace, single space between parens)", msg.Sender)
	}
	if msg.Status != "" {
		t.Errorf("Status should be empty for inbox row, got %q", msg.Status)
	}
	if msg.Attachments != 0 {
		t.Errorf("Attachments = %d, want 0", msg.Attachments)
	}
	if !strings.HasPrefix(msg.BodyPreview, "Ahoj Dušane") {
		t.Errorf("BodyPreview = %q, want prefix 'Ahoj Dušane'", msg.BodyPreview)
	}

	// Date should round-trip via RFC3339 to 2026-05-21 12:31 local.
	gotTime, err := time.Parse(time.RFC3339, msg.Date)
	if err != nil {
		t.Fatalf("parse Date %q: %v", msg.Date, err)
	}
	want := time.Date(2026, 5, 21, 12, 31, 0, 0, time.Local)
	if !gotTime.Equal(want) {
		t.Errorf("Date parsed to %v, want %v", gotTime, want)
	}
}

func TestParseRow_InboxWithAttachments(t *testing.T) {
	t.Parallel()

	msg, err := parseRow("m-289862", inboxRowNovinkyHTML, false)
	if err != nil {
		t.Fatalf("parseRow: %v", err)
	}
	if msg.Attachments != 9 {
		t.Errorf("Attachments = %d, want 9", msg.Attachments)
	}
	if !strings.Contains(msg.Subject, "NOVINKY") {
		t.Errorf("Subject %q should contain 'NOVINKY'", msg.Subject)
	}
	if msg.Sender != "Odborný konzultant Edookit" {
		t.Errorf("Sender = %q", msg.Sender)
	}
}

func TestParseRow_Sent(t *testing.T) {
	t.Parallel()

	msg, err := parseRow("m-290462", sentRowDigiHTML, true)
	if err != nil {
		t.Fatalf("parseRow: %v", err)
	}
	if msg.Status != "Publikováno" {
		t.Errorf("Status = %q, want 'Publikováno'", msg.Status)
	}
	if msg.Sender != "" {
		t.Errorf("Sender should be empty for sent row, got %q", msg.Sender)
	}
	if msg.Subject != "DIGI Den" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if msg.Attachments != 1 {
		t.Errorf("Attachments = %d, want 1", msg.Attachments)
	}
}

func TestParseRow_BadUID(t *testing.T) {
	t.Parallel()

	// Non-numeric UID should still parse but Number stays 0.
	msg, err := parseRow("custom-id", inboxRowKaliskovaHTML, false)
	if err != nil {
		t.Fatalf("parseRow: %v", err)
	}
	if msg.ID != "custom-id" {
		t.Errorf("ID = %q", msg.ID)
	}
	if msg.Number != 0 {
		t.Errorf("Number = %d, want 0 for non-m-N format", msg.Number)
	}
}

func TestParseAttachmentCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		html string
		want int
	}{
		{
			name: "no attachments",
			html: inboxRowKaliskovaHTML,
			want: 0,
		},
		{
			name: "nine attachments",
			html: inboxRowNovinkyHTML,
			want: 9,
		},
		{
			name: "one attachment (sent)",
			html: sentRowDigiHTML,
			want: 1,
		},
		{
			name: "Přílohy span without (N) sibling — defensive",
			html: `<div><span>Přílohy</span></div>`,
			want: 0,
		},
		{
			name: "Přílohy nested in unrelated context",
			html: `<div><span>Other text</span><span><b>(5)</b></span></div>`,
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + tc.html + "</div>"))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := parseAttachmentCount(doc)
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseBodyPreview(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		html     string
		wantHas  string // substring expected in result
		wantNot  string // substring that must NOT be in result
		maxBytes int    // expected upper bound on body length (0 = don't check)
	}{
		{
			name:    "simple body",
			html:    `<small>...</small><div>...</div>Hello world<br><div>tail</div>`,
			wantHas: "Hello world",
			wantNot: "tail",
		},
		{
			name:    "nbsp collapses to space",
			html:    `<small>...</small><div>...</div>Hello&nbsp;world<br>`,
			wantHas: "Hello world",
		},
		{
			name:    "whitespace collapses",
			html:    `<small>...</small><div>...</div>Hello   world  here<br>`,
			wantHas: "Hello world here",
		},
		{
			name:     "long body truncates with ellipsis",
			html:     `<small>...</small><div>...</div>` + strings.Repeat("x", 500) + `<br>`,
			wantHas:  "…",
			maxBytes: 250, // ~200 chars + ellipsis (3 bytes for ‘…’ in UTF-8)
		},
		{
			name: "no body present",
			html: `<small>...</small><div>...</div>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseBodyPreview(tc.html)
			if tc.wantHas != "" && !strings.Contains(got, tc.wantHas) {
				t.Errorf("got %q, want substring %q", got, tc.wantHas)
			}
			if tc.wantNot != "" && strings.Contains(got, tc.wantNot) {
				t.Errorf("got %q, must not contain %q", got, tc.wantNot)
			}
			if tc.maxBytes > 0 && len(got) > tc.maxBytes {
				t.Errorf("body length %d > max %d", len(got), tc.maxBytes)
			}
		})
	}
}

func TestParseSince(t *testing.T) {
	t.Parallel()

	now := time.Now()

	cases := []struct {
		name    string
		input   string
		wantOK  bool
		within  time.Duration // expected (now - parsed) tolerance, 0 = exact day
		exactT  time.Time
		isEmpty bool
	}{
		{name: "empty", input: "", wantOK: true, isEmpty: true},
		{name: "7d", input: "7d", wantOK: true, within: 7*24*time.Hour + time.Minute},
		{name: "1w", input: "1w", wantOK: true, within: 7*24*time.Hour + time.Minute},
		{name: "2m", input: "2m", wantOK: true, within: 65 * 24 * time.Hour}, // ~2 months, loose
		{name: "1y", input: "1y", wantOK: true, within: 366 * 24 * time.Hour},
		{name: "iso date (local TZ)", input: "2026-05-01", wantOK: true, exactT: time.Date(2026, 5, 1, 0, 0, 0, 0, time.Local)},
		{name: "garbage", input: "not-a-date", wantOK: false},
		{name: "zero count", input: "0d", wantOK: false},
		{name: "negative", input: "-5d", wantOK: false},
		{name: "unknown unit", input: "5q", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseSince(tc.input)
			if (err == nil) != tc.wantOK {
				t.Fatalf("parseSince(%q) error = %v, wantOK = %v", tc.input, err, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if tc.isEmpty {
				if !got.IsZero() {
					t.Errorf("got %v, want zero time", got)
				}
				return
			}
			if !tc.exactT.IsZero() {
				if !got.Equal(tc.exactT) {
					t.Errorf("got %v, want %v", got, tc.exactT)
				}
				return
			}
			// Relative case: result should be at most `within` before now.
			diff := now.Sub(got)
			if diff <= 0 || diff > tc.within {
				t.Errorf("now - got = %v, want in (0, %v]", diff, tc.within)
			}
		})
	}
}

func TestNormalizeLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want int
	}{
		{in: 0, want: defaultLimit},
		{in: -10, want: defaultLimit},
		{in: 1, want: 1},
		{in: 50, want: 50},
		{in: 200, want: 200},
		{in: 500, want: maxLimit},
	}
	for _, tc := range cases {
		if got := normalizeLimit(tc.in); got != tc.want {
			t.Errorf("normalizeLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
