package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- pure helpers ----------

func TestResolveMime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, contentType, filename, want string
	}{
		{"explicit image type", "image/jpeg", "x", "image/jpeg"},
		{"strips charset param", "text/plain; charset=utf-8", "x", "text/plain"},
		{"octet-stream falls back to extension", "application/octet-stream", "a.png", "image/png"},
		{"empty falls back to extension", "", "schedule.pdf", "application/pdf"},
		{"ics extension extra", "application/octet-stream", "event.ics", "text/calendar"},
		{"unknown keeps octet-stream", "application/octet-stream", "a.bin", "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveMime(tc.contentType, tc.filename); got != tc.want {
				t.Errorf("resolveMime(%q,%q) = %q, want %q", tc.contentType, tc.filename, got, tc.want)
			}
		})
	}
}

func TestIsTextLike(t *testing.T) {
	t.Parallel()
	yes := []struct{ mime, name string }{
		{"text/plain", "a.txt"},
		{"application/json", "a.json"},
		{"application/octet-stream", "data.csv"}, // by extension
		{"application/octet-stream", "cal.ics"},
	}
	no := []struct{ mime, name string }{
		{"image/png", "a.png"},
		{"application/pdf", "a.pdf"},
		{"application/zip", "a.zip"},
	}
	for _, c := range yes {
		if !isTextLike(c.mime, c.name) {
			t.Errorf("isTextLike(%q,%q) = false, want true", c.mime, c.name)
		}
	}
	for _, c := range no {
		if isTextLike(c.mime, c.name) {
			t.Errorf("isTextLike(%q,%q) = true, want false", c.mime, c.name)
		}
	}
}

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestDownscaleImage(t *testing.T) {
	t.Parallel()

	t.Run("oversized is scaled to maxImageDim", func(t *testing.T) {
		t.Parallel()
		out, mimeType, ok := downscaleImage(makePNG(t, 3000, 1000))
		if !ok {
			t.Fatal("expected downscale, got ok=false")
		}
		if mimeType != "image/png" {
			t.Errorf("mime = %q, want image/png", mimeType)
		}
		img, _, err := image.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("decode resized: %v", err)
		}
		longest := img.Bounds().Dx()
		if img.Bounds().Dy() > longest {
			longest = img.Bounds().Dy()
		}
		if longest != maxImageDim {
			t.Errorf("longest edge = %d, want %d", longest, maxImageDim)
		}
	})

	t.Run("small image is left alone", func(t *testing.T) {
		t.Parallel()
		if _, _, ok := downscaleImage(makePNG(t, 100, 80)); ok {
			t.Error("expected ok=false (no resize) for a small image")
		}
	})

	t.Run("extreme aspect ratio keeps a non-zero short edge", func(t *testing.T) {
		t.Parallel()
		out, _, ok := downscaleImage(makePNG(t, 5000, 1)) // short edge would truncate to 0
		if !ok {
			t.Fatal("expected downscale, got ok=false")
		}
		img, _, err := image.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("decode resized: %v", err)
		}
		if img.Bounds().Dy() < 1 || img.Bounds().Dx() < 1 {
			t.Errorf("resized dims = %dx%d, want both >= 1", img.Bounds().Dx(), img.Bounds().Dy())
		}
	})

	t.Run("non-image bytes are left alone", func(t *testing.T) {
		t.Parallel()
		if _, _, ok := downscaleImage([]byte("not an image")); ok {
			t.Error("expected ok=false for non-image bytes")
		}
	})
}

func TestExtractPDFText_InvalidReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := extractPDFText([]byte("%PDF-1.4 not really a pdf")); got != "" {
		t.Errorf("extractPDFText(invalid) = %q, want empty", got)
	}
}

// ---------- ViewAttachment end-to-end ----------

// viewAttachmentServer wires a test server that serves a message-edit JSON
// carrying one attachment (name/id) whose download endpoint returns body with
// contentType. The attachment Link is absolute (srv.URL + path) so the client's
// same-origin preflight passes.
func viewAttachmentServer(t *testing.T, name, id, contentType string, body []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("warmup ok")) })
	mux.HandleFunc("/handler/download/file", func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write(body)
	})
	var msgJSON []byte
	mux.HandleFunc("/handler/page/message-edit", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(msgJSON)
	})
	srv := httptest.NewServer(mux)
	msgJSON = buildMessageEditJSON(t, 555, []messageEditAttachment{
		{ID: id, Name: name, Link: srv.URL + "/handler/download/file"},
	})
	return srv
}

func TestViewAttachment_Image(t *testing.T) {
	t.Parallel()

	srv := viewAttachmentServer(t, "photo.png", "1@img", "image/png", makePNG(t, 20, 20))
	defer srv.Close()

	cli := buildClient(t, srv)
	res, err := ViewAttachment(context.Background(), cli, "m-555", "1@img", ViewOptions{})
	if err != nil {
		t.Fatalf("ViewAttachment: %v", err)
	}
	if len(res.Blocks) != 2 {
		t.Fatalf("got %d blocks, want 2 (header + image)", len(res.Blocks))
	}
	if !strings.Contains(res.Blocks[0].Text, "image/png") || !strings.Contains(res.Blocks[0].Text, "photo.png") {
		t.Errorf("header block = %q, want it to mention photo.png and image/png", res.Blocks[0].Text)
	}
	if res.Blocks[1].ImageB64 == "" || res.Blocks[1].ImageMime != "image/png" {
		t.Errorf("image block = %+v, want non-empty base64 with image/png", res.Blocks[1])
	}
	if _, err := base64.StdEncoding.DecodeString(res.Blocks[1].ImageB64); err != nil {
		t.Errorf("image block is not valid base64: %v", err)
	}
}

func TestViewAttachment_TextFile(t *testing.T) {
	t.Parallel()
	const content = "name,grade\nNěmec,1\nTůmová,2\n"

	srv := viewAttachmentServer(t, "grades.csv", "1@csv", "text/csv", []byte(content))
	defer srv.Close()

	cli := buildClient(t, srv)
	res, err := ViewAttachment(context.Background(), cli, "m-555", "1@csv", ViewOptions{})
	if err != nil {
		t.Fatalf("ViewAttachment: %v", err)
	}
	if len(res.Blocks) != 2 {
		t.Fatalf("got %d blocks, want 2 (header + content)", len(res.Blocks))
	}
	if !strings.Contains(res.Blocks[1].Text, "Němec,1") {
		t.Errorf("content block = %q, want it to contain the CSV body", res.Blocks[1].Text)
	}
}

func TestViewAttachment_TooLarge(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("x"), 1024*1024+512) // just over 1 MB

	srv := viewAttachmentServer(t, "huge.pdf", "1@big", "application/pdf", big)
	defer srv.Close()

	cli := buildClient(t, srv)
	res, err := ViewAttachment(context.Background(), cli, "m-555", "1@big", ViewOptions{MaxSizeMB: 1})
	if err != nil {
		t.Fatalf("ViewAttachment: %v", err)
	}
	if len(res.Blocks) != 1 {
		t.Fatalf("got %d blocks, want 1 (the too-large note)", len(res.Blocks))
	}
	if !strings.Contains(res.Blocks[0].Text, "přesahuje") || !strings.Contains(res.Blocks[0].Text, "edookit_download_attachments") {
		t.Errorf("block = %q, want a too-large note pointing at the download tool", res.Blocks[0].Text)
	}
}

func TestViewAttachment_UnknownBinary(t *testing.T) {
	t.Parallel()

	srv := viewAttachmentServer(t, "bundle.zip", "1@zip", "application/zip", []byte("PK\x03\x04 not really"))
	defer srv.Close()

	cli := buildClient(t, srv)
	res, err := ViewAttachment(context.Background(), cli, "m-555", "1@zip", ViewOptions{})
	if err != nil {
		t.Fatalf("ViewAttachment: %v", err)
	}
	if len(res.Blocks) != 2 || !strings.Contains(res.Blocks[1].Text, "edookit_download_attachments") {
		t.Errorf("blocks = %+v, want a 'use download' note for binary type", res.Blocks)
	}
}

func TestViewAttachment_NotFound(t *testing.T) {
	t.Parallel()

	srv := viewAttachmentServer(t, "real.pdf", "1@real", "application/pdf", []byte("%PDF-1.4"))
	defer srv.Close()

	cli := buildClient(t, srv)
	_, err := ViewAttachment(context.Background(), cli, "m-555", "1@missing", ViewOptions{})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want a not-found error", err)
	}
}
