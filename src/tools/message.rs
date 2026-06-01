//! `get_message` — full body + attachment metadata + read-receipts for one
//! message (shared inbox/sent endpoint). Port of Go's `internal/tools/message.go`.
//!
//! The `/handler/page/message-edit` response is loosely typed (the `data` field
//! of a workspace component is an object for the form/fileviewer panels but a
//! bare array for the acceptance grid), so we interpret it through
//! `serde_json::Value` rather than modelling every shape — the polymorphism the
//! Go version handled with a custom `UnmarshalJSON` falls out naturally.

use std::sync::LazyLock;

use anyhow::{anyhow, bail};
use jiff::tz::TimeZone;
use regex::Regex;
use scraper::Selector;
use serde::Serialize;
use serde_json::Value;

use super::htmlutil::{collapse_text_lines, collapse_whitespace, parse_fragment, root_of};
use crate::client::Client;

/// A single message with full body and attachment list. Same shape for received
/// and sent (Edookit serves both via one endpoint).
#[derive(Debug, Clone, Default, Serialize)]
pub struct FullMessage {
    pub id: String,
    pub number: i64,
    pub subject: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub status: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub author: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub date: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub body_html: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub body_text: String,
    #[serde(skip_serializing_if = "is_false")]
    pub deleted: bool,
    pub attachments: Vec<Attachment>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub recipients: Vec<Recipient>,
}

fn is_false(b: &bool) -> bool {
    !*b
}

/// One row of the delivery / read-receipt table ("Doručenky"). On sent
/// messages this tells the author who opened the message and when.
#[derive(Debug, Clone, Default, PartialEq, Serialize)]
pub struct Recipient {
    pub name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub read_at: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub parents: Vec<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub parents_read_at: Vec<String>,
}

/// One file linked from a message. `url` is fully qualified and GET-able by the
/// authenticated session.
#[derive(Debug, Clone, Default, PartialEq, Serialize)]
pub struct Attachment {
    pub id: String,
    pub name: String,
    pub url: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub date: String,
}

const DOM_FORM_MESSAGE: &str = "__lc_Form_Message";
const DOM_FILEVIEWER: &str = "__lc_Fileviewer_Slave_datatemplate_message";
const DOM_ACCEPTANCE: &str = "__lc_Grid_Acceptance";

/// Fetches and parses a single message by row UID ("m-NNNNNN" or "NNNNNN").
pub async fn get_message(cli: &Client, id_or_uid: &str) -> anyhow::Result<FullMessage> {
    let num = normalize_message_id(id_or_uid)?;
    let path = format!("/handler/page/message-edit?__index={num}");
    let raw: Value = cli
        .get_json(&path)
        .await
        .map_err(|e| anyhow!("fetch message {id_or_uid}: {e}"))?;
    parse_full_message(num, &raw, cli.timezone())
}

/// Strips the optional "m-" prefix and parses a positive int.
pub fn normalize_message_id(s: &str) -> anyhow::Result<i64> {
    let stripped = s.trim();
    let stripped = stripped.strip_prefix("m-").unwrap_or(stripped);
    match stripped.parse::<i64>() {
        Ok(n) if n > 0 => Ok(n),
        _ => bail!("invalid message id {s:?} (expected m-NNNN or NNNN)"),
    }
}

fn parse_full_message(num: i64, raw: &Value, tz: &TimeZone) -> anyhow::Result<FullMessage> {
    if raw.get("authenticated") == Some(&Value::Bool(false)) {
        bail!("server reported authenticated=false");
    }

    let workspace = raw
        .get("components")
        .and_then(|c| c.get("workspace"))
        .and_then(Value::as_array);
    let workspace = workspace
        .ok_or_else(|| anyhow!("message-edit response has no components.workspace array"))?;

    let mut form: Option<&Value> = None;
    let mut fileviewer: Option<&Value> = None;
    let mut acceptance: Option<&Value> = None;
    for comp in workspace {
        match comp.get("DOMTarget").and_then(Value::as_str) {
            Some(DOM_FORM_MESSAGE) => form = comp.get("data"),
            Some(DOM_FILEVIEWER) => fileviewer = comp.get("data"),
            Some(DOM_ACCEPTANCE) => acceptance = comp.get("data"),
            _ => {}
        }
    }

    let Some(form) = form else {
        bail!(
            "message-edit response has no {DOM_FORM_MESSAGE} workspace component (got {} components)",
            workspace.len()
        );
    };

    let mut msg = FullMessage {
        id: format!("m-{num}"),
        number: num,
        attachments: Vec::new(),
        ..Default::default()
    };
    populate_message_fields(&mut msg, form, tz);

    // Author-deleted messages: Edookit keeps status/author/date but strips
    // subject + body, and the status label starts "Smazané autorem …".
    if msg.status.starts_with("Smazané") {
        msg.deleted = true;
    }

    // Schema-drift guard: empty status AND subject AND body means the form lost
    // all human-readable content (real drift). Author-deletion leaves status
    // non-empty, so this won't false-positive on it.
    if msg.status.is_empty()
        && msg.subject.is_empty()
        && msg.body_text.is_empty()
        && msg.body_html.is_empty()
    {
        bail!(
            "message {num}: parsed message has empty status AND subject AND body — Edookit form schema may have drifted"
        );
    }

    if let Some(fv) = fileviewer {
        msg.attachments = collect_attachments(fv, tz);
    }
    if let Some(acc) = acceptance.and_then(Value::as_array) {
        msg.recipients = collect_recipients(acc);
    }
    Ok(msg)
}

/// Extracts subject / status / author / date / body from the form panels.
fn populate_message_fields(msg: &mut FullMessage, form_data: &Value, tz: &TimeZone) {
    let panels = form_data.get("__form_panel_main").and_then(Value::as_array);
    let Some(panels) = panels else { return };

    if let Some(item) = find_form_item(panels, "name") {
        msg.subject = value_to_string(item.get("val")).trim().to_string();
    }
    if let Some(item) = find_form_item(panels, "object_status") {
        let (status, author, date) = parse_status_html(&value_to_string(item.get("val")), tz);
        msg.status = status;
        msg.author = author;
        msg.date = date;
    }
    if let Some(item) = find_form_item(panels, "description__editor") {
        // Prefer readValue (single-escaped, ready to render); val is double-escaped.
        let read = value_to_string(item.get("readValue"));
        msg.body_html = if read.is_empty() {
            value_to_string(item.get("val"))
        } else {
            read
        };
        msg.body_text = html_to_text(&msg.body_html);
    }
}

/// Walks the labeled/unlabeled panels of `__form_panel_main` for the first item
/// with `items[].name == item_name`.
fn find_form_item<'a>(panels: &'a [Value], item_name: &str) -> Option<&'a Value> {
    for panel in panels {
        if let Some(items) = panel.get("items").and_then(Value::as_array) {
            for item in items {
                if item.get("name").and_then(Value::as_str) == Some(item_name) {
                    return Some(item);
                }
            }
        }
    }
    None
}

/// Stringifies an Edookit field value, handling the scalar cases (string,
/// integer-valued number, bool). Unknown shapes (arrays/objects/null) → "".
fn value_to_string(v: Option<&Value>) -> String {
    match v {
        Some(Value::String(s)) => s.clone(),
        Some(Value::Number(n)) => {
            if let Some(i) = n.as_i64() {
                i.to_string()
            } else {
                n.to_string()
            }
        }
        Some(Value::Bool(b)) => b.to_string(),
        _ => String::new(),
    }
}

fn collect_attachments(fileviewer_data: &Value, tz: &TimeZone) -> Vec<Attachment> {
    let Some(rows) = fileviewer_data.get("data").and_then(Value::as_array) else {
        return Vec::new();
    };
    let mut out = Vec::with_capacity(rows.len());
    for a in rows {
        if a.get("trashed").and_then(Value::as_bool) == Some(true) {
            continue;
        }
        out.push(Attachment {
            id: value_to_string(a.get("id")),
            name: value_to_string(a.get("name")),
            url: value_to_string(a.get("link")),
            date: super::date::unix_to_rfc3339(
                a.get("date").and_then(Value::as_i64).unwrap_or(0),
                tz,
            ),
        });
    }
    out
}

/// Converts acceptance-grid rows into recipients. Row layout (5-tuple):
/// `[id, name, first_seen, parents(br-joined), parents_first_seen(br-joined)]`.
fn collect_recipients(rows: &[Value]) -> Vec<Recipient> {
    let mut out = Vec::with_capacity(rows.len());
    for row in rows {
        let Some(cells) = row.as_array() else {
            continue;
        };
        if cells.len() < 5 {
            continue;
        }
        let cell = |i: usize| cells[i].as_str().unwrap_or("");
        let parents = split_br(cell(3));
        let parents_read: Vec<String> = split_br(cell(4))
            .iter()
            .map(|s| czech_date_to_iso(s))
            .collect();
        out.push(Recipient {
            name: cell(1).trim().to_string(),
            read_at: czech_date_to_iso(cell(2)),
            parents_read_at: align_parents_read(&parents_read, parents.len()),
            parents,
        });
    }
    out
}

/// Normalizes the parents-read indicator to match the parent count. Edookit
/// collapses a uniform "Ne / Ne" to a single "Ne"; we replicate the last value
/// to fill, empty stays empty, over-long is truncated.
fn align_parents_read(read: &[String], n_parents: usize) -> Vec<String> {
    if n_parents == 0 {
        return Vec::new();
    }
    if read.is_empty() {
        return vec![String::new(); n_parents];
    }
    if read.len() < n_parents {
        let mut out = Vec::with_capacity(n_parents);
        for i in 0..n_parents {
            out.push(
                read.get(i)
                    .cloned()
                    .unwrap_or_else(|| read[read.len() - 1].clone()),
            );
        }
        return out;
    }
    read[..n_parents].to_vec()
}

/// Converts Czech "DD.MM.YYYY" → ISO "YYYY-MM-DD". Empty, "Ne" (not read), or
/// anything unparseable → "".
fn czech_date_to_iso(s: &str) -> String {
    let s = s.trim();
    if s.is_empty() || s == "Ne" {
        return String::new();
    }
    // Tolerate stray internal spaces ("21. 5. 2026"), then parse strictly.
    let compact = s.replace(' ', "");
    let mut parts = compact.split('.');
    let (Some(d), Some(m), Some(y), None) =
        (parts.next(), parts.next(), parts.next(), parts.next())
    else {
        return String::new();
    };
    let (Ok(d), Ok(m), Ok(y)) = (d.parse::<i8>(), m.parse::<i8>(), y.parse::<i16>()) else {
        return String::new();
    };
    match jiff::civil::Date::new(y, m, d) {
        Ok(date) => format!("{:04}-{:02}-{:02}", date.year(), date.month(), date.day()),
        Err(_) => String::new(),
    }
}

fn split_br(s: &str) -> Vec<String> {
    if s.is_empty() {
        return Vec::new();
    }
    s.split("<br>").map(|x| x.to_string()).collect()
}

static STATUS_DATE_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"Od\s+(\d{1,2})\.(\d{1,2})\.(\d{4})\s+(\d{1,2}):(\d{1,2})").unwrap()
});
static SPAN_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("span").unwrap());
static SPAN_B_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("span b").unwrap());

/// Extracts (status, author, date_rfc3339) from the `object_status` HTML. Any
/// field that can't be parsed is returned empty.
fn parse_status_html(s: &str, tz: &TimeZone) -> (String, String, String) {
    if s.is_empty() {
        return (String::new(), String::new(), String::new());
    }
    let doc = parse_fragment(s);
    let Some(root) = root_of(&doc) else {
        return (String::new(), String::new(), String::new());
    };

    // First top-level <span> = status word.
    let status = doc
        .select(&SPAN_SEL)
        .next()
        .map(|e| e.text().collect::<String>().trim().to_string())
        .unwrap_or_default();
    // <b> inside a <span> holds the author.
    let author = doc
        .select(&SPAN_B_SEL)
        .next()
        .map(|e| collapse_whitespace(&e.text().collect::<String>()))
        .unwrap_or_default();
    // Inline "Od DD.MM.YYYY HH:MM" — present for received messages.
    let root_text = root.text().collect::<String>();
    let date = STATUS_DATE_RE
        .captures(&root_text)
        .and_then(|c| {
            super::date::civil_to_rfc3339(
                c[3].parse().ok()?,
                c[2].parse().ok()?,
                c[1].parse().ok()?,
                c[4].parse().ok()?,
                c[5].parse().ok()?,
                tz,
            )
        })
        .unwrap_or_default();

    (status, author, date)
}

/// Renders editor HTML to plain text: tags stripped, entities decoded,
/// paragraph/`<br>` breaks preserved as newlines, whitespace collapsed.
fn html_to_text(raw_html: &str) -> String {
    if raw_html.is_empty() {
        return String::new();
    }
    let doc = parse_fragment(raw_html);
    let Some(root) = root_of(&doc) else {
        return String::new();
    };
    let mut r = TextRenderer::default();
    for child in root.children() {
        r.walk(child);
    }
    collapse_text_lines(&r.sb)
}

#[derive(Default)]
struct TextRenderer {
    sb: String,
    last_was_newline: bool,
}

impl TextRenderer {
    fn write_str(&mut self, s: &str) {
        if s.is_empty() {
            return;
        }
        self.sb.push_str(s);
        self.last_was_newline = s.ends_with('\n');
    }
    fn write_newline(&mut self) {
        self.sb.push('\n');
        self.last_was_newline = true;
    }
    fn ensure_newline(&mut self) {
        if !self.last_was_newline && !self.sb.is_empty() {
            self.write_newline();
        }
    }
    fn walk(&mut self, node: ego_tree::NodeRef<'_, scraper::Node>) {
        if let Some(el) = node.value().as_element() {
            match el.name() {
                "br" => self.write_newline(),
                "p" | "div" | "li" => {
                    self.ensure_newline();
                    self.walk_children(node);
                    self.ensure_newline();
                }
                _ => self.walk_children(node),
            }
        } else if let Some(t) = node.value().as_text() {
            self.write_str(t);
        } else {
            self.walk_children(node);
        }
    }
    fn walk_children(&mut self, node: ego_tree::NodeRef<'_, scraper::Node>) {
        for c in node.children() {
            self.walk(c);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn tz() -> TimeZone {
        TimeZone::get("Europe/Prague").unwrap()
    }

    fn message_edit(status_html: &str, with_attachment: bool, with_acceptance: bool) -> Value {
        let mut workspace = vec![json!({
            "DOMTarget": "__lc_Form_Message",
            "data": {
                "__form_panel_main": [{
                    "items": [
                        {"name": "name", "val": "NOVINKY: třídní schůzky"},
                        {"name": "object_status", "val": status_html},
                        {"name": "description__editor", "readValue": "<p>Dobrý den.</p><p>Druhý odstavec.</p>"}
                    ]
                }]
            }
        })];
        if with_attachment {
            workspace.push(json!({
                "DOMTarget": "__lc_Fileviewer_Slave_datatemplate_message",
                "data": {"data": [
                    {"id": "1@191968", "name": "schedule.pdf", "link": "https://school.edookit.net/handler/download/file-x", "date": 1_716_000_000, "trashed": false},
                    {"id": "2@191969", "name": "old.pdf", "link": "https://x/old", "date": 0, "trashed": true}
                ]}
            }));
        }
        if with_acceptance {
            workspace.push(json!({
                "DOMTarget": "__lc_Grid_Acceptance",
                "data": [
                    ["sid1", "Fajkus Eliáš", "21.05.2026", "Fajkus Martin<br>Fajkusová Soňa", "Ne<br>21.05.2026"],
                    ["sid2", "Novák Petr", "Ne", "", ""]
                ]
            }));
        }
        json!({"authenticated": true, "components": {"workspace": workspace}})
    }

    #[test]
    fn parses_received_message_with_attachments_and_recipients() {
        let status = r#"<span style="color:#090">Publikováno</span> Od 21.05.2026 12:31 <span>, </span><span style="font-size:75%"><b>Nováková Eva</b>, Po 21.05. 12:31</span>"#;
        let raw = message_edit(status, true, true);
        let msg = parse_full_message(290491, &raw, &tz()).unwrap();

        assert_eq!(msg.id, "m-290491");
        assert_eq!(msg.subject, "NOVINKY: třídní schůzky");
        assert_eq!(msg.status, "Publikováno");
        assert_eq!(msg.author, "Nováková Eva");
        assert_eq!(msg.date, "2026-05-21T12:31:00+02:00");
        assert_eq!(msg.body_text, "Dobrý den.\nDruhý odstavec.");
        assert!(msg.body_html.contains("<p>"));
        assert!(!msg.deleted);

        // Trashed attachment filtered out.
        assert_eq!(msg.attachments.len(), 1);
        assert_eq!(msg.attachments[0].name, "schedule.pdf");
        assert!(!msg.attachments[0].date.is_empty());

        // Recipients + parents alignment.
        assert_eq!(msg.recipients.len(), 2);
        assert_eq!(msg.recipients[0].name, "Fajkus Eliáš");
        assert_eq!(msg.recipients[0].read_at, "2026-05-21");
        assert_eq!(
            msg.recipients[0].parents,
            vec!["Fajkus Martin", "Fajkusová Soňa"]
        );
        assert_eq!(msg.recipients[0].parents_read_at, vec!["", "2026-05-21"]);
        // Staff recipient (no parents): "Ne" read_at → empty, no parents.
        assert_eq!(msg.recipients[1].read_at, "");
        assert!(msg.recipients[1].parents.is_empty());
    }

    #[test]
    fn author_deleted_message_sets_deleted_not_drift_error() {
        let status = r#"<span>Smazané autorem 01.06.2026 09:00</span>"#;
        // No subject/body content (Edookit strips them) — but status survives.
        let raw = json!({
            "authenticated": true,
            "components": {"workspace": [{
                "DOMTarget": "__lc_Form_Message",
                "data": {"__form_panel_main": [{"items": [
                    {"name": "name", "val": ""},
                    {"name": "object_status", "val": status},
                    {"name": "description__editor", "readValue": ""}
                ]}]}
            }]}
        });
        let msg = parse_full_message(1, &raw, &tz()).unwrap();
        assert!(msg.deleted);
        assert!(msg.status.starts_with("Smazané"));
    }

    #[test]
    fn empty_form_is_schema_drift_error() {
        let raw = json!({
            "authenticated": true,
            "components": {"workspace": [{
                "DOMTarget": "__lc_Form_Message",
                "data": {"__form_panel_main": [{"items": []}]}
            }]}
        });
        let err = parse_full_message(1, &raw, &tz()).unwrap_err();
        assert!(err.to_string().contains("drift"), "got: {err}");
    }

    #[test]
    fn missing_form_component_errors() {
        let raw = json!({"authenticated": true, "components": {"workspace": []}});
        let err = parse_full_message(1, &raw, &tz()).unwrap_err();
        assert!(err.to_string().contains("__lc_Form_Message"), "got: {err}");
    }

    #[test]
    fn normalize_id_forms() {
        assert_eq!(normalize_message_id("m-289862").unwrap(), 289862);
        assert_eq!(normalize_message_id("289862").unwrap(), 289862);
        assert_eq!(normalize_message_id("  m-1 ").unwrap(), 1);
        assert!(normalize_message_id("0").is_err());
        assert!(normalize_message_id("-5").is_err());
        assert!(normalize_message_id("abc").is_err());
    }

    #[test]
    fn html_to_text_breaks_and_entities() {
        assert_eq!(html_to_text("<p>A</p><p>B</p>"), "A\nB");
        assert_eq!(html_to_text("line1<br>line2"), "line1\nline2");
        assert_eq!(html_to_text("a &amp; b"), "a & b");
        assert_eq!(html_to_text(""), "");
    }

    #[test]
    fn czech_date_to_iso_cases() {
        assert_eq!(czech_date_to_iso("21.05.2026"), "2026-05-21");
        assert_eq!(czech_date_to_iso("Ne"), "");
        assert_eq!(czech_date_to_iso(""), "");
        assert_eq!(czech_date_to_iso("32.13.2026"), "");
    }
}
