package tools

import (
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

func TestParseFullMessage_ErrorsWhenSubjectAndBodyBothMissing(t *testing.T) {
	t.Parallel()
	// Form panel present but the item names that carry subject and body
	// have vanished (Edookit renamed them). parseFullMessage must fail
	// loudly so schema drift doesn't masquerade as "the message is empty".
	resp := messageEditResponse{
		Authenticated: boolPtr(true),
		Components: messageEditComponents{
			Workspace: []messageEditWorkspaceComponent{{
				DOMTarget: domTargetFormMessage,
				Data: messageEditWorkspaceData{
					FormPanelMain: []messageEditPanel{
						// only object_status survives — name and description__editor are gone
						{Label: "Stav:", Items: []messageEditPanelItem{{
							Name: "object_status", Type: "html",
							Val: `<span style="color:#77bb00">Publikováno</span>`,
						}}},
					},
				},
			}},
		},
	}
	_, err := parseFullMessage(1, &resp, testTZ)
	if err == nil {
		t.Fatal("expected error when both subject and body are missing, got nil")
	}
	if !strings.Contains(err.Error(), "schema") && !strings.Contains(err.Error(), "drifted") {
		t.Errorf("error %q should hint at schema drift", err.Error())
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
