package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register GIF decoder for image.Decode
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"mime"
	"path/filepath"
	"strings"

	"github.com/ledongthuc/pdf"
	xdraw "golang.org/x/image/draw"

	"github.com/dsaiko/edookit-mcp/internal/client"
)

const (
	// defaultViewMaxMB caps the inline body by default; base64 in the response
	// inflates ~33% and large blobs risk client/context limits.
	defaultViewMaxMB = 8
	// maxViewMaxMB is the hard ceiling regardless of a caller's max_size_mb.
	maxViewMaxMB = 25
	// maxImageDim is the longest-edge pixel size we bother sending — Claude
	// downsamples larger images server-side anyway, so shrinking first saves
	// tokens and bandwidth.
	maxImageDim = 1568
	// maxViewTextRunes bounds extracted PDF / text-file content so a huge file
	// can't blow the context window. Text is cheap per token but not free.
	maxViewTextRunes = 50_000
	// defaultViewMaxPages / maxViewMaxPages bound how many PDF pages we
	// rasterize into image blocks (each image is expensive in tokens).
	defaultViewMaxPages = 5
	maxViewMaxPages     = 20
)

const (
	mimePDF         = "application/pdf"
	mimeOctetStream = "application/octet-stream"
	mimeJPEG        = "image/jpeg"
	mimePNG         = "image/png"
)

// ViewBlock is one transport-neutral content block. The MCP layer (main.go)
// maps an ImageB64 block to image content and otherwise to text content.
// Keeping mcp-go types out of this package preserves the transport-agnostic
// boundary.
type ViewBlock struct {
	Text      string
	ImageB64  string
	ImageMime string
}

// ViewResult is the ordered list of content blocks for one viewed attachment.
type ViewResult struct {
	Blocks []ViewBlock
}

// ViewOptions carries the optional caller knobs for ViewAttachment.
type ViewOptions struct {
	MaxSizeMB int // 0 → default; clamped to maxViewMaxMB
	MaxPages  int // PDF: max pages to rasterize; 0 → default; clamped to maxViewMaxPages
}

func textBlock(s string) ViewBlock              { return ViewBlock{Text: s} }
func imageBlock(b64, mimeType string) ViewBlock { return ViewBlock{ImageB64: b64, ImageMime: mimeType} }

// ViewAttachment fetches one attachment and returns it as inline content blocks
// the MCP client can render directly (no file written to disk). Images come
// back as image content (downscaled past maxImageDim); PDFs as their extracted
// text plus the first pages rasterized to PNG images so the page is actually
// visible; text-like files as their decoded body. Office docs and other binary
// types return a short note pointing at edookit_download_attachments, which
// remains the way to get the raw file.
func ViewAttachment(ctx context.Context, cli *client.Client, messageID, attachmentID string, opts ViewOptions) (*ViewResult, error) {
	msg, err := GetMessage(ctx, cli, messageID)
	if err != nil {
		return nil, err
	}

	var att *Attachment
	for i := range msg.Attachments {
		if msg.Attachments[i].ID == attachmentID {
			att = &msg.Attachments[i]
			break
		}
	}
	if att == nil {
		return nil, fmt.Errorf("attachment %q not found on message %s (it has %d attachment(s); use edookit_get_message to list their ids)",
			attachmentID, messageID, len(msg.Attachments))
	}

	limitMB := opts.MaxSizeMB
	if limitMB <= 0 {
		limitMB = defaultViewMaxMB
	}
	if limitMB > maxViewMaxMB {
		limitMB = maxViewMaxMB
	}
	limit := int64(limitMB) * 1024 * 1024

	body, ctype, err := cli.GetBytes(ctx, att.URL, limit)
	if errors.Is(err, client.ErrAttachmentTooLarge) {
		return &ViewResult{Blocks: []ViewBlock{textBlock(fmt.Sprintf(
			"Příloha %q přesahuje %d MB — pro inline zobrazení je příliš velká. Použij edookit_download_attachments a otevři ji lokálně.",
			att.Name, limitMB))}}, nil
	}
	if err != nil {
		return nil, err
	}

	mimeType := resolveMime(ctype, att.Name)
	res := &ViewResult{Blocks: []ViewBlock{
		textBlock(fmt.Sprintf("Příloha: %s (%s, %d B)", att.Name, mimeType, len(body))),
	}}

	switch {
	case strings.HasPrefix(mimeType, "image/"):
		b64, outMime := encodeImageForView(body, mimeType)
		res.Blocks = append(res.Blocks, imageBlock(b64, outMime))

	case mimeType == mimePDF:
		res.Blocks = append(res.Blocks, pdfBlocks(ctx, body, opts.MaxPages)...)

	case isTextLike(mimeType, att.Name):
		res.Blocks = append(res.Blocks, textBlock("--- obsah ---\n"+truncateRunes(string(body), maxViewTextRunes)))

	default:
		res.Blocks = append(res.Blocks, textBlock(fmt.Sprintf(
			"Binární typ (%s) — inline nezobrazitelný. Použij edookit_download_attachments.", mimeType)))
	}
	return res, nil
}

// resolveMime prefers the server Content-Type but falls back to the filename
// extension when the server is unhelpful (empty or the generic
// application/octet-stream that Edookit sometimes returns for downloads).
func resolveMime(contentType, filename string) string {
	if mt := parseMediaType(contentType); mt != "" && mt != mimeOctetStream {
		return mt
	}
	if ext := strings.ToLower(filepath.Ext(filename)); ext != "" {
		if mt := parseMediaType(mime.TypeByExtension(ext)); mt != "" {
			return mt
		}
		switch ext { // a few that mime.TypeByExtension misses on some hosts
		case ".ics":
			return "text/calendar"
		case ".md":
			return "text/markdown"
		}
	}
	if mt := parseMediaType(contentType); mt != "" {
		return mt // the octet-stream we skipped above, as a last resort
	}
	return mimeOctetStream
}

func parseMediaType(v string) string {
	if v == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	return strings.ToLower(mt)
}

// isTextLike reports whether the attachment is plain-text-ish and safe to inline
// as text — by MIME family or, when the MIME is unhelpful, by extension.
func isTextLike(mimeType, filename string) bool {
	if strings.HasPrefix(mimeType, "text/") {
		return true
	}
	switch mimeType {
	case "application/json", "application/xml", "application/x-yaml", "application/yaml":
		return true
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".txt", ".csv", ".tsv", ".md", ".json", ".xml", ".ics", ".html", ".htm", ".log", ".yaml", ".yml":
		return true
	}
	return false
}

// pdfBlocks turns a PDF into content blocks: its text layer (if any) plus the
// first maxPages pages rasterized to PNG so the page is actually visible. If
// rendering is unavailable (encrypted/malformed/init failure) it degrades to
// whatever text was extracted, or a download note.
func pdfBlocks(ctx context.Context, body []byte, maxPages int) []ViewBlock {
	if maxPages <= 0 {
		maxPages = defaultViewMaxPages
	}
	if maxPages > maxViewMaxPages {
		maxPages = maxViewMaxPages
	}

	var blocks []ViewBlock
	if text := strings.TrimSpace(extractPDFText(body)); text != "" {
		blocks = append(blocks, textBlock("--- text PDF ---\n"+truncateRunes(text, maxViewTextRunes)))
	}

	pngs, totalPages, err := rasterizePDF(ctx, body, maxPages)
	if err != nil {
		log.Printf("[tools] pdf rasterize failed: %v", err)
	}
	for i, p := range pngs {
		blocks = append(blocks,
			textBlock(fmt.Sprintf("--- strana %d/%d ---", i+1, totalPages)),
			imageBlock(base64.StdEncoding.EncodeToString(p), mimePNG))
	}
	// Explain why not all pages are shown — but only attribute it to max_pages
	// when rendering actually succeeded. A mid-document render failure (err != nil)
	// gets a different note: bumping max_pages wouldn't help there.
	switch {
	case err != nil && len(pngs) > 0:
		blocks = append(blocks, textBlock(fmt.Sprintf(
			"(Zobrazeno prvních %d stran; další se nepodařilo vyrenderovat. Zbytek viz text výše nebo edookit_download_attachments.)",
			len(pngs))))
	case err == nil && totalPages > len(pngs):
		blocks = append(blocks, textBlock(fmt.Sprintf(
			"(Zobrazeno prvních %d z %d stran. Pro zbytek zvyš max_pages nebo použij edookit_download_attachments.)",
			len(pngs), totalPages)))
	}
	if len(blocks) == 0 {
		blocks = append(blocks, textBlock(
			"PDF se nepodařilo zobrazit ani z něj získat text. Použij edookit_download_attachments."))
	}
	return blocks
}

// extractPDFText pulls the text layer from a PDF. Returns "" for image-only
// PDFs or anything that fails to parse. ledongthuc/pdf can panic on malformed
// input, so the panic is contained here.
func extractPDFText(body []byte) (text string) {
	defer func() {
		if recover() != nil {
			text = ""
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return ""
	}
	rd, err := r.GetPlainText()
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rd); err != nil {
		return ""
	}
	return buf.String()
}

// encodeImageForView returns base64 image data and its MIME, downscaling first
// if the image is larger than maxImageDim on its longest edge. On any decode/
// encode trouble it falls back to the original bytes unchanged.
func encodeImageForView(body []byte, mimeType string) (b64, outMime string) {
	if resized, rMime, ok := downscaleImage(body); ok {
		return base64.StdEncoding.EncodeToString(resized), rMime
	}
	return base64.StdEncoding.EncodeToString(body), mimeType
}

// downscaleImage decodes body, and if its longest edge exceeds maxImageDim,
// scales it down and re-encodes (JPEG for JPEG sources, PNG otherwise).
// Returns ok=false when no resize happened or the image couldn't be processed,
// in which case the caller sends the original bytes.
func downscaleImage(body []byte) (out []byte, outMime string, ok bool) {
	img, format, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, "", false
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	longest := w
	if h > longest {
		longest = h
	}
	if longest <= maxImageDim {
		return nil, "", false // already small enough — send as-is
	}
	// Pin the longest edge to maxImageDim exactly (scaling both via a single
	// float factor can truncate the longest edge to maxImageDim-1).
	var nw, nh int
	if w >= h {
		nw, nh = maxImageDim, int(float64(h)*float64(maxImageDim)/float64(w))
	} else {
		nw, nh = int(float64(w)*float64(maxImageDim)/float64(h)), maxImageDim
	}
	// Extreme aspect ratios (e.g. 5000x1) can truncate the short edge to 0,
	// which would make a zero-sized destination. Keep at least 1px.
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)

	var buf bytes.Buffer
	if format == "jpeg" {
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
			return nil, "", false
		}
		return buf.Bytes(), mimeJPEG, true
	}
	if err := png.Encode(&buf, dst); err != nil {
		return nil, "", false
	}
	return buf.Bytes(), mimePNG, true
}
