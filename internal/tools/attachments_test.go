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
	// Failed download must not leave a partial file behind.
	if _, err := os.Stat(broken.Path); !os.IsNotExist(err) {
		t.Errorf("broken file still exists on disk: %v", err)
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
