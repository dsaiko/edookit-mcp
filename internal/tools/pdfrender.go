package tools

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

// pdfRenderDPI is the rasterization resolution. 150 DPI is enough to read a
// scanned/printed page; oversized renders are downscaled to maxImageDim before
// being sent, so this mainly affects sharpness, not the final pixel budget.
const pdfRenderDPI = 150

// PDFium is shipped as a single WebAssembly module embedded in this binary and
// run via wazero (pure Go, no cgo, no runtime dependency). Initializing the
// pool compiles the module once, so it's done lazily and reused for the life
// of the process.
var (
	pdfPoolOnce sync.Once
	pdfPool     pdfium.Pool
	pdfPoolErr  error
)

func pdfiumPool() (pdfium.Pool, error) {
	pdfPoolOnce.Do(func() {
		pdfPool, pdfPoolErr = webassembly.Init(webassembly.Config{
			MinIdle:  0,
			MaxIdle:  1,
			MaxTotal: 1,
		})
	})
	return pdfPool, pdfPoolErr
}

// rasterizePDF renders up to maxPages pages of the PDF to PNG bytes (each
// downscaled to maxImageDim on its longest edge). It returns the rendered PNGs
// and the document's total page count so the caller can note any pages that
// were capped. An error means rendering was unavailable for this document
// (encrypted, malformed, pool init failure); the caller should fall back to
// text / a download note.
func rasterizePDF(ctx context.Context, body []byte, maxPages int) (pngs [][]byte, totalPages int, err error) {
	if maxPages < 1 {
		maxPages = 1
	}
	pool, err := pdfiumPool()
	if err != nil {
		return nil, 0, fmt.Errorf("pdfium init: %w", err)
	}
	// The pool is a single shared worker (one wasm instance is heavy on memory),
	// so concurrent renders queue here. Acquire with the call's context plus a
	// deadline so a canceled or stuck caller frees its turn instead of blocking
	// indefinitely.
	acqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	inst, err := pool.GetInstanceWithContext(acqCtx)
	if err != nil {
		return nil, 0, fmt.Errorf("pdfium instance: %w", err)
	}
	defer func() { _ = inst.Close() }()

	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &body})
	if err != nil {
		return nil, 0, fmt.Errorf("open pdf: %w", err)
	}
	defer func() { _, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document}) }()

	pc, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return nil, 0, fmt.Errorf("page count: %w", err)
	}
	totalPages = pc.PageCount

	n := totalPages
	if n > maxPages {
		n = maxPages
	}
	for i := range n {
		if err := ctx.Err(); err != nil {
			return pngs, totalPages, err // caller canceled mid-render
		}
		pngBytes, rerr := renderOnePage(inst, doc.Document, i)
		if rerr != nil {
			// Partial success is still useful — return what we have plus the error
			// context for the caller to decide.
			return pngs, totalPages, fmt.Errorf("render page %d: %w", i+1, rerr)
		}
		pngs = append(pngs, pngBytes)
	}
	return pngs, totalPages, nil
}

func renderOnePage(inst pdfium.Pdfium, doc references.FPDF_DOCUMENT, index int) ([]byte, error) {
	rp, err := inst.RenderPageInDPI(&requests.RenderPageInDPI{
		DPI:  pdfRenderDPI,
		Page: requests.Page{ByIndex: &requests.PageByIndex{Document: doc, Index: index}},
	})
	if err != nil {
		return nil, err
	}
	// In the WebAssembly runtime the rendered image holds wasm-side resources
	// that MUST be released once we've copied the pixels out.
	defer rp.Cleanup()

	var buf bytes.Buffer
	if err := png.Encode(&buf, rp.Result.Image); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	out := buf.Bytes()
	// A 150-DPI page can exceed maxImageDim; shrink it like any other image so
	// we don't waste tokens on pixels the model can't use.
	if resized, _, ok := downscaleImage(out); ok {
		out = resized
	}
	return out, nil
}
