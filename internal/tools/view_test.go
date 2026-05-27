package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// buildTestPDF assembles a well-formed PDF with `pages` blank 144x144 pages
// (all sharing one content stream), including a correct xref table and
// startxref offset. Building it with computed offsets — rather than a
// hand-written literal — means the render tests exercise PDFium's normal
// parse path, not its lenient repair fallback.
func buildTestPDF(t *testing.T, pages int) []byte {
	t.Helper()
	const content = "1 0 0 RG 10 10 m 134 134 l S\n"
	contentObj := 3 + pages // catalog=1, pages=2, page objs=3..2+pages, content=last

	bodies := make([]string, 0, pages+3) // catalog + pages + N page objs + content
	bodies = append(bodies, "<</Type/Catalog/Pages 2 0 R>>")
	kids := ""
	for i := range pages {
		kids += fmt.Sprintf("%d 0 R ", 3+i)
	}
	bodies = append(bodies, fmt.Sprintf("<</Type/Pages/Count %d/Kids[%s]>>", pages, strings.TrimSpace(kids)))
	for range pages {
		bodies = append(bodies, fmt.Sprintf("<</Type/Page/Parent 2 0 R/MediaBox[0 0 144 144]/Resources<<>>/Contents %d 0 R>>", contentObj))
	}
	bodies = append(bodies, fmt.Sprintf("<</Length %d>>stream\n%sendstream", len(content), content))

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(bodies)+1)
	for i, body := range bodies {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj%s\nendobj\n", i+1, body)
	}
	xrefOff := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(bodies)+1)
	for i := 1; i <= len(bodies); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF", len(bodies)+1, xrefOff)
	return buf.Bytes()
}

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
	// header + download note (no inline content for unknown binary)
	if len(res.Blocks) != 2 {
		t.Fatalf("got %d blocks, want 2 (header + note)", len(res.Blocks))
	}
	if !strings.Contains(res.Blocks[1].Text, "edookit_download_attachments") {
		t.Errorf("note block = %q, want a 'use download' fallback", res.Blocks[1].Text)
	}
}

func TestViewAttachment_PDFInvalidFallsBack(t *testing.T) {
	t.Parallel()

	// Not a real PDF: no text layer and rendering fails → a download note, no
	// image blocks.
	srv := viewAttachmentServer(t, "schedule.pdf", "1@pdf", "application/pdf", []byte("%PDF-1.4 not really a pdf"))
	defer srv.Close()

	cli := buildClient(t, srv)
	res, err := ViewAttachment(context.Background(), cli, "m-555", "1@pdf", ViewOptions{})
	if err != nil {
		t.Fatalf("ViewAttachment: %v", err)
	}
	for _, b := range res.Blocks {
		if b.ImageB64 != "" {
			t.Fatalf("invalid PDF should not produce image blocks, got %+v", res.Blocks)
		}
	}
	if !strings.Contains(res.Blocks[len(res.Blocks)-1].Text, "edookit_download_attachments") {
		t.Errorf("last block = %q, want a download fallback note", res.Blocks[len(res.Blocks)-1].Text)
	}
}

func TestViewAttachment_PDFRendersToImages(t *testing.T) {
	t.Parallel()

	srv := viewAttachmentServer(t, "schedule.pdf", "1@pdf", "application/pdf", buildTestPDF(t, 1))
	defer srv.Close()

	cli := buildClient(t, srv)
	res, err := ViewAttachment(context.Background(), cli, "m-555", "1@pdf", ViewOptions{})
	if err != nil {
		t.Fatalf("ViewAttachment: %v", err)
	}
	var images int
	for _, b := range res.Blocks {
		if b.ImageB64 == "" {
			continue
		}
		images++
		if b.ImageMime != "image/png" {
			t.Errorf("rendered page mime = %q, want image/png", b.ImageMime)
		}
		dec, derr := base64.StdEncoding.DecodeString(b.ImageB64)
		if derr != nil {
			t.Errorf("page image is not valid base64: %v", derr)
			continue
		}
		if _, _, ierr := image.Decode(bytes.NewReader(dec)); ierr != nil {
			t.Errorf("page image does not decode: %v", ierr)
		}
	}
	if images < 1 {
		t.Fatalf("expected at least one rendered page image, got blocks %+v", res.Blocks)
	}
}

func TestViewAttachment_PDFMultiPageRespectsMaxPages(t *testing.T) {
	t.Parallel()

	srv := viewAttachmentServer(t, "list.pdf", "1@pdf", "application/pdf", buildTestPDF(t, 3))
	defer srv.Close()

	cli := buildClient(t, srv)
	res, err := ViewAttachment(context.Background(), cli, "m-555", "1@pdf", ViewOptions{MaxPages: 2})
	if err != nil {
		t.Fatalf("ViewAttachment: %v", err)
	}

	var images int
	var sawPage1, sawPage2, sawCapNote bool
	for _, b := range res.Blocks {
		if b.ImageB64 != "" {
			images++
			if b.ImageMime != "image/png" {
				t.Errorf("page mime = %q, want image/png", b.ImageMime)
			}
			continue
		}
		switch {
		case strings.Contains(b.Text, "strana 1/3"):
			sawPage1 = true
		case strings.Contains(b.Text, "strana 2/3"):
			sawPage2 = true
		case strings.Contains(b.Text, "2 z 3"):
			sawCapNote = true
		}
	}

	if images != 2 {
		t.Errorf("got %d image blocks, want 2 (capped by max_pages)", images)
	}
	if !sawPage1 || !sawPage2 {
		t.Errorf("expected per-page labels 'strana 1/3' and 'strana 2/3'; blocks=%+v", res.Blocks)
	}
	if !sawCapNote {
		t.Errorf("expected a cap note mentioning '2 z 3' pages; blocks=%+v", res.Blocks)
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
