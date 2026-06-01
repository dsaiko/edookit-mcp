//! PDF page rasterization via PDFium (pdfium-render). Port of Go's
//! `internal/tools/pdfrender.go`.
//!
//! Unlike the Go build (which embedded PDFium as WASM via wazero for a no-cgo
//! single binary), this loads a bundled native PDFium shared library at runtime
//! — fetched by `make build` / `scripts/fetch-pdfium.sh` into
//! `third_party/pdfium/`, shipped alongside the binary in packaging. `Pdfium`
//! is `Send + Sync` but PDFium itself is single-threaded, so a global `Mutex`
//! serializes renders (mirrors Go's single-worker pool) and the work runs on a
//! blocking thread.

use std::path::PathBuf;
use std::sync::{Mutex, OnceLock};

use pdfium_render::prelude::*;

/// Longest-edge pixel bound for a rendered page — caps the bitmap PDFium
/// allocates regardless of the page's declared MediaBox.
const MAX_IMAGE_DIM: Pixels = 1568;

static PDFIUM: OnceLock<Result<Mutex<Pdfium>, String>> = OnceLock::new();

/// Lazily binds the bundled PDFium library. Compiling the binding once and
/// reusing it for the life of the process.
fn pdfium() -> Result<&'static Mutex<Pdfium>, String> {
    PDFIUM
        .get_or_init(|| {
            let path = resolve_pdfium_path();
            let bindings = Pdfium::bind_to_library(&path)
                .map_err(|e| format!("bind pdfium at {}: {e}", path.display()))?;
            Ok(Mutex::new(Pdfium::new(bindings)))
        })
        .as_ref()
        .map_err(|e| e.clone())
}

/// Resolves the PDFium library path: explicit override → next to the executable
/// (installed layout) → the vendored dev path (`third_party/pdfium/lib`).
fn resolve_pdfium_path() -> PathBuf {
    if let Ok(p) = std::env::var("EDOOKIT_PDFIUM_LIB")
        && !p.is_empty()
    {
        return PathBuf::from(p);
    }
    if let Ok(exe) = std::env::current_exe()
        && let Some(dir) = exe.parent()
    {
        let p = Pdfium::pdfium_platform_library_name_at_path(dir);
        if p.exists() {
            return p;
        }
    }
    Pdfium::pdfium_platform_library_name_at_path(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/third_party/pdfium/lib"
    ))
}

/// Renders up to `max_pages` pages of the PDF to PNG bytes (each bounded to
/// `MAX_IMAGE_DIM` on its longest edge). Returns the PNGs and the document's
/// total page count. An `Err` means rendering was unavailable for this document
/// (encrypted / malformed / lib bind failure); the caller falls back to text.
pub async fn rasterize_pdf(body: &[u8], max_pages: usize) -> Result<(Vec<Vec<u8>>, usize), String> {
    let body = body.to_vec();
    let max_pages = max_pages.max(1);
    // Rendering is CPU-bound and blocks; run it off the async runtime.
    tokio::task::spawn_blocking(move || render_blocking(&body, max_pages))
        .await
        .map_err(|e| format!("rasterize task: {e}"))?
}

fn render_blocking(body: &[u8], max_pages: usize) -> Result<(Vec<Vec<u8>>, usize), String> {
    let lock = pdfium()?;
    // Serialize: PDFium is single-threaded.
    let pdfium = lock
        .lock()
        .map_err(|_| "pdfium mutex poisoned".to_string())?;

    let doc = pdfium
        .load_pdf_from_byte_slice(body, None)
        .map_err(|e| format!("open pdf: {e}"))?;
    let pages = doc.pages();
    let total = pages.len() as usize;
    let n = total.min(max_pages);

    let config = PdfRenderConfig::new()
        .set_target_width(MAX_IMAGE_DIM)
        .set_maximum_height(MAX_IMAGE_DIM);

    let mut pngs = Vec::with_capacity(n);
    for (i, page) in pages.iter().enumerate().take(n) {
        let bitmap = page
            .render_with_config(&config)
            .map_err(|e| format!("render page {}: {e}", i + 1))?;
        let image = bitmap
            .as_image()
            .map_err(|e| format!("page {} as_image: {e}", i + 1))?;
        let mut buf = Vec::new();
        image
            .write_to(&mut std::io::Cursor::new(&mut buf), image::ImageFormat::Png)
            .map_err(|e| format!("encode png page {}: {e}", i + 1))?;
        pngs.push(buf);
    }
    Ok((pngs, total))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A valid one-page PDF (200×200 MediaBox) with a correct xref table, so
    /// PDFium loads and renders it deterministically.
    fn minimal_pdf() -> Vec<u8> {
        let mut pdf = String::new();
        let mut offsets = Vec::new();
        pdf.push_str("%PDF-1.4\n");
        offsets.push(pdf.len());
        pdf.push_str("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n");
        offsets.push(pdf.len());
        pdf.push_str("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n");
        offsets.push(pdf.len());
        pdf.push_str("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>\nendobj\n");
        let xref_off = pdf.len();
        pdf.push_str("xref\n0 4\n0000000000 65535 f \n");
        for off in &offsets {
            pdf.push_str(&format!("{off:010} 00000 n \n"));
        }
        pdf.push_str(&format!(
            "trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n{xref_off}\n%%EOF"
        ));
        pdf.into_bytes()
    }

    #[tokio::test]
    async fn rasterizes_a_real_pdf_via_bundled_pdfium() {
        let pdf = minimal_pdf();
        let (pngs, total) = rasterize_pdf(&pdf, 5)
            .await
            .expect("bundled PDFium should load + render (run `make pdfium`)");
        assert_eq!(total, 1, "one page");
        assert_eq!(pngs.len(), 1);
        // PNG magic number.
        assert_eq!(
            &pngs[0][..8],
            &[0x89, b'P', b'N', b'G', 0x0d, 0x0a, 0x1a, 0x0a]
        );
    }

    #[tokio::test]
    async fn invalid_pdf_errors() {
        assert!(rasterize_pdf(b"not a pdf at all", 5).await.is_err());
    }
}
