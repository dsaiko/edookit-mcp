package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dsaiko/edookit-mcp/internal/client"
)

// buildMessageEditJSON synthesizes a /handler/page/message-edit response
// pointing at the given attachment URLs. Used by the attachment-downloader
// tests so we control both the message JSON and the file bytes the test
// HTTP server serves. mIndex is the numeric portion of the message id.
func buildMessageEditJSON(t *testing.T, mIndex int, attachments []messageEditAttachment) []byte {
	t.Helper()
	resp := messageEditResponse{
		Authenticated: boolPtr(true),
		Components: messageEditComponents{
			Workspace: []messageEditWorkspaceComponent{
				{
					DOMTarget: domTargetFormMessage,
					Data: messageEditWorkspaceData{
						FormPanelMain: []messageEditPanel{
							{Label: "Stav:", Items: []messageEditPanelItem{{
								Name: "object_status",
								Type: "html",
								Val:  `<span style="color:#77bb00">Publikováno</span><span>, </span><span><b>Test</b>, Čt 21.05.</span>`,
							}}},
							{Label: "Předmět:", Items: []messageEditPanelItem{{Name: "name", Type: "text", Val: "Subject"}}},
							{Label: "Obsah:", Items: []messageEditPanelItem{{Name: "description__editor", Type: "simple_editor", ReadValue: "<p>body</p>"}}},
						},
					},
				},
				{
					DOMTarget: domTargetFileviewer,
					Data:      messageEditWorkspaceData{Data: attachments},
				},
			},
		},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// buildClient wires a client.Client at srv.URL with a no-op LoginFunc and
// retries disabled — keeps tests fast and isolated from the real OIDC flow.
// The cookie-jar fence and authenticated-false retry paths are exercised
// elsewhere (in internal/client tests).
func buildClient(t *testing.T, srv *httptest.Server) *client.Client {
	t.Helper()
	cli, err := client.New(client.Config{
		BaseURL:     srv.URL,
		Username:    "u",
		Password:    "p",
		MaxAttempts: 1,
		LoginFunc: func(_ context.Context) ([]*http.Cookie, error) {
			return []*http.Cookie{{Name: "PHPSESSID", Value: "fixed", Path: "/"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return cli
}

// ---------- resolveDestDir ----------

func TestResolveDestDir_DefaultUsesOSTempEdookitMsgID(t *testing.T) {
	t.Parallel()
	got, err := resolveDestDir("", "m-12345")
	if err != nil {
		t.Fatalf("resolveDestDir: %v", err)
	}
	want := filepath.Join(os.TempDir(), "edookit-mcp", "m-12345")
	if got != want {
		t.Errorf("default path = %q, want %q (os-portable temp dir)", got, want)
	}
}

func TestResolveDestDir_ExpandsTilde(t *testing.T) {
	t.Parallel()
	got, err := resolveDestDir("~/foo/bar", "m-1")
	if err != nil {
		t.Fatalf("resolveDestDir: %v", err)
	}
	if strings.HasPrefix(got, "~") {
		t.Errorf("path = %q still starts with ~, want it expanded", got)
	}
	if !strings.HasSuffix(got, filepath.Join("foo", "bar")) {
		t.Errorf("path = %q should end with foo/bar", got)
	}
}

func TestResolveDestDir_AbsolutePathPassedThrough(t *testing.T) {
	t.Parallel()
	got, err := resolveDestDir("/tmp/explicit/path", "m-1")
	if err != nil {
		t.Fatalf("resolveDestDir: %v", err)
	}
	if got != "/tmp/explicit/path" {
		t.Errorf("got %q, want exact passthrough", got)
	}
}

func TestResolveDestDir_RelativePathRejected(t *testing.T) {
	t.Parallel()
	// MCP server's cwd is unpredictable (whatever started the host app),
	// so accepting relative paths would land files in surprising places.
	cases := []string{"Downloads", "./foo", "foo/bar"}
	for _, raw := range cases {
		_, err := resolveDestDir(raw, "m-1")
		if err == nil {
			t.Errorf("resolveDestDir(%q) returned nil error; want a clear rejection", raw)
		}
		if err != nil && !strings.Contains(err.Error(), "absolute") {
			t.Errorf("resolveDestDir(%q) error %q should mention absolute requirement", raw, err.Error())
		}
	}
}

// ---------- DownloadAttachments end-to-end ----------

func TestDownloadAttachments_HappyPath(t *testing.T) {
	t.Parallel()

	// Two attachments served by the same test server. Each has its own
	// payload of known size so assertions are precise.
	files := map[string]string{
		"/handler/download/file-aaa": "AAAA hello",       // 10 bytes
		"/handler/download/file-bbb": "BBBB bigger body", // 16 bytes
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("warmup ok")) })

	// Track which attachment downloads were requested so we can assert the
	// loop actually drove both.
	hits := map[string]int{}
	for path, body := range files {
		p, b := path, body // capture
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			hits[p]++
			_, _ = w.Write([]byte(b))
		})
	}

	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("__index") != "12345" {
			http.Error(w, "wrong index", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(msgJSON)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	msgJSON = buildMessageEditJSON(t, 12345, []messageEditAttachment{
		{ID: "1@1", Name: "first.txt", Link: srv.URL + "/handler/download/file-aaa", Date: 1716100800},
		{ID: "1@2", Name: "second.txt", Link: srv.URL + "/handler/download/file-bbb", Date: 1716100900},
		{ID: "1@3", Name: "skipped.txt", Link: srv.URL + "/handler/download/file-aaa", Date: 0, Trashed: true},
	})

	cli := buildClient(t, srv)
	destDir := t.TempDir()

	res, err := DownloadAttachments(context.Background(), cli, "m-12345", DownloadOptions{DestDir: destDir})
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	if res.MessageID != "m-12345" {
		t.Errorf("MessageID = %q", res.MessageID)
	}
	if res.Directory != destDir {
		t.Errorf("Directory = %q, want %q", res.Directory, destDir)
	}
	// Trashed attachment must have been filtered out by parseFullMessage.
	if len(res.Files) != 2 {
		t.Fatalf("Files = %d entries, want 2 (trashed must be skipped)", len(res.Files))
	}

	for _, f := range res.Files {
		if f.Error != "" {
			t.Errorf("attachment %q reported error: %s", f.Name, f.Error)
		}
		// File must exist on disk with the reported size.
		info, err := os.Stat(f.Path)
		if err != nil {
			t.Errorf("stat %s: %v", f.Path, err)
			continue
		}
		if info.Size() != f.Bytes {
			t.Errorf("file %s on-disk size %d != reported %d", f.Path, info.Size(), f.Bytes)
		}
		// 0600 perms (sensitive school content)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("file %s perms = %o, want 0600", f.Path, info.Mode().Perm())
		}
	}

	// Both server endpoints should have been hit exactly once.
	for path := range files {
		if hits[path] != 1 {
			t.Errorf("server endpoint %s hit %d times, want 1", path, hits[path])
		}
	}
}

func TestDownloadAttachments_OverwritePolicy(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("warmup ok")) })

	const newBody = "FRESH CONTENT"
	const oldBody = "OLD CONTENT"

	var serverHits int
	mux.HandleFunc("/handler/download/file-x", func(w http.ResponseWriter, _ *http.Request) {
		serverHits++
		_, _ = w.Write([]byte(newBody))
	})

	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(msgJSON)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	msgJSON = buildMessageEditJSON(t, 1, []messageEditAttachment{
		{ID: "1@1", Name: "policy.txt", Link: srv.URL + "/handler/download/file-x"},
	})
	cli := buildClient(t, srv)

	destDir := t.TempDir()
	target := filepath.Join(destDir, "policy.txt")

	// Pre-create a file with old content so the "skip if exists" path fires.
	if err := os.WriteFile(target, []byte(oldBody), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 1) overwrite=false -> existing file is left alone (skipped).
	res, err := DownloadAttachments(context.Background(), cli, "m-1", DownloadOptions{DestDir: destDir})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(res.Files) != 1 || !res.Files[0].Skipped {
		t.Errorf("expected one skipped file, got %+v", res.Files)
	}
	if serverHits != 0 {
		t.Errorf("server hit %d times during skip path, want 0", serverHits)
	}
	if got, _ := os.ReadFile(target); string(got) != oldBody {
		t.Errorf("file content changed under overwrite=false: %q", got)
	}

	// 2) overwrite=true -> file is replaced with new content.
	res, err = DownloadAttachments(context.Background(), cli, "m-1", DownloadOptions{DestDir: destDir, Overwrite: true})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if res.Files[0].Skipped {
		t.Errorf("overwrite=true should not be marked skipped")
	}
	if serverHits != 1 {
		t.Errorf("server hit %d times during overwrite path, want 1", serverHits)
	}
	if got, _ := os.ReadFile(target); string(got) != newBody {
		t.Errorf("file content = %q after overwrite, want %q", got, newBody)
	}
}

func TestDownloadAttachments_PerFileFailureDoesntAbortLoop(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("warmup ok")) })

	mux.HandleFunc("/handler/download/file-ok", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok body"))
	})
	mux.HandleFunc("/handler/download/file-broken", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})

	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(msgJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	msgJSON = buildMessageEditJSON(t, 1, []messageEditAttachment{
		{ID: "1@a", Name: "ok.txt", Link: srv.URL + "/handler/download/file-ok"},
		{ID: "1@b", Name: "broken.txt", Link: srv.URL + "/handler/download/file-broken"},
	})
	cli := buildClient(t, srv)
	destDir := t.TempDir()

	res, err := DownloadAttachments(context.Background(), cli, "m-1", DownloadOptions{DestDir: destDir})
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("Files = %d, want 2 (broken should still appear in the result)", len(res.Files))
	}
	var ok, broken DownloadedFile
	for _, f := range res.Files {
		switch f.Name {
		case "ok.txt":
			ok = f
		case "broken.txt":
			broken = f
		}
	}
	if ok.Error != "" {
		t.Errorf("ok entry has error: %s", ok.Error)
	}
	if ok.Bytes != int64(len("ok body")) {
		t.Errorf("ok bytes = %d", ok.Bytes)
	}
	if broken.Error == "" {
		t.Errorf("broken entry has no error, expected one")
	}
	// Failed download must not leave a partial file behind under the final
	// name OR as a half-written .part temp file. The temp-then-rename
	// implementation cleans both up; this asserts that.
	if _, err := os.Stat(broken.Path); !os.IsNotExist(err) {
		t.Errorf("broken file still exists on disk: %v", err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(destDir, ".edookit-download-*.part"))
	if len(leftovers) > 0 {
		t.Errorf("temp file(s) left behind after failed download: %v", leftovers)
	}
}

// U2: overwrite=true must NOT clobber a pre-existing file if the download
// fails — the temp-then-rename pattern keeps the old file intact until
// the new one is fully written. Without this guarantee, a flaky network
// would corrupt previously-good downloads on retry.
func TestDownloadAttachments_OverwriteTruePreservesOldFileOnFailure(t *testing.T) {
	t.Parallel()

	const oldContent = "PREVIOUSLY DOWNLOADED, MUST SURVIVE"

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/handler/download/file-broken", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "transient", http.StatusInternalServerError)
	})

	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(msgJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	msgJSON = buildMessageEditJSON(t, 1, []messageEditAttachment{
		{ID: "1@b", Name: "important.pdf", Link: srv.URL + "/handler/download/file-broken"},
	})
	cli := buildClient(t, srv)

	destDir := t.TempDir()
	target := filepath.Join(destDir, "important.pdf")
	if err := os.WriteFile(target, []byte(oldContent), 0o600); err != nil {
		t.Fatalf("seed pre-existing file: %v", err)
	}

	res, err := DownloadAttachments(context.Background(), cli, "m-1", DownloadOptions{DestDir: destDir, Overwrite: true})
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	if res.Files[0].Error == "" {
		t.Fatal("expected per-file error from failed download")
	}
	// Old content must still be on disk — temp-then-rename guarantees the
	// rename only happens after a fully successful download + close.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("re-read pre-existing file: %v", err)
	}
	if string(got) != oldContent {
		t.Errorf("pre-existing file content changed after failed overwrite: got %q, want %q", got, oldContent)
	}
	// And no orphan .part temp file should be left behind.
	leftovers, _ := filepath.Glob(filepath.Join(destDir, ".edookit-download-*.part"))
	if len(leftovers) > 0 {
		t.Errorf("temp file(s) left behind: %v", leftovers)
	}
}

// U3 + W3: windowsUnsafeName is a no-op on non-Windows and flags reserved
// chars / reserved DOS basenames / trailing-space-or-dot on Windows. We
// can't easily test the Windows-positive branch from a Unix CI host
// (runtime.GOOS is fixed at build), but we can verify the GOOS gate by
// asserting that NONE of the per-rule cases trigger on a non-Windows host.
func TestWindowsUnsafeName(t *testing.T) {
	t.Parallel()
	// On the test host (not Windows in any plausible CI / dev setup) the
	// function should always return "" — Unix users with file names like
	// "report 12:00.pdf", "CON", or "trailing space " should not be blocked.
	cases := []string{
		// reserved chars
		"report:stream", `file<x>.txt`, `"quoted".pdf`, "weird|name.txt", "what?.txt", "star*.bin",
		// reserved basenames
		"CON", "con.txt", "PRN.pdf", "AUX", "NUL.log", "COM1", "COM9.docx", "LPT0", "lpt9.bin",
		// trailing space / dot
		"trailing space ", "trailing-dot.", "ok.pdf.",
		// negative controls — should always pass
		"ok.pdf", "../escape", "ContentList.json",
	}
	for _, name := range cases {
		if got := windowsUnsafeName(name); got != "" {
			t.Errorf("non-Windows: windowsUnsafeName(%q) = %q, want empty (GOOS gate must keep all Unix names passing)", name, got)
		}
	}
}

func TestDownloadAttachments_PathTraversalRefused(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("warmup ok")) })
	mux.HandleFunc("/handler/download/file-x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	})

	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(msgJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Server-supplied filename with traversal — must be sanitized to bare
	// base name "passwd", landing inside destDir, not at /tmp/passwd.
	msgJSON = buildMessageEditJSON(t, 1, []messageEditAttachment{
		{ID: "1@a", Name: "../../../tmp/passwd", Link: srv.URL + "/handler/download/file-x"},
	})
	cli := buildClient(t, srv)
	destDir := t.TempDir()

	res, err := DownloadAttachments(context.Background(), cli, "m-1", DownloadOptions{DestDir: destDir})
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	// File must have ended up inside destDir.
	want := filepath.Join(destDir, "passwd")
	if res.Files[0].Path != want {
		t.Errorf("Path = %q, want %q (traversal must be neutralized to base name)", res.Files[0].Path, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected file at %s, stat err: %v", want, err)
	}
}

func TestDownloadAttachments_DateInRFC3339(t *testing.T) {
	t.Parallel()
	// Make sure the attachment date came back as RFC3339 (not raw unix int).
	ts := int64(1716100800) // 2024-05-19 08:00:00 UTC
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/handler/download/file-d", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("d"))
	})
	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(msgJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	msgJSON = buildMessageEditJSON(t, 7, []messageEditAttachment{
		{ID: "1@d", Name: "f.txt", Link: srv.URL + "/handler/download/file-d", Date: ts},
	})
	cli := buildClient(t, srv)

	msg, err := GetMessage(context.Background(), cli, "m-7")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(msg.Attachments))
	}
	got := msg.Attachments[0].Date
	if got == "" {
		t.Fatalf("attachment date empty, want RFC3339")
	}
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("attachment date %q not RFC3339: %v", got, err)
	}
	if parsed.Unix() != ts {
		t.Errorf("parsed unix %d != input %d", parsed.Unix(), ts)
	}
}

// Used elsewhere to silence the linter about unused variables in fixtures.
var _ = strconv.Itoa

func TestDownloadAttachments_EmptyURLReportedPerFile(t *testing.T) {
	t.Parallel()
	// An attachment with no Link must not silently resolve to baseURL and
	// save the landing page as the file. The other attachments in the
	// same call must continue normally.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/handler/download/file-good", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("good"))
	})
	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(msgJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	msgJSON = buildMessageEditJSON(t, 1, []messageEditAttachment{
		{ID: "1@a", Name: "good.txt", Link: srv.URL + "/handler/download/file-good"},
		{ID: "1@b", Name: "ghost.txt", Link: ""},
		{ID: "1@c", Name: "blank.txt", Link: "   "},
	})
	cli := buildClient(t, srv)
	destDir := t.TempDir()

	res, err := DownloadAttachments(context.Background(), cli, "m-1", DownloadOptions{DestDir: destDir})
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	if len(res.Files) != 3 {
		t.Fatalf("Files = %d, want 3", len(res.Files))
	}
	for _, f := range res.Files {
		switch f.Name {
		case "good.txt":
			if f.Error != "" {
				t.Errorf("good entry has error: %s", f.Error)
			}
		case "ghost.txt", "blank.txt":
			if f.Error == "" {
				t.Errorf("%s should have errored on empty URL", f.Name)
			}
			if _, err := os.Stat(filepath.Join(destDir, f.Name)); err == nil {
				t.Errorf("%s should not have been created on disk", f.Name)
			}
		}
	}
}

func TestDownloadAttachments_DuplicateFilenamesGetSuffixed(t *testing.T) {
	t.Parallel()
	// Two attachments share the same Edookit-supplied name. We want both
	// files written to disk — second one gets a "-2" suffix so neither
	// collides nor is swallowed by skip-if-exists.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/handler/download/file-1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first payload"))
	})
	mux.HandleFunc("/handler/download/file-2", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("second payload"))
	})
	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(msgJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	msgJSON = buildMessageEditJSON(t, 1, []messageEditAttachment{
		{ID: "1@a", Name: "report.pdf", Link: srv.URL + "/handler/download/file-1"},
		{ID: "1@b", Name: "report.pdf", Link: srv.URL + "/handler/download/file-2"},
	})
	cli := buildClient(t, srv)
	destDir := t.TempDir()

	res, err := DownloadAttachments(context.Background(), cli, "m-1", DownloadOptions{DestDir: destDir})
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("Files = %d, want 2", len(res.Files))
	}
	// Name (display) stays "report.pdf" for both; Path is disambiguated.
	if res.Files[0].Name != "report.pdf" || res.Files[1].Name != "report.pdf" {
		t.Errorf("Names = %q / %q, want both report.pdf", res.Files[0].Name, res.Files[1].Name)
	}
	if filepath.Base(res.Files[0].Path) == filepath.Base(res.Files[1].Path) {
		t.Errorf("Paths collide: both %q — second attachment should have been suffixed", res.Files[0].Path)
	}
	if filepath.Base(res.Files[1].Path) != "report-2.pdf" {
		t.Errorf("second path = %q, want filename report-2.pdf", res.Files[1].Path)
	}
	// Both files should exist with their distinct contents.
	a, _ := os.ReadFile(res.Files[0].Path)
	b, _ := os.ReadFile(res.Files[1].Path)
	if string(a) != "first payload" || string(b) != "second payload" {
		t.Errorf("contents mismatch: %q / %q", a, b)
	}
}

func TestDownloadAttachments_SessionExpiryDetectedOnHTMLResponse(t *testing.T) {
	t.Parallel()
	// Edookit can answer a binary download URL with HTTP 200 + an HTML
	// login page when the session expires (no off-origin redirect). Without
	// the Content-Type check in GetTo we would happily stream the login
	// HTML into the destination file. Here the test server returns HTML
	// for the download endpoint; we expect the per-file error to mention
	// the html / re-login condition and the file to NOT be created.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/handler/download/file-html", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>please log in</body></html>"))
	})
	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(msgJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	msgJSON = buildMessageEditJSON(t, 1, []messageEditAttachment{
		{ID: "1@h", Name: "doc.pdf", Link: srv.URL + "/handler/download/file-html"},
	})
	cli := buildClient(t, srv)
	destDir := t.TempDir()

	res, err := DownloadAttachments(context.Background(), cli, "m-1", DownloadOptions{DestDir: destDir})
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("Files = %d, want 1", len(res.Files))
	}
	if res.Files[0].Error == "" {
		t.Fatalf("expected per-file error for HTML response; got none")
	}
	if !strings.Contains(strings.ToLower(res.Files[0].Error), "html") &&
		!strings.Contains(strings.ToLower(res.Files[0].Error), "re-login") {
		t.Errorf("error %q should mention html / re-login", res.Files[0].Error)
	}
	if _, err := os.Stat(res.Files[0].Path); err == nil {
		t.Errorf("file at %s should not have been created", res.Files[0].Path)
	}
}

// V2: an application/json response on a download endpoint (e.g.
// {"authenticated":false} or a generic API error envelope) must NOT be
// written into the destination file. Unlike text/html, JSON is treated
// as deterministic so we propagate the error rather than triggering
// invalidate+retry.
func TestDownloadAttachments_JSONResponseRejected(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/handler/download/file-json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authenticated":false}`))
	})
	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(msgJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	msgJSON = buildMessageEditJSON(t, 1, []messageEditAttachment{
		{ID: "1@j", Name: "doc.pdf", Link: srv.URL + "/handler/download/file-json"},
	})
	cli := buildClient(t, srv)
	destDir := t.TempDir()

	res, err := DownloadAttachments(context.Background(), cli, "m-1", DownloadOptions{DestDir: destDir})
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	if res.Files[0].Error == "" {
		t.Fatal("expected per-file error for JSON response on download endpoint")
	}
	if !strings.Contains(strings.ToLower(res.Files[0].Error), "json") {
		t.Errorf("error %q should mention json", res.Files[0].Error)
	}
	if _, err := os.Stat(res.Files[0].Path); err == nil {
		t.Errorf("file at %s should not have been created", res.Files[0].Path)
	}
}

// V3: GetTo pre-flight origin check — an attachment URL pointing
// off-origin must be refused before any request is dispatched. Without
// this guard the bogus URL would be requested twice (once + retry after
// invalidate) before the post-response check failed.
func TestDownloadAttachments_OffOriginURLRefusedPreFlight(t *testing.T) {
	t.Parallel()

	// "Plus4U" lookalike served on a different host. localhost vs 127.0.0.1
	// are DNS-equivalent but lexically distinct, matching what sameOrigin
	// would treat as cross-origin.
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("evil payload"))
	}))
	defer foreign.Close()
	foreignURL := strings.Replace(foreign.URL, "127.0.0.1", "localhost", 1)

	var foreignHits int
	foreignMux := http.NewServeMux()
	foreignMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		foreignHits++
		_, _ = w.Write([]byte("should never reach here"))
	})
	foreign.Config.Handler = foreignMux

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(msgJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	msgJSON = buildMessageEditJSON(t, 1, []messageEditAttachment{
		{ID: "1@x", Name: "doc.pdf", Link: foreignURL + "/handler/download/file-evil"},
	})
	cli := buildClient(t, srv)
	destDir := t.TempDir()

	res, err := DownloadAttachments(context.Background(), cli, "m-1", DownloadOptions{DestDir: destDir})
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	if res.Files[0].Error == "" {
		t.Fatal("expected per-file error for off-origin URL")
	}
	if !strings.Contains(strings.ToLower(res.Files[0].Error), "off-origin") {
		t.Errorf("error %q should mention off-origin", res.Files[0].Error)
	}
	if foreignHits != 0 {
		t.Errorf("foreign server was hit %d time(s); want 0 (the pre-flight check should prevent the request)", foreignHits)
	}
}

// V4: case-insensitive filesystems (macOS APFS default, Windows NTFS)
// alias "Report.pdf" and "report.pdf" to the same on-disk file. Even on
// case-sensitive Linux, we want the dedup logic to be defensive so a
// downloaded result moved to a case-insensitive disk (USB stick / cloud
// sync) doesn't lose one of the two files. Normalization is via
// strings.ToLower on the dedup key only — display Name keeps the
// original case.
func TestDownloadAttachments_DedupIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/handler/download/file-A", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first"))
	})
	mux.HandleFunc("/handler/download/file-B", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("second"))
	})
	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(msgJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	msgJSON = buildMessageEditJSON(t, 1, []messageEditAttachment{
		{ID: "1@a", Name: "Report.PDF", Link: srv.URL + "/handler/download/file-A"},
		{ID: "1@b", Name: "report.pdf", Link: srv.URL + "/handler/download/file-B"},
	})
	cli := buildClient(t, srv)
	destDir := t.TempDir()

	res, err := DownloadAttachments(context.Background(), cli, "m-1", DownloadOptions{DestDir: destDir})
	if err != nil {
		t.Fatalf("DownloadAttachments: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("Files = %d, want 2", len(res.Files))
	}
	// Display Name preserves the original Edookit-supplied casing.
	if res.Files[0].Name != "Report.PDF" || res.Files[1].Name != "report.pdf" {
		t.Errorf("Names = %q / %q, want original casing preserved", res.Files[0].Name, res.Files[1].Name)
	}
	// But the on-disk paths must differ even after case-folding so
	// case-insensitive filesystems don't alias them to the same file.
	base0 := strings.ToLower(filepath.Base(res.Files[0].Path))
	base1 := strings.ToLower(filepath.Base(res.Files[1].Path))
	if base0 == base1 {
		t.Errorf("on-disk filenames %q and %q would alias on a case-insensitive filesystem", res.Files[0].Path, res.Files[1].Path)
	}
}
