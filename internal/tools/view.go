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
)

const (
	mimePDF         = "application/pdf"
	mimeOctetStream = "application/octet-stream"
	mimeJPEG        = "image/jpeg"
	mimePNG         = "image/png"
)

// ViewBlock is one transport-neutral content block. The MCP layer (main.go)
// maps it by precedence: a ResourceB64 block → an embedded resource (raw file
// blob), an ImageB64 block → image content, otherwise text content. Keeping
// mcp-go types out of this package preserves the transport-agnostic boundary.
type ViewBlock struct {
	Text         string
	ImageB64     string
	ImageMime    string
	ResourceB64  string // base64 of the raw file, surfaced as an embedded resource
	ResourceMime string
	ResourceName string // original filename, used to build the resource URI
}

// ViewResult is the ordered list of content blocks for one viewed attachment.
type ViewResult struct {
	Blocks []ViewBlock
}

// ViewOptions carries the optional caller knobs for ViewAttachment.
type ViewOptions struct {
	MaxSizeMB int // 0 → default; clamped to maxViewMaxMB
}

func textBlock(s string) ViewBlock              { return ViewBlock{Text: s} }
func imageBlock(b64, mimeType string) ViewBlock { return ViewBlock{ImageB64: b64, ImageMime: mimeType} }

// rawResourceBlock carries the original file bytes as an embedded resource so a
// client that supports it can offer/render the actual file (e.g. the PDF),
// rather than only the extracted text. Whether a given MCP client surfaces it
// is client-dependent.
func rawResourceBlock(body []byte, mimeType, name string) ViewBlock {
	return ViewBlock{
		ResourceB64:  base64.StdEncoding.EncodeToString(body),
		ResourceMime: mimeType,
		ResourceName: name,
	}
}

// ViewAttachment fetches one attachment and returns it as inline content blocks
// the MCP client can render directly (no file written to disk). Images come
// back as image content (downscaled past maxImageDim); PDFs as their extracted
// text; text-like files as their decoded body. Anything else (Office docs,
// image-only/scanned PDFs, unknown binary) returns a short note pointing at
// edookit_download_attachments, which remains the way to get the raw file.
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
		if text := extractPDFText(body); strings.TrimSpace(text) != "" {
			res.Blocks = append(res.Blocks, textBlock("--- text PDF ---\n"+truncateRunes(text, maxViewTextRunes)))
		} else {
			res.Blocks = append(res.Blocks, textBlock(
				"PDF nemá čitelnou textovou vrstvu (pravděpodobně sken nebo samé obrázky). "+
					"Surové PDF je přiloženo jako soubor (pokud ho tvůj klient zobrazí); "+
					"jinak použij edookit_download_attachments."))
		}
		// Attach the raw PDF so a capable client can render/offer the file itself.
		res.Blocks = append(res.Blocks, rawResourceBlock(body, mimeType, att.Name))

	case isTextLike(mimeType, att.Name):
		res.Blocks = append(res.Blocks, textBlock("--- obsah ---\n"+truncateRunes(string(body), maxViewTextRunes)))

	default:
		res.Blocks = append(res.Blocks,
			textBlock(fmt.Sprintf(
				"Binární typ (%s) — inline nezobrazitelný. Surový soubor je přiložen jako resource "+
					"(pokud ho tvůj klient zobrazí); jinak použij edookit_download_attachments.", mimeType)),
			rawResourceBlock(body, mimeType, att.Name))
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
