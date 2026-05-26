package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// testTZ is the wall-clock timezone the parsers anchor against. Europe/Prague
// is what production uses for Edookit; pinning it here makes the RFC3339
// assertions stable regardless of where the test host happens to live.
var testTZ = func() *time.Location {
	l, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		panic(err)
	}
	return l
}()

// inboxFixture is a sanitized but structurally-faithful capture of one
// /handler/page/message-edit?__index=N response for a received message
// with one attachment. The shape mirrors what Edookit returns; all PII
// has been replaced with neutral placeholders so the file is safe to
// commit.
var inboxFixture = messageEditResponse{
	Authenticated: boolPtr(true),
	Components: messageEditComponents{
		Workspace: []messageEditWorkspaceComponent{
			{
				DOMTarget: domTargetFormMessage,
				Data: messageEditWorkspaceData{
					FormPanelMain: []messageEditPanel{
						{Hidden: true, Items: []messageEditPanelItem{{Name: "__index", Type: "hidden", Val: "289862"}}},
						{Label: "Stav:", Items: []messageEditPanelItem{
							{
								Name: "object_status",
								Type: "html",
								Val:  `<span style="color:#77bb00">Publikováno</span> Od 19.05.2026 6:24<span style="color:#a6a6a6">, </span><span style="font-size:75%"><b>Vzdělávací konzultant</b>, Út 19.05. 6:24</span>`,
							},
						}},
						{Label: "Předmět:", Items: []messageEditPanelItem{
							{Name: "name", Type: "text", Val: "Letní příměstský tábor 2026"},
						}},
						{Label: "Obsah:", Items: []messageEditPanelItem{
							{
								Name:      "description__editor",
								Type:      "simple_editor",
								ReadValue: `<p>Vážení rodiče,</p><p>posíláme přihlášku na letní tábor.</p><p>Doplňující informace najdete <a href="https://example.test/info">zde</a>.</p><p>S pozdravem,<br />Konzultant</p>`,
								Val:       `&lt;p&gt;Vážení rodiče,&lt;/p&gt;`,
							},
						}},
					},
				},
			},
			{
				DOMTarget: domTargetFileviewer,
				Data: messageEditWorkspaceData{
					Data: []messageEditAttachment{
						{ID: "1@191968", Name: "prihlaska.pdf", Link: "https://example.test/handler/download/file-aaaaaaaa", Date: 1716100800, Trashed: false},
						{ID: "1@191969", Name: "stary-soubor.pdf", Link: "https://example.test/handler/download/file-bbbbbbbb", Date: 1716100800, Trashed: true}, // server says "removed" — should be filtered out
					},
				},
			},
		},
	},
}

// sentFixture mirrors the sent-message shape: same endpoint, same panel
// layout, but the object_status HTML lacks the "Od …" inline date that
// received messages have. Status word + bold author still present.
var sentFixture = messageEditResponse{
	Authenticated: boolPtr(true),
	Components: messageEditComponents{
		Workspace: []messageEditWorkspaceComponent{
			{
				DOMTarget: domTargetFormMessage,
				Data: messageEditWorkspaceData{
					FormPanelMain: []messageEditPanel{
						{Label: "Stav:", Items: []messageEditPanelItem{
							{
								Name: "object_status",
								Type: "html",
								Val:  `<span style="color:#77bb00">Publikováno</span><span style="color:#a6a6a6">, </span><span style="font-size:75%"><b>Test User</b>, Čt 21.05. 7:54</span>`,
							},
						}},
						{Label: "Předmět:", Items: []messageEditPanelItem{
							{Name: "name", Type: "text", Val: "DIGI Den"},
						}},
						{Label: "Obsah:", Items: []messageEditPanelItem{
							{
								Name:      "description__editor",
								Type:      "simple_editor",
								ReadValue: `<p>Dobrý den,</p><p><br /></p><p>posílám informace.</p><p>Test User</p>`,
							},
						}},
					},
				},
			},
			{
				DOMTarget: domTargetFileviewer,
				Data: messageEditWorkspaceData{
					Data: []messageEditAttachment{
						{ID: "1@200000", Name: "ukazka.docx", Link: "https://example.test/handler/download/file-cccccccc", Date: 1716301200},
					},
				},
			},
		},
	},
}

func boolPtr(b bool) *bool { return &b }

// ---------- parseFullMessage ----------

func TestParseFullMessage_Inbox(t *testing.T) {
	t.Parallel()

	msg, err := parseFullMessage(289862, &inboxFixture, testTZ)
	if err != nil {
		t.Fatalf("parseFullMessage: %v", err)
	}
	if msg.ID != "m-289862" || msg.Number != 289862 {
		t.Errorf("got id=%q number=%d, want m-289862/289862", msg.ID, msg.Number)
	}
	if msg.Subject != "Letní příměstský tábor 2026" {
		t.Errorf("subject = %q", msg.Subject)
	}
	if msg.Status != "Publikováno" {
		t.Errorf("status = %q, want Publikováno", msg.Status)
	}
	if msg.Author != "Vzdělávací konzultant" {
		t.Errorf("author = %q, want 'Vzdělávací konzultant'", msg.Author)
	}
	// Inbox status carries "Od 19.05.2026 6:24" — should parse to RFC3339 in Europe/Prague.
	if !strings.HasPrefix(msg.Date, "2026-05-19T06:24:00") {
		t.Errorf("date = %q, want 2026-05-19T06:24:00... prefix", msg.Date)
	}
	if !strings.Contains(msg.BodyHTML, "Vážení rodiče") || !strings.Contains(msg.BodyHTML, "<a href") {
		t.Errorf("body_html missing expected content: %q", msg.BodyHTML)
	}
	if !strings.Contains(msg.BodyText, "Vážení rodiče") {
		t.Errorf("body_text missing greeting: %q", msg.BodyText)
	}
	if strings.Contains(msg.BodyText, "<a") || strings.Contains(msg.BodyText, "<p>") {
		t.Errorf("body_text leaked HTML tags: %q", msg.BodyText)
	}
	// Trashed attachment must be filtered out; only 1 of the 2 fixture entries should survive.
	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1 (the trashed one must be filtered)", len(msg.Attachments))
	}
	a := msg.Attachments[0]
	if a.Name != "prihlaska.pdf" || a.URL == "" || a.ID != "1@191968" {
		t.Errorf("attachment = %+v", a)
	}
}

func TestParseFullMessage_Sent(t *testing.T) {
	t.Parallel()

	msg, err := parseFullMessage(290462, &sentFixture, testTZ)
	if err != nil {
		t.Fatalf("parseFullMessage: %v", err)
	}
	if msg.Subject != "DIGI Den" {
		t.Errorf("subject = %q", msg.Subject)
	}
	if msg.Status != "Publikováno" {
		t.Errorf("status = %q", msg.Status)
	}
	if msg.Author != "Test User" {
		t.Errorf("author = %q", msg.Author)
	}
	// Sent fixture lacks the "Od …" inline date — parser leaves Date empty
	// rather than guessing a year from the "Čt 21.05. 7:54" short form.
	if msg.Date != "" {
		t.Errorf("date = %q, want empty for sent message without 'Od ...' marker", msg.Date)
	}
	if !strings.Contains(msg.BodyText, "Dobrý den") || !strings.Contains(msg.BodyText, "Test User") {
		t.Errorf("body_text = %q", msg.BodyText)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].Name != "ukazka.docx" {
		t.Errorf("attachments = %+v", msg.Attachments)
	}
}

func TestParseFullMessage_AuthenticatedFalseErrs(t *testing.T) {
	t.Parallel()
	resp := messageEditResponse{Authenticated: boolPtr(false)}
	if _, err := parseFullMessage(1, &resp, testTZ); err == nil {
		t.Fatal("expected error for authenticated=false response, got nil")
	}
}

func TestParseFullMessage_MissingFormComponentErrs(t *testing.T) {
	t.Parallel()
	// Workspace with no __lc_Form_Message component — should fail loudly so
	// callers don't get a silently empty FullMessage.
	resp := messageEditResponse{
		Authenticated: boolPtr(true),
		Components: messageEditComponents{
			Workspace: []messageEditWorkspaceComponent{{DOMTarget: "__lc_SomethingElse"}},
		},
	}
	if _, err := parseFullMessage(1, &resp, testTZ); err == nil {
		t.Fatal("expected error for response with no form component, got nil")
	}
}

func TestParseFullMessage_ErrorsWhenStatusSubjectBodyAllMissing(t *testing.T) {
	t.Parallel()
	// Form panel present but EVERY human-readable item is gone
	// (object_status, name, description__editor all missing). That's
	// real schema drift, not a normal state — must fail loudly.
	resp := messageEditResponse{
		Authenticated: boolPtr(true),
		Components: messageEditComponents{
			Workspace: []messageEditWorkspaceComponent{{
				DOMTarget: domTargetFormMessage,
				Data: messageEditWorkspaceData{
					FormPanelMain: []messageEditPanel{
						// only hidden-form internals survive
						{Hidden: true, Items: []messageEditPanelItem{{Name: "__index", Type: "hidden", Val: "1"}}},
					},
				},
			}},
		},
	}
	_, err := parseFullMessage(1, &resp, testTZ)
	if err == nil {
		t.Fatal("expected error when status/subject/body are all missing, got nil")
	}
	if !strings.Contains(err.Error(), "schema") && !strings.Contains(err.Error(), "drifted") {
		t.Errorf("error %q should hint at schema drift", err.Error())
	}
}

// Real-world case discovered by the 294-message smoke loop: messages
// the author deleted in Edookit's UI keep their metadata (status,
// sender, date) but strip subject + body server-side. The status label
// changes to "Smazané autorem DD.MM.YYYY HH:MM". This should parse
// successfully with Deleted=true so callers can distinguish it from
// real schema drift.
func TestParseFullMessage_AuthorDeletedMessageParsesAsDeleted(t *testing.T) {
	t.Parallel()
	resp := messageEditResponse{
		Authenticated: boolPtr(true),
		Components: messageEditComponents{
			Workspace: []messageEditWorkspaceComponent{{
				DOMTarget: domTargetFormMessage,
				Data: messageEditWorkspaceData{
					FormPanelMain: []messageEditPanel{
						{Label: "Stav:", Items: []messageEditPanelItem{{
							Name: "object_status", Type: "html",
							Val: `<span style="color:#989898">Smazané autorem 13.10.2025 7:43</span><span>, </span><span><b>Braun Zdeněk</b>, Po 13.10.25 7:42</span>`,
						}}},
						// No "Předmět:" / "Obsah:" panels — server stripped them.
					},
				},
			}},
		},
	}
	msg, err := parseFullMessage(1, &resp, testTZ)
	if err != nil {
		t.Fatalf("expected successful parse for author-deleted message, got error: %v", err)
	}
	if !msg.Deleted {
		t.Errorf("Deleted = false, want true (status begins with 'Smazané')")
	}
	if !strings.HasPrefix(msg.Status, "Smazané autorem") {
		t.Errorf("Status = %q, want it to start with 'Smazané autorem'", msg.Status)
	}
	if msg.Author != "Braun Zdeněk" {
		t.Errorf("Author = %q, want 'Braun Zdeněk' (even on deleted, the author span survives)", msg.Author)
	}
	if msg.Subject != "" || msg.BodyText != "" || msg.BodyHTML != "" {
		t.Errorf("expected subject/body to be empty on deletion, got Subject=%q Body=%q", msg.Subject, msg.BodyText)
	}
}

func TestParseFullMessage_SubjectOnlyIsAccepted(t *testing.T) {
	t.Parallel()
	// As long as subject OR body is present, the message is considered
	// parseable — empty body is a legitimate "drág & drop attachment only"
	// message Edookit users sometimes send.
	resp := messageEditResponse{
		Authenticated: boolPtr(true),
		Components: messageEditComponents{
			Workspace: []messageEditWorkspaceComponent{{
				DOMTarget: domTargetFormMessage,
				Data: messageEditWorkspaceData{
					FormPanelMain: []messageEditPanel{
						{Label: "Předmět:", Items: []messageEditPanelItem{{Name: "name", Type: "text", Val: "Subject only"}}},
					},
				},
			}},
		},
	}
	msg, err := parseFullMessage(1, &resp, testTZ)
	if err != nil {
		t.Fatalf("expected success for subject-only message: %v", err)
	}
	if msg.Subject != "Subject only" {
		t.Errorf("subject = %q", msg.Subject)
	}
}

func TestParseFullMessage_AttachmentsEmptyArrayNotNull(t *testing.T) {
	t.Parallel()
	// Even without an attachments component, the JSON output must carry
	// `"attachments": []` so callers can iterate unconditionally.
	resp := messageEditResponse{
		Authenticated: boolPtr(true),
		Components: messageEditComponents{
			Workspace: []messageEditWorkspaceComponent{{
				DOMTarget: domTargetFormMessage,
				Data: messageEditWorkspaceData{
					FormPanelMain: []messageEditPanel{
						{Label: "Předmět:", Items: []messageEditPanelItem{{Name: "name", Type: "text", Val: "Bare message"}}},
					},
				},
			}},
		},
	}
	msg, err := parseFullMessage(1, &resp, testTZ)
	if err != nil {
		t.Fatalf("parseFullMessage: %v", err)
	}
	if msg.Attachments == nil {
		t.Error("Attachments is nil; want empty slice for JSON [] output")
	}
	if len(msg.Attachments) != 0 {
		t.Errorf("Attachments = %+v, want empty", msg.Attachments)
	}
}

// ---------- parseStatusHTML ----------

func TestParseStatusHTML(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		html       string
		wantStatus string
		wantAuthor string
		wantDate   string // RFC3339 prefix; empty means "no date should parse"
	}{
		{
			name:       "received with Od date",
			html:       `<span style="color:#77bb00">Publikováno</span> Od 19.05.2026 6:24<span style="color:#a6a6a6">, </span><span style="font-size:75%"><b>Foo Bar</b>, Út 19.05. 6:24</span>`,
			wantStatus: "Publikováno",
			wantAuthor: "Foo Bar",
			wantDate:   "2026-05-19T06:24:00",
		},
		{
			name:       "sent without Od date",
			html:       `<span style="color:#77bb00">Publikováno</span><span style="color:#a6a6a6">, </span><span style="font-size:75%"><b>Saiko Dušan</b>, Čt 21.05. 7:54</span>`,
			wantStatus: "Publikováno",
			wantAuthor: "Saiko Dušan",
			wantDate:   "",
		},
		{
			name:       "Nepublikováno status",
			html:       `<span style="color:#cc4444">Nepublikováno</span>, draft state`,
			wantStatus: "Nepublikováno",
			wantAuthor: "",
			wantDate:   "",
		},
		{
			name:       "empty input",
			html:       "",
			wantStatus: "",
			wantAuthor: "",
			wantDate:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotS, gotA, gotD := parseStatusHTML(tc.html, testTZ)
			if gotS != tc.wantStatus {
				t.Errorf("status = %q, want %q", gotS, tc.wantStatus)
			}
			if gotA != tc.wantAuthor {
				t.Errorf("author = %q, want %q", gotA, tc.wantAuthor)
			}
			if tc.wantDate == "" {
				if gotD != "" {
					t.Errorf("date = %q, want empty", gotD)
				}
			} else if !strings.HasPrefix(gotD, tc.wantDate) {
				t.Errorf("date = %q, want prefix %q", gotD, tc.wantDate)
			}
		})
	}
}

// ---------- htmlToText ----------

func TestHTMLToText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "paragraphs separated by newline",
			in:   "<p>First line</p><p>Second line</p>",
			want: "First line\nSecond line",
		},
		{
			name: "br inside paragraph",
			in:   "<p>Hello,<br />world</p>",
			want: "Hello,\nworld",
		},
		{
			name: "entity decode",
			in:   "<p>Caf&eacute; &amp; spol.</p>",
			want: "Café & spol.",
		},
		{
			name: "inline tags preserve text",
			in:   "<p>plain <a href=\"x\">link</a> and <b>bold</b> text</p>",
			want: "plain link and bold text",
		},
		{
			name: "p with only br collapses to nothing meaningful",
			in:   "<p>One</p><p><br /></p><p>Two</p>",
			want: "One\n\nTwo",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := htmlToText(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------- normalizeMessageID ----------

func TestNormalizeMessageID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "m-289862", want: 289862},
		{in: "289862", want: 289862},
		{in: "  m-1  ", want: 1},
		{in: "", wantErr: true},
		{in: "m-abc", wantErr: true},
		{in: "0", wantErr: true},
		{in: "-5", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeMessageID(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got %d", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error for %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// ---------- findFormItem ----------

func TestFindFormItem(t *testing.T) {
	t.Parallel()
	panels := []messageEditPanel{
		{Label: "A", Items: []messageEditPanelItem{{Name: "a1"}, {Name: "a2"}}},
		{Label: "B", Items: []messageEditPanelItem{{Name: "b1"}}},
	}
	if item, idx := findFormItem(panels, "a2"); item == nil || idx != 0 || item.Name != "a2" {
		t.Errorf("findFormItem(a2) = (%v, %d), want non-nil/0", item, idx)
	}
	if item, idx := findFormItem(panels, "b1"); item == nil || idx != 1 {
		t.Errorf("findFormItem(b1) = (%v, %d), want non-nil/1", item, idx)
	}
	if item, idx := findFormItem(panels, "missing"); item != nil || idx != -1 {
		t.Errorf("findFormItem(missing) = (%v, %d), want (nil, -1)", item, idx)
	}
}

// ---------- asString ----------

func TestAsString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   any
		want string
	}{
		{in: "hello", want: "hello"},
		{in: float64(42), want: "42"},
		{in: float64(3.5), want: "3.5"},
		{in: true, want: "true"},
		{in: nil, want: ""},
		{in: []string{"x"}, want: ""}, // unknown type → empty
	}
	for _, tc := range cases {
		got := asString(tc.in)
		if got != tc.want {
			t.Errorf("asString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------- workspace shape tolerance + Acceptance grid ----------

// Real-world regression: at least one Edookit message returns the
// __lc_Grid_Acceptance workspace component with `data` as a JSON array
// (rows of the read-receipt table) instead of an object. The custom
// UnmarshalJSON on messageEditWorkspaceData must dispatch on the first
// non-whitespace byte, decoding objects into the typed fields and
// arrays into the Acceptance grid. Anything else is a tolerated no-op
// so a future unknown component shape doesn't break the whole parse.
func TestWorkspaceData_UnmarshalDispatchesOnShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		raw            string
		wantFormPanels int
		wantAttachLen  int
		wantAcceptRows int
	}{
		{
			name:           "object with form panels",
			raw:            `{"__form_panel_main":[{"label":"Předmět:","items":[{"name":"name","type":"text","val":"x"}]}]}`,
			wantFormPanels: 1,
		},
		{
			name:          "object with attachments",
			raw:           `{"data":[{"id":"1@1","name":"a.pdf","link":"https://x/a","date":0}]}`,
			wantAttachLen: 1,
		},
		{
			name:           "array of acceptance rows",
			raw:            `[["1","Alice","21.05.2026","",""],["2","Bob","","Bob Sr",""]]`,
			wantAcceptRows: 2,
		},
		{"null is a no-op", `null`, 0, 0, 0},
		{"true is a no-op", `true`, 0, 0, 0},
		{"number is a no-op", `42`, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var d messageEditWorkspaceData
			if err := json.Unmarshal([]byte(tc.raw), &d); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(d.FormPanelMain) != tc.wantFormPanels {
				t.Errorf("FormPanelMain len = %d, want %d", len(d.FormPanelMain), tc.wantFormPanels)
			}
			if len(d.Data) != tc.wantAttachLen {
				t.Errorf("Data len = %d, want %d", len(d.Data), tc.wantAttachLen)
			}
			if len(d.Acceptance) != tc.wantAcceptRows {
				t.Errorf("Acceptance len = %d, want %d", len(d.Acceptance), tc.wantAcceptRows)
			}
		})
	}
}

// End-to-end: a mixed workspace (form + fileviewer + acceptance) decodes
// cleanly through json.Unmarshal — this is the production wire shape
// that triggered the original bug on m-290491.
func TestParseFullMessage_MixedWorkspaceWithAcceptanceGrid(t *testing.T) {
	t.Parallel()
	rawJSON := `{
		"authenticated": true,
		"components": {
			"workspace": [
				{
					"DOMTarget": "__lc_Form_Message",
					"data": {
						"__form_panel_main": [
							{"label":"Stav:","items":[{"name":"object_status","type":"html","val":"<span>Publikováno</span><span><b>Test</b>, Čt 21.05.</span>"}]},
							{"label":"Předmět:","items":[{"name":"name","type":"text","val":"Subject"}]},
							{"label":"Obsah:","items":[{"name":"description__editor","type":"simple_editor","readValue":"<p>body</p>"}]}
						]
					}
				},
				{
					"DOMTarget": "__lc_Fileviewer_Slave_datatemplate_message",
					"data": {"data": []}
				},
				{
					"DOMTarget": "__lc_Grid_Acceptance",
					"data": [
						["1001","Fajkus Eliáš","21.05.2026","Fajkus Martin<br>Fajkusová Soňa","Ne<br>22.05.2026"],
						["1002","Holub Tadeáš","","",""]
					]
				}
			]
		}
	}`
	var raw messageEditResponse
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg, err := parseFullMessage(290491, &raw, testTZ)
	if err != nil {
		t.Fatalf("parseFullMessage: %v", err)
	}
	if msg.Subject != "Subject" {
		t.Errorf("subject = %q", msg.Subject)
	}
	if len(msg.Recipients) != 2 {
		t.Fatalf("Recipients = %d, want 2", len(msg.Recipients))
	}

	// Fajkus Eliáš: read 21.05.2026, two parents — one unread ("Ne") + one read 22.05.2026.
	r0 := msg.Recipients[0]
	if r0.Name != "Fajkus Eliáš" {
		t.Errorf("r0.Name = %q", r0.Name)
	}
	if r0.ReadAt != "2026-05-21" {
		t.Errorf("r0.ReadAt = %q, want 2026-05-21", r0.ReadAt)
	}
	if len(r0.Parents) != 2 || r0.Parents[0] != "Fajkus Martin" || r0.Parents[1] != "Fajkusová Soňa" {
		t.Errorf("r0.Parents = %v", r0.Parents)
	}
	if len(r0.ParentsReadAt) != 2 || r0.ParentsReadAt[0] != "" || r0.ParentsReadAt[1] != "2026-05-22" {
		t.Errorf("r0.ParentsReadAt = %v, want [\"\", \"2026-05-22\"]", r0.ParentsReadAt)
	}

	// Holub Tadeáš: not yet read, no parents listed (staff or empty fields).
	r1 := msg.Recipients[1]
	if r1.Name != "Holub Tadeáš" || r1.ReadAt != "" {
		t.Errorf("r1 = %+v", r1)
	}
	if len(r1.Parents) != 0 {
		t.Errorf("r1.Parents = %v, want empty", r1.Parents)
	}
}

func TestCollectRecipients(t *testing.T) {
	t.Parallel()
	// Defensive against short rows (Edookit could one day add or drop a
	// column; short rows are silently skipped rather than panicking).
	rows := [][]string{
		{"1", "Alice", "21.05.2026", "", ""},
		{"2", "Bob"}, // too short, must be skipped
		{"3", "Carol", "Ne", "Carol Sr<br>Carol Jr", "Ne<br>22.05.2026"},
	}
	got := collectRecipients(rows)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (the 2-cell row must be skipped)", len(got))
	}
	if got[0].Name != "Alice" || got[0].ReadAt != "2026-05-21" {
		t.Errorf("Alice: %+v", got[0])
	}
	if got[1].Name != "Carol" || got[1].ReadAt != "" {
		t.Errorf("Carol: %+v", got[1])
	}
	if len(got[1].Parents) != 2 || len(got[1].ParentsReadAt) != 2 {
		t.Errorf("Carol parents: %+v / %+v", got[1].Parents, got[1].ParentsReadAt)
	}
	if got[1].ParentsReadAt[1] != "2026-05-22" {
		t.Errorf("Carol second parent read = %q", got[1].ParentsReadAt[1])
	}
}

// Edookit collapses parents_first_seen to a single value when all
// parents share the same status (one "Ne" for two unread parents).
// The parser normalizes that so ParentsReadAt is always aligned with
// Parents — Claude/users can index them in pairs without surprises.
func TestCollectRecipients_AlignsParentsReadStatusWithParentsList(t *testing.T) {
	t.Parallel()
	rows := [][]string{
		// Both parents unread — Edookit sends single "Ne", normalize to ["", ""].
		{"1", "Both unread", "21.05.2026", "Parent A<br>Parent B", "Ne"},
		// One unread + one read — Edookit sends both, no normalization needed.
		{"2", "Split read", "21.05.2026", "Parent C<br>Parent D", "Ne<br>22.05.2026"},
		// Three parents but only one status — replicate.
		{"3", "Three same", "21.05.2026", "P E<br>P F<br>P G", "Ne"},
	}
	got := collectRecipients(rows)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if len(got[0].Parents) != 2 || len(got[0].ParentsReadAt) != 2 {
		t.Errorf("row 0: Parents=%d, ParentsReadAt=%d, want both 2", len(got[0].Parents), len(got[0].ParentsReadAt))
	}
	if got[0].ParentsReadAt[0] != "" || got[0].ParentsReadAt[1] != "" {
		t.Errorf("row 0: ParentsReadAt = %v, want [\"\", \"\"] (single Ne → both unread)", got[0].ParentsReadAt)
	}
	if got[1].ParentsReadAt[0] != "" || got[1].ParentsReadAt[1] != "2026-05-22" {
		t.Errorf("row 1: ParentsReadAt = %v, want [\"\", \"2026-05-22\"]", got[1].ParentsReadAt)
	}
	if len(got[2].Parents) != 3 || len(got[2].ParentsReadAt) != 3 {
		t.Errorf("row 2: Parents=%d, ParentsReadAt=%d, want both 3", len(got[2].Parents), len(got[2].ParentsReadAt))
	}
}

func TestCzechDateToISO(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"21.05.2026", "2026-05-21"},
		{"1.1.2026", "2026-01-01"},
		{" 21.05.2026 ", "2026-05-21"},
		{"Ne", ""}, // Edookit's "not read"
		{"", ""},
		{"not-a-date", ""},
		{"21.05", ""}, // not 3 components
		{"00.05.2026", ""},
	}
	for _, tc := range cases {
		if got := czechDateToISO(tc.in); got != tc.want {
			t.Errorf("czechDateToISO(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
