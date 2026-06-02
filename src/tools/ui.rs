//! Experimental MCP-UI widget for the inbox listing.
//!
//! This is *additive* to the normal text/JSON tool output: `edookit_list_inbox`
//! still returns the untrusted-JSON text block (the model reasons over that),
//! and — when the operator opts in via `EDOOKIT_UI_RESOURCES` — also appends an
//! embedded `ui://` resource holding a small HTML widget. MCP-UI–capable
//! clients render that widget in a sandboxed iframe; clients that don't grok
//! `ui://` ignore it and fall back to the JSON.
//!
//! ## Convention
//!
//! We follow the MCP-UI embedded-resource convention (mcp-ui.com): a
//! `text/html` resource carried inline in the tool result's `content` array,
//! identified by a `ui://` URI. No `resources/list`+`resources/read` round-trip
//! and no server capability change is needed — the HTML travels with the
//! result. On click, the widget posts an MCP-UI `tool` action to the host
//! asking it to call [`edookit_get_message`] for the clicked id, which renders
//! the message detail. (Hosts that only support the `prompt` action need a
//! one-line change in [`CLICK_HANDLER`].)
//!
//! ## Security
//!
//! Every Edookit-derived field is third-party-controlled (see
//! [`super::untrusted`]). The widget therefore:
//!   * HTML-escapes **every** interpolated field via [`esc`] — a malicious
//!     subject/sender cannot break out into markup or attributes; and
//!   * renders only the *list metadata* already in hand and passes an opaque
//!     `id` back to the host. The message **body** is never injected into the
//!     widget DOM — the detail comes back through the normal untrusted-text
//!     path of `edookit_get_message`.

use rmcp::model::{Content, ResourceContents};

use super::messages::{ListResult, Message};

/// MCP-UI resource URI for the inbox widget.
pub const INBOX_UI_URI: &str = "ui://edookit/inbox";

/// JS run inside the widget iframe. Wires every `[data-id]` row to an MCP-UI
/// `tool` action that asks the host to call `edookit_get_message` for that id —
/// i.e. "click a row → show its detail". Kept dependency-free so it runs in a
/// bare sandboxed iframe.
const CLICK_HANDLER: &str = r#"
function openMessage(id){
  if(!id) return;
  // MCP-UI host message: request a tool call. Hosts that only support the
  // 'prompt' action can swap this for {type:'prompt',payload:{prompt:...}}.
  window.parent.postMessage(
    {type:'tool',payload:{toolName:'edookit_get_message',params:{id:id}}},'*');
}
document.addEventListener('click',function(e){
  var row=e.target.closest('[data-id]');
  if(row) openMessage(row.getAttribute('data-id'));
});
document.addEventListener('keydown',function(e){
  if(e.key!=='Enter'&&e.key!==' ') return;
  var row=e.target.closest('[data-id]');
  if(row){e.preventDefault();openMessage(row.getAttribute('data-id'));}
});
"#;

const STYLE: &str = r#"
*{box-sizing:border-box}
body{margin:0;font:14px/1.4 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;
  color:#1a1a1a;background:#fff}
h1{font-size:15px;font-weight:600;margin:0;padding:12px 16px;border-bottom:1px solid #e5e5e5;
  color:#444;position:sticky;top:0;background:#fff}
ul{list-style:none;margin:0;padding:0}
li{padding:10px 16px;border-bottom:1px solid #f0f0f0;cursor:pointer;outline:none}
li:hover,li:focus{background:#f5f8ff}
li:focus{box-shadow:inset 2px 0 0 #2563eb}
.top{display:flex;justify-content:space-between;gap:8px;margin-bottom:2px}
.sender{font-weight:600;color:#111;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.date{color:#888;font-size:12px;white-space:nowrap;flex-shrink:0}
.subject{color:#222;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.preview{color:#888;font-size:13px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.clip{color:#2563eb;font-size:12px;margin-left:6px}
.empty{padding:24px 16px;color:#888;text-align:center}
"#;

/// HTML-escapes a string for safe interpolation into both element text and
/// double-quoted attribute values.
fn esc(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 8);
    for c in s.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&#39;"),
            _ => out.push(c),
        }
    }
    out
}

/// Renders one inbox row as a clickable `<li>`. `data-id` carries the opaque
/// message id the click handler hands to `edookit_get_message`.
fn render_row(m: &Message) -> String {
    let clip = if m.attachments > 0 {
        format!(r#"<span class="clip">📎 {}</span>"#, m.attachments)
    } else {
        String::new()
    };
    let preview = if m.body_preview.is_empty() {
        String::new()
    } else {
        format!(r#"<div class="preview">{}</div>"#, esc(&m.body_preview))
    };
    format!(
        r#"<li tabindex="0" role="button" data-id="{id}">
  <div class="top"><span class="sender">{sender}</span><span class="date">{date}</span></div>
  <div class="subject">{subject}{clip}</div>
  {preview}
</li>"#,
        id = esc(&m.id),
        sender = esc(&m.sender),
        date = esc(&m.date),
        subject = esc(&m.subject),
        clip = clip,
        preview = preview,
    )
}

/// Renders the full inbox widget HTML for a [`ListResult`]. Pure function of
/// the parsed list — no network, no message bodies.
pub fn render_inbox_html(result: &ListResult) -> String {
    let body = if result.messages.is_empty() {
        r#"<div class="empty">Žádné zprávy.</div>"#.to_string()
    } else {
        let rows: String = result.messages.iter().map(render_row).collect();
        format!("<ul>{rows}</ul>")
    };
    format!(
        r#"<!doctype html>
<html lang="cs"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>{style}</style></head>
<body>
<h1>Přijaté zprávy ({count})</h1>
{body}
<script>{script}</script>
</body></html>"#,
        style = STYLE,
        count = result.messages.len(),
        body = body,
        script = CLICK_HANDLER,
    )
}

/// Builds the MCP-UI embedded-resource content block for the inbox widget.
pub fn inbox_resource(result: &ListResult) -> Content {
    Content::resource(ResourceContents::TextResourceContents {
        uri: INBOX_UI_URI.to_string(),
        mime_type: Some("text/html".to_string()),
        text: render_inbox_html(result),
        meta: None,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tools::messages::Message;

    fn msg(id: &str, sender: &str, subject: &str) -> Message {
        Message {
            id: id.to_string(),
            sender: sender.to_string(),
            subject: subject.to_string(),
            date: "2026-05-21T12:31:00+02:00".to_string(),
            ..Default::default()
        }
    }

    #[test]
    fn renders_uri_and_html_mime() {
        let r = ListResult {
            messages: vec![msg("m-1", "Učitel", "Ahoj")],
            ..Default::default()
        };
        let content = inbox_resource(&r);
        let res = content.raw.as_resource().expect("resource variant");
        match &res.resource {
            ResourceContents::TextResourceContents { uri, mime_type, .. } => {
                assert_eq!(uri, INBOX_UI_URI);
                assert_eq!(mime_type.as_deref(), Some("text/html"));
            }
            _ => panic!("expected text resource"),
        }
    }

    #[test]
    fn serializes_to_mcp_ui_embedded_resource_shape() {
        // Lock the on-the-wire JSON: MCP-UI clients look for a content block of
        // {type:"resource", resource:{uri, mimeType, text}}.
        let r = ListResult {
            messages: vec![msg("m-1", "S", "Subj")],
            ..Default::default()
        };
        let v = serde_json::to_value(inbox_resource(&r)).unwrap();
        assert_eq!(v["type"], "resource");
        assert_eq!(v["resource"]["uri"], INBOX_UI_URI);
        assert_eq!(v["resource"]["mimeType"], "text/html");
        assert!(
            v["resource"]["text"]
                .as_str()
                .unwrap()
                .contains("<!doctype html>")
        );
    }

    #[test]
    fn click_wires_get_message_with_id() {
        let r = ListResult {
            messages: vec![msg("m-290491", "S", "Subj")],
            ..Default::default()
        };
        let html = render_inbox_html(&r);
        assert!(html.contains("edookit_get_message"));
        assert!(html.contains(r#"data-id="m-290491""#));
    }

    #[test]
    fn escapes_malicious_subject_and_sender() {
        // A teacher-controlled subject/sender must not break out into markup or
        // attributes — this is the whole security premise of the widget.
        let r = ListResult {
            messages: vec![msg(
                "m-1",
                r#""><img src=x onerror=alert(1)>"#,
                "<script>alert('xss')</script>",
            )],
            ..Default::default()
        };
        let html = render_inbox_html(&r);
        assert!(
            !html.contains("<script>alert"),
            "raw <script> leaked into the widget"
        );
        assert!(
            !html.contains("<img src=x onerror"),
            "raw <img> leaked into the widget"
        );
        assert!(html.contains("&lt;script&gt;"), "subject not escaped");
        assert!(
            html.contains("&quot;&gt;&lt;img"),
            "sender attribute payload not escaped"
        );
    }

    #[test]
    fn empty_inbox_has_no_rows() {
        let html = render_inbox_html(&ListResult::default());
        assert!(html.contains("Žádné zprávy"));
        assert!(!html.contains("<li"));
        assert!(html.contains("Přijaté zprávy (0)"));
    }

    #[test]
    fn attachment_badge_only_when_present() {
        let mut m = msg("m-1", "S", "Subj");
        m.attachments = 3;
        let html = render_inbox_html(&ListResult {
            messages: vec![m],
            ..Default::default()
        });
        assert!(html.contains("📎 3"));
    }
}
