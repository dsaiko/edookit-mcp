//! `list_inbox` / `list_sent` — paginated list of inbox / sent rows with HTML
//! parsing. Port of Go's `internal/tools/messages.go`.

use std::sync::LazyLock;

use anyhow::{Context, anyhow, bail};
use jiff::tz::TimeZone;
use regex::Regex;
use scraper::{ElementRef, Selector};
use serde::{Deserialize, Serialize};

use super::date::civil_to_rfc3339;
use super::htmlutil::{collapse_whitespace, parse_fragment, root_of, truncate_runes};
use crate::client::Client;

/// One row returned by [`list_inbox`] / [`list_sent`]. `sender` is populated for
/// inbox rows; `status` for sent rows. Subjects/bodies stay in their original
/// Czech.
#[derive(Debug, Clone, PartialEq, Default, Serialize)]
pub struct Message {
    pub id: String,
    pub number: i64,
    pub date: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub sender: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub status: String,
    pub subject: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub body_preview: String,
    pub attachments: i64,
}

/// Result of [`list_inbox`] / [`list_sent`]. `parse_warnings` records each row
/// the server returned that we couldn't parse (schema drift), so the caller can
/// distinguish "no messages match" from "the parser silently dropped
/// everything". When *every* fetched row fails to parse, the call errors.
#[derive(Debug, Clone, Default, Serialize)]
pub struct ListResult {
    pub messages: Vec<Message>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub parse_warnings: Vec<String>,
}

#[derive(Debug, Default, Clone)]
pub struct InboxOptions {
    pub view: String,
    pub fulltext: String,
    pub since: String,
    pub limit: i64,
}

#[derive(Debug, Default, Clone)]
pub struct SentOptions {
    pub fulltext: String,
    pub since: String,
    pub limit: i64,
}

pub const VIEW_INBOX: &str = "inbox";
pub const VIEW_UNREAD: &str = "unread";
pub const VIEW_STARRED: &str = "starred";
pub const VIEW_ARCHIVED: &str = "archived";
pub const VIEW_ALL: &str = "all";

const DEFAULT_LIMIT: i64 = 50;
const MAX_LIMIT: i64 = 200;
const PAGE_SIZE: usize = 100; // server-fixed
const MAX_PAGES: usize = 5; // safety cap (5 × 100 = 500 rows max)

fn valid_inbox_view(v: &str) -> bool {
    matches!(v, VIEW_INBOX | VIEW_UNREAD | VIEW_STARRED | VIEW_ARCHIVED | VIEW_ALL)
}

/// Fetches received messages (Komunikace → Přijaté).
pub async fn list_inbox(cli: &Client, opts: InboxOptions) -> anyhow::Result<ListResult> {
    let view = if opts.view.is_empty() { VIEW_INBOX } else { opts.view.as_str() };
    if !valid_inbox_view(view) {
        bail!(
            "invalid view {view:?} (want one of {VIEW_INBOX}/{VIEW_UNREAD}/{VIEW_STARRED}/{VIEW_ARCHIVED}/{VIEW_ALL})"
        );
    }
    let mut q: Vec<(&str, String)> = vec![
        ("object_type_general", "object_type_message".into()),
        ("object_filter", view.into()),
    ];
    if !opts.fulltext.is_empty() {
        q.push(("fulltext", opts.fulltext.clone()));
    }
    fetch_and_parse(
        cli,
        "/handler/grid/objects-for-me-data",
        &q,
        &opts.since,
        normalize_limit(opts.limit),
        false,
    )
    .await
}

/// Fetches messages the user has sent (Komunikace → Vytvořené).
pub async fn list_sent(cli: &Client, opts: SentOptions) -> anyhow::Result<ListResult> {
    let mut q: Vec<(&str, String)> = vec![("object_type_general", "object_type_message".into())];
    if !opts.fulltext.is_empty() {
        q.push(("fulltext", opts.fulltext.clone()));
    }
    fetch_and_parse(
        cli,
        "/handler/grid/created-objects-data",
        &q,
        &opts.since,
        normalize_limit(opts.limit),
        true,
    )
    .await
}

fn normalize_limit(n: i64) -> i64 {
    if n <= 0 {
        DEFAULT_LIMIT
    } else if n > MAX_LIMIT {
        MAX_LIMIT
    } else {
        n
    }
}

#[derive(Deserialize)]
struct GridResponse {
    #[serde(default)]
    components: GridComponents,
}
#[derive(Deserialize, Default)]
struct GridComponents {
    #[serde(default)]
    workspace: Vec<GridWorkspace>,
}
#[derive(Deserialize)]
struct GridWorkspace {
    #[serde(default)]
    data: Vec<Vec<String>>,
}

async fn fetch_and_parse(
    cli: &Client,
    path: &str,
    base_query: &[(&str, String)],
    since: &str,
    limit: i64,
    is_sent: bool,
) -> anyhow::Result<ListResult> {
    let tz = cli.timezone();
    let since_ts = parse_since(since, tz).with_context(|| format!("invalid since {since:?}"))?;

    let mut result = ListResult::default();
    let mut rows_fetched = 0usize;

    for page in 1..=MAX_PAGES {
        let mut pairs = base_query.to_vec();
        pairs.push(("page", page.to_string()));
        let full = format!("{path}?{}", encode_query(&pairs));

        let resp: GridResponse = cli
            .get_json(&full)
            .await
            .map_err(|e| anyhow!("fetch page {page}: {e}"))?;
        let Some(ws) = resp.components.workspace.first() else {
            break;
        };
        if ws.data.is_empty() {
            break;
        }
        let rows = &ws.data;
        rows_fetched += rows.len();

        for (row_idx, row) in rows.iter().enumerate() {
            if row.len() < 3 {
                let w = format!(
                    "row {} on page {page} has only {} cells (expected >=3)",
                    row_idx + 1,
                    row.len()
                );
                tracing::warn!("skipping {w}");
                result.parse_warnings.push(w);
                continue;
            }
            match parse_row(&row[0], &row[2], is_sent, tz) {
                Err(e) => {
                    tracing::warn!("skipping malformed row: {e}");
                    result.parse_warnings.push(e.to_string());
                    continue;
                }
                Ok(msg) => {
                    if let Some(floor) = since_ts
                        && let Ok(t) = msg.date.parse::<jiff::Timestamp>()
                        && t < floor
                    {
                        // Skip older row but keep scanning — don't assume the
                        // server returns rows strictly newest-first.
                        continue;
                    }
                    result.messages.push(msg);
                    if result.messages.len() >= limit as usize {
                        return finalize_result(result, rows_fetched);
                    }
                }
            }
        }
        if rows.len() < PAGE_SIZE {
            break; // last (partial) page
        }
    }
    finalize_result(result, rows_fetched)
}

/// If the server gave us rows but every one failed to parse, that's a hard
/// failure (schema drift) — surface it rather than a silent empty mailbox.
fn finalize_result(r: ListResult, rows_fetched: usize) -> anyhow::Result<ListResult> {
    if rows_fetched > 0 && r.messages.is_empty() && !r.parse_warnings.is_empty() {
        bail!(
            "fetched {rows_fetched} row(s) but none parsed — schema may have drifted (first warning: {})",
            r.parse_warnings[0]
        );
    }
    Ok(r)
}

fn encode_query(pairs: &[(&str, String)]) -> String {
    let mut s = url::form_urlencoded::Serializer::new(String::new());
    for (k, v) in pairs {
        s.append_pair(k, v);
    }
    s.finish()
}

static CZECH_DATE_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(\d{1,2})\.(\d{1,2})\.(\d{4}) (\d{1,2}):(\d{1,2})").unwrap());
// Edookit renders "Dnes HH:MM" / "Včera HH:MM" for the last day or two; the day
// word sits in its own colored span inside the date <b>.
static RELATIVE_DATE_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?i)(dnes|včera)\s+(\d{1,2}):(\d{1,2})").unwrap());
static SMALL_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("small").unwrap());
static SUBJECT_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("div a b").unwrap());

fn parse_part<T: std::str::FromStr>(s: &str) -> Option<T> {
    s.parse().ok()
}

/// Extracts the message date from a row's `<small>` text. Absolute
/// "DD.MM.YYYY HH:MM" or the relative "Dnes/Včera HH:MM" resolved against the
/// school's wall-clock today.
fn parse_small_date(small_text: &str, tz: &TimeZone) -> Option<String> {
    if let Some(c) = CZECH_DATE_RE.captures(small_text) {
        return civil_to_rfc3339(
            parse_part(&c[3])?,
            parse_part(&c[2])?,
            parse_part(&c[1])?,
            parse_part(&c[4])?,
            parse_part(&c[5])?,
            tz,
        );
    }
    if let Some(c) = RELATIVE_DATE_RE.captures(small_text) {
        let now = jiff::Zoned::now().with_time_zone(tz.clone());
        let day = if c[1].eq_ignore_ascii_case("včera") {
            now.checked_sub(jiff::Span::new().days(1)).ok()?
        } else {
            now
        };
        let d = day.date();
        return civil_to_rfc3339(
            d.year(),
            d.month(),
            d.day(),
            parse_part(&c[2])?,
            parse_part(&c[3])?,
            tz,
        );
    }
    None
}

/// Extracts structured fields from one grid row. Required fields (date,
/// subject, and sender-or-status) must all be present — if any are missing,
/// returns an error naming them so the caller logs and skips the row.
fn parse_row(uid: &str, row_html: &str, is_sent: bool, tz: &TimeZone) -> anyhow::Result<Message> {
    if uid.is_empty() {
        bail!("empty row UID");
    }
    let mut msg = Message {
        id: uid.to_string(),
        ..Default::default()
    };
    if let Ok(n) = uid.strip_prefix("m-").unwrap_or(uid).parse::<i64>()
        && n > 0
    {
        msg.number = n;
    }

    let doc = parse_fragment(row_html);
    let mut missing: Vec<&str> = Vec::new();

    let small = doc.select(&SMALL_SEL).next();
    let small_text = small.map(|s| s.text().collect::<String>()).unwrap_or_default();
    match parse_small_date(&small_text, tz) {
        Some(iso) => msg.date = iso,
        None => missing.push("date"),
    }

    // First direct-child <span> of <small> is the sender (inbox) or status
    // (sent). Use children (not a descendant find) because relative-date rows
    // nest a "Dnes"/"Včera" span inside the date <b>.
    let first_span = small
        .and_then(|s| s.children().filter_map(ElementRef::wrap).find(|e| e.value().name() == "span"))
        .map(|e| collapse_whitespace(&e.text().collect::<String>()))
        .filter(|t| !t.is_empty());
    match first_span {
        Some(t) if is_sent => msg.status = t,
        Some(t) => msg.sender = t,
        None if is_sent => missing.push("status"),
        None => missing.push("sender"),
    }

    // Subject: the <b> inside a row main-area <a>.
    if let Some(b) = doc.select(&SUBJECT_SEL).find(|b| !b.text().collect::<String>().trim().is_empty()) {
        msg.subject = b.text().collect::<String>().trim().to_string();
    }
    if msg.subject.is_empty() {
        missing.push("subject");
    }

    if let Some(root) = root_of(&doc) {
        msg.attachments = parse_attachment_count(root);
        msg.body_preview = parse_body_preview(root);
    }

    if !missing.is_empty() {
        bail!("row {uid} missing required field(s): {}", missing.join(", "));
    }
    Ok(msg)
}

/// Finds "Přílohy" followed by "(N)" in the row DOM. Returns 0 if absent.
fn parse_attachment_count(root: ElementRef) -> i64 {
    let mut seen_prilohy = false;
    for n in root.descendants() {
        let Some(el) = n.value().as_element() else {
            continue;
        };
        let Some(eref) = ElementRef::wrap(n) else { continue };
        match el.name() {
            "span" if eref.text().collect::<String>().trim() == "Přílohy" => {
                seen_prilohy = true;
            }
            "b" if seen_prilohy => {
                let t = eref.text().collect::<String>();
                let t = t.trim();
                if let Some(inner) = t.strip_prefix('(').and_then(|x| x.strip_suffix(')'))
                    && let Ok(n) = inner.parse::<i64>()
                {
                    return n;
                }
            }
            _ => {}
        }
    }
    0
}

const BODY_PREVIEW_MAX_RUNES: usize = 200;

/// Extracts the body preview: content between the subject `<div>` and the first
/// `<br>`. Walks the parsed DOM so inline tags decode and entities resolve.
fn parse_body_preview(root: ElementRef) -> String {
    let mut sb = String::new();
    let mut saw_subject_div = false;
    for n in root.children() {
        if let Some(el) = n.value().as_element() {
            match el.name() {
                "div" => {
                    if !saw_subject_div {
                        // First top-level <div> = subject + attachments container.
                        saw_subject_div = true;
                    } else {
                        break; // a subsequent <div> (toolbar) — body ended.
                    }
                }
                "br" if saw_subject_div => break,
                _ if saw_subject_div => {
                    if let Some(er) = ElementRef::wrap(n) {
                        sb.push_str(&er.text().collect::<String>());
                    }
                }
                _ => {}
            }
        } else if let Some(t) = n.value().as_text()
            && saw_subject_div
        {
            sb.push_str(t);
        }
    }
    truncate_runes(&collapse_whitespace(&sb), BODY_PREVIEW_MAX_RUNES)
}

/// Accepts "7d"/"1w"/"2m"/"1y" or an ISO date (YYYY-MM-DD in `tz`, or RFC3339).
/// Empty string means "no floor". Returns the instant floor.
fn parse_since(s: &str, tz: &TimeZone) -> anyhow::Result<Option<jiff::Timestamp>> {
    if s.is_empty() {
        return Ok(None);
    }
    if s.len() >= 2 {
        let (num, unit) = s.split_at(s.len() - 1);
        if let Ok(val) = num.parse::<i64>()
            && val > 0
        {
            let span = match unit {
                "d" => Some(jiff::Span::new().days(val)),
                "w" => Some(jiff::Span::new().days(val * 7)),
                "m" => Some(jiff::Span::new().months(val)),
                "y" => Some(jiff::Span::new().years(val)),
                _ => None,
            };
            if let Some(span) = span {
                let floor = jiff::Zoned::now().checked_sub(span)?.timestamp();
                return Ok(Some(floor));
            }
        }
    }
    // Bare YYYY-MM-DD interpreted in the school's tz (start of day).
    if let Ok(date) = s.parse::<jiff::civil::Date>() {
        return Ok(Some(date.to_zoned(tz.clone())?.timestamp()));
    }
    // RFC3339 — its own offset is preserved.
    if let Ok(ts) = s.parse::<jiff::Timestamp>() {
        return Ok(Some(ts));
    }
    bail!("expected '7d', '1w', '2m', '1y', or ISO date (YYYY-MM-DD / RFC3339)")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tz() -> TimeZone {
        TimeZone::get("Europe/Prague").unwrap()
    }

    // A realistic inbox row: <small> with date + sender span, subject <a><b>,
    // a body preview text node, and an attachments badge.
    fn inbox_row() -> &'static str {
        // Date in a <b>, sender as a direct-child <span> of the same <small>
        // (matches real Edookit rows). Body preview is the text node right after
        // the subject <div>, up to the first <br>.
        concat!(
            r#"<small><b>21.05.2026 12:31</b> <span style="color:#77bb00">Nováková Eva (učitel 4SC)</span></small>"#,
            r#"<div><a href="x"><b>Pozvánka na třídní schůzky</b></a> <span>Přílohy</span> <b>(2)</b></div>"#,
            "Dobrý den, zveme Vás na třídní schůzky ve čtvrtek.<br><div>toolbar</div>"
        )
    }

    #[test]
    fn parse_inbox_row_extracts_fields() {
        let msg = parse_row("m-290491", inbox_row(), false, &tz()).unwrap();
        assert_eq!(msg.id, "m-290491");
        assert_eq!(msg.number, 290491);
        assert_eq!(msg.date, "2026-05-21T12:31:00+02:00");
        assert_eq!(msg.sender, "Nováková Eva (učitel 4SC)");
        assert_eq!(msg.subject, "Pozvánka na třídní schůzky");
        assert_eq!(msg.attachments, 2);
        assert!(msg.body_preview.starts_with("Dobrý den, zveme Vás"));
        assert!(msg.status.is_empty());
    }

    #[test]
    fn sent_row_uses_status_not_sender() {
        let row = r#"<small><b>21.05.2026 12:31</b> <span>Publikováno</span></small><div><a href="x"><b>Test</b></a></div>"#;
        let msg = parse_row("m-1", row, true, &tz()).unwrap();
        assert_eq!(msg.status, "Publikováno");
        assert!(msg.sender.is_empty());
        assert_eq!(msg.attachments, 0);
    }

    #[test]
    fn impossible_date_makes_row_invalid() {
        let row = r#"<small><b>32.13.2026 25:99</b> <span>X</span></small><div><a href="x"><b>S</b></a></div>"#;
        let err = parse_row("m-1", row, false, &tz()).unwrap_err();
        assert!(err.to_string().contains("date"), "got: {err}");
    }

    #[test]
    fn missing_fields_reported() {
        let err = parse_row("m-1", "<div></div>", false, &tz()).unwrap_err();
        let s = err.to_string();
        assert!(s.contains("date") && s.contains("sender") && s.contains("subject"), "got: {s}");
    }

    #[test]
    fn empty_uid_rejected() {
        assert!(parse_row("", "<div></div>", false, &tz()).is_err());
    }

    #[test]
    fn body_preview_truncates_to_runes() {
        let long = "á".repeat(300);
        let row = format!(
            r#"<small><b>21.05.2026 12:31</b> <span>X</span></small><div><a href="x"><b>S</b></a></div>{long}<br>"#
        );
        let msg = parse_row("m-1", &row, false, &tz()).unwrap();
        let n = msg.body_preview.chars().count();
        assert_eq!(n, BODY_PREVIEW_MAX_RUNES + 1, "200 runes + ellipsis"); // includes the '…'
        assert!(msg.body_preview.ends_with('…'));
    }

    #[test]
    fn parse_since_relative_and_absolute() {
        let tz = tz();
        assert!(parse_since("", &tz).unwrap().is_none());
        assert!(parse_since("7d", &tz).unwrap().is_some());
        assert!(parse_since("2m", &tz).unwrap().is_some());
        assert!(parse_since("2026-05-01", &tz).unwrap().is_some());
        // invalid forms
        assert!(parse_since("0d", &tz).is_err());
        assert!(parse_since("-5d", &tz).is_err());
        assert!(parse_since("5q", &tz).is_err());
        assert!(parse_since("garbage", &tz).is_err());
    }

    #[test]
    fn normalize_limit_clamps() {
        assert_eq!(normalize_limit(0), DEFAULT_LIMIT);
        assert_eq!(normalize_limit(-3), DEFAULT_LIMIT);
        assert_eq!(normalize_limit(10), 10);
        assert_eq!(normalize_limit(9999), MAX_LIMIT);
    }
}
