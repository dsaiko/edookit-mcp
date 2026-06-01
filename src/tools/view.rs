//! `view_attachment` — returns an attachment as inline content blocks (no file
//! written). Port of Go's `internal/tools/view.go`. Images come back downscaled;
//! PDFs as extracted text + rasterized page images; text-like files as content.

use std::path::Path;

use anyhow::anyhow;
use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;

use super::htmlutil::truncate_runes;
use super::message::get_message;
use super::pdfrender::rasterize_pdf;
use crate::client::{Client, ClientError};

const DEFAULT_VIEW_MAX_MB: i64 = 8;
const MAX_VIEW_MAX_MB: i64 = 25;
/// Longest-edge pixel size we bother sending — Claude downsamples larger images
/// server-side anyway, so shrinking first saves tokens.
const MAX_IMAGE_DIM: u32 = 1568;
const MAX_VIEW_TEXT_RUNES: usize = 50_000;
const DEFAULT_VIEW_MAX_PAGES: i64 = 5;
const MAX_VIEW_MAX_PAGES: i64 = 20;

const MIME_PDF: &str = "application/pdf";
const MIME_OCTET: &str = "application/octet-stream";

/// One transport-neutral content block. The MCP layer maps `Image` to image
/// content and `Text` to text content (keeps rmcp types out of this module).
#[derive(Debug, Clone, PartialEq)]
pub enum ViewBlock {
    Text(String),
    Image { b64: String, mime: String },
}

#[derive(Debug, Clone, Default)]
pub struct ViewResult {
    pub blocks: Vec<ViewBlock>,
}

#[derive(Debug, Default, Clone)]
pub struct ViewOptions {
    pub max_size_mb: i64,
    pub max_pages: i64,
}

/// Fetches one attachment and returns it as inline content blocks.
pub async fn view_attachment(
    cli: &Client,
    message_id: &str,
    attachment_id: &str,
    opts: ViewOptions,
) -> anyhow::Result<ViewResult> {
    let msg = get_message(cli, message_id).await?;
    let att = msg
        .attachments
        .iter()
        .find(|a| a.id == attachment_id)
        .ok_or_else(|| {
            anyhow!(
                "attachment {attachment_id:?} not found on message {message_id} (it has {} attachment(s); use edookit_get_message to list their ids)",
                msg.attachments.len()
            )
        })?;

    let limit_mb = clamp_i64(opts.max_size_mb, DEFAULT_VIEW_MAX_MB, MAX_VIEW_MAX_MB);
    let limit = (limit_mb as u64) * 1024 * 1024;

    let (body, ctype) = match cli.get_bytes(&att.url, limit).await {
        Ok(x) => x,
        Err(ClientError::AttachmentTooLarge) => {
            return Ok(ViewResult {
                blocks: vec![ViewBlock::Text(format!(
                    "Příloha {:?} přesahuje {limit_mb} MB — pro inline zobrazení je příliš velká. Použij edookit_download_attachments a otevři ji lokálně.",
                    att.name
                ))],
            });
        }
        Err(e) => return Err(e.into()),
    };

    let mime = resolve_mime(&ctype, &att.name);
    let mut blocks = vec![ViewBlock::Text(format!("Příloha: {} ({}, {} B)", att.name, mime, body.len()))];

    if let Some(rest) = mime.strip_prefix("image/") {
        let _ = rest;
        let (b64, out_mime) = encode_image_for_view(&body, &mime);
        blocks.push(ViewBlock::Image { b64, mime: out_mime });
    } else if mime == MIME_PDF {
        blocks.extend(pdf_blocks(&body, opts.max_pages).await);
    } else if is_text_like(&mime, &att.name) {
        let content = truncate_runes(&String::from_utf8_lossy(&body), MAX_VIEW_TEXT_RUNES);
        blocks.push(ViewBlock::Text(format!("--- obsah ---\n{content}")));
    } else {
        blocks.push(ViewBlock::Text(format!(
            "Binární typ ({mime}) — inline nezobrazitelný. Použij edookit_download_attachments."
        )));
    }
    Ok(ViewResult { blocks })
}

fn clamp_i64(v: i64, default: i64, max: i64) -> i64 {
    if v <= 0 {
        default
    } else if v > max {
        max
    } else {
        v
    }
}

/// Prefers the server Content-Type but falls back to the filename extension
/// when the server is unhelpful (empty or the generic octet-stream).
fn resolve_mime(content_type: &str, filename: &str) -> String {
    let mt = parse_media_type(content_type);
    if !mt.is_empty() && mt != MIME_OCTET {
        return mt;
    }
    if let Some(ext) = Path::new(filename).extension().and_then(|e| e.to_str()).map(|e| e.to_ascii_lowercase()) {
        if let Some(m) = mime_guess::from_ext(&ext).first_raw() {
            return m.split(';').next().unwrap_or(m).trim().to_ascii_lowercase();
        }
        match ext.as_str() {
            "ics" => return "text/calendar".to_string(),
            "md" => return "text/markdown".to_string(),
            _ => {}
        }
    }
    if !mt.is_empty() {
        return mt; // the octet-stream we skipped above, as a last resort
    }
    MIME_OCTET.to_string()
}

fn parse_media_type(v: &str) -> String {
    if v.is_empty() {
        return String::new();
    }
    v.split(';').next().unwrap_or("").trim().to_ascii_lowercase()
}

/// Whether the attachment is plain-text-ish and safe to inline as text.
fn is_text_like(mime: &str, filename: &str) -> bool {
    if mime.starts_with("text/") {
        return true;
    }
    if matches!(mime, "application/json" | "application/xml" | "application/x-yaml" | "application/yaml") {
        return true;
    }
    matches!(
        Path::new(filename).extension().and_then(|e| e.to_str()).map(|e| e.to_ascii_lowercase()).as_deref(),
        Some("txt" | "csv" | "tsv" | "md" | "json" | "xml" | "ics" | "html" | "htm" | "log" | "yaml" | "yml")
    )
}

/// PDF → text layer (if any) + the first `max_pages` pages rasterized to PNG.
async fn pdf_blocks(body: &[u8], max_pages: i64) -> Vec<ViewBlock> {
    let max_pages = clamp_i64(max_pages, DEFAULT_VIEW_MAX_PAGES, MAX_VIEW_MAX_PAGES) as usize;
    let mut blocks = Vec::new();

    let text = extract_pdf_text(body);
    let text = text.trim();
    if !text.is_empty() {
        blocks.push(ViewBlock::Text(format!("--- text PDF ---\n{}", truncate_runes(text, MAX_VIEW_TEXT_RUNES))));
    }

    match rasterize_pdf(body, max_pages).await {
        Ok((pngs, total)) => {
            for (i, png) in pngs.iter().enumerate() {
                blocks.push(ViewBlock::Text(format!("--- strana {}/{} ---", i + 1, total)));
                blocks.push(ViewBlock::Image { b64: B64.encode(png), mime: "image/png".to_string() });
            }
            if total > pngs.len() {
                blocks.push(ViewBlock::Text(format!(
                    "(Zobrazeno prvních {} z {} stran. Pro zbytek zvyš max_pages nebo použij edookit_download_attachments.)",
                    pngs.len(),
                    total
                )));
            }
        }
        Err(e) => tracing::warn!("pdf rasterize failed: {e}"),
    }

    if blocks.is_empty() {
        blocks.push(ViewBlock::Text(
            "PDF se nepodařilo zobrazit ani z něj získat text. Použij edookit_download_attachments.".to_string(),
        ));
    }
    blocks
}

/// Pulls the text layer from a PDF. "" for image-only PDFs or parse failures.
/// pdf-extract can panic on malformed input, so the panic is contained.
fn extract_pdf_text(body: &[u8]) -> String {
    std::panic::catch_unwind(|| pdf_extract::extract_text_from_mem(body).unwrap_or_default())
        .unwrap_or_default()
}

/// Returns base64 image data + MIME, downscaling first if larger than
/// `MAX_IMAGE_DIM` on its longest edge. Falls back to the original bytes on any
/// decode/encode trouble.
fn encode_image_for_view(body: &[u8], mime: &str) -> (String, String) {
    if let Some((resized, rmime)) = downscale_image(body) {
        return (B64.encode(&resized), rmime);
    }
    (B64.encode(body), mime.to_string())
}

/// Decodes `body`, and if its longest edge exceeds `MAX_IMAGE_DIM`, scales it
/// down and re-encodes (JPEG for JPEG sources, PNG otherwise). `None` when no
/// resize happened or the image couldn't be processed.
fn downscale_image(body: &[u8]) -> Option<(Vec<u8>, String)> {
    let format = image::guess_format(body).ok()?;
    let img = image::load_from_memory(body).ok()?;
    let (w, h) = (img.width(), img.height());
    if w.max(h) <= MAX_IMAGE_DIM {
        return None; // already small enough
    }
    // Pin the longest edge to MAX_IMAGE_DIM exactly, preserving aspect.
    let (nw, nh) = if w >= h {
        (MAX_IMAGE_DIM, (h as f64 * MAX_IMAGE_DIM as f64 / w as f64) as u32)
    } else {
        ((w as f64 * MAX_IMAGE_DIM as f64 / h as f64) as u32, MAX_IMAGE_DIM)
    };
    let resized = img.resize_exact(nw.max(1), nh.max(1), image::imageops::FilterType::CatmullRom);

    let mut buf = Vec::new();
    let out_format = if format == image::ImageFormat::Jpeg {
        image::ImageFormat::Jpeg
    } else {
        image::ImageFormat::Png
    };
    resized.write_to(&mut std::io::Cursor::new(&mut buf), out_format).ok()?;
    let mime = if out_format == image::ImageFormat::Jpeg { "image/jpeg" } else { "image/png" };
    Some((buf, mime.to_string()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn resolve_mime_prefers_server_then_extension() {
        assert_eq!(resolve_mime("image/png", "x.png"), "image/png");
        // octet-stream falls back to extension
        assert_eq!(resolve_mime("application/octet-stream", "x.pdf"), "application/pdf");
        assert_eq!(resolve_mime("", "notes.txt"), "text/plain");
        assert_eq!(resolve_mime("", "cal.ics"), "text/calendar");
        assert_eq!(resolve_mime("", "blob"), "application/octet-stream");
    }

    #[test]
    fn is_text_like_cases() {
        assert!(is_text_like("text/plain", "x.txt"));
        assert!(is_text_like("application/json", "x"));
        assert!(is_text_like("application/octet-stream", "data.csv"));
        assert!(!is_text_like("image/png", "x.png"));
        assert!(!is_text_like("application/pdf", "x.pdf"));
        assert!(!is_text_like("application/zip", "x.zip"));
    }

    fn png_bytes(w: u32, h: u32) -> Vec<u8> {
        let img = image::DynamicImage::ImageRgb8(image::RgbImage::new(w, h));
        let mut buf = Vec::new();
        img.write_to(&mut std::io::Cursor::new(&mut buf), image::ImageFormat::Png).unwrap();
        buf
    }

    #[test]
    fn downscale_large_image_pins_longest_edge() {
        let (out, mime) = downscale_image(&png_bytes(3000, 1000)).expect("should downscale");
        assert_eq!(mime, "image/png");
        let decoded = image::load_from_memory(&out).unwrap();
        assert_eq!(decoded.width().max(decoded.height()), MAX_IMAGE_DIM);
    }

    #[test]
    fn downscale_small_image_is_noop() {
        assert!(downscale_image(&png_bytes(100, 80)).is_none());
    }

    #[test]
    fn downscale_non_image_is_none() {
        assert!(downscale_image(b"not an image").is_none());
    }

    #[test]
    fn clamp_view_knobs() {
        assert_eq!(clamp_i64(0, DEFAULT_VIEW_MAX_MB, MAX_VIEW_MAX_MB), DEFAULT_VIEW_MAX_MB);
        assert_eq!(clamp_i64(100, DEFAULT_VIEW_MAX_MB, MAX_VIEW_MAX_MB), MAX_VIEW_MAX_MB);
        assert_eq!(clamp_i64(10, DEFAULT_VIEW_MAX_MB, MAX_VIEW_MAX_MB), 10);
    }
}
