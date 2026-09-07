//! Overview (Schránka) HTML parsers for Edookit tenants that no longer expose
//! the legacy `/handler/*` JSON API. Newer instances render Komunikace as
//! server-side HTML at `/overview/updates`, `/overview/sent`, etc.

use std::sync::LazyLock;

use anyhow::{Context, anyhow, bail};
use jiff::tz::TimeZone;
use regex::Regex;
use scraper::{ElementRef, Html, Selector};

use super::date::civil_to_rfc3339;
use super::htmlutil::{collapse_whitespace, truncate_runes};
use super::message::{html_to_text, Attachment, FullMessage, Recipient};
use super::messages::{
    InboxOptions, ListResult, Message, SentOptions, VIEW_ALL, VIEW_ARCHIVED, VIEW_INBOX,
    VIEW_STARRED, VIEW_UNREAD,
};
use crate::client::Client;

const MESSAGE_TYPE: &str = "Zpráva";
const BODY_PREVIEW_MAX_RUNES: usize = 200;

static INBOX_ITEM_SEL: LazyLock<Selector> =
    LazyLock::new(|| Selector::parse("div.item.message.inboxMessage").unwrap());
static OBJECT_NAME_A_SEL: LazyLock<Selector> =
    LazyLock::new(|| Selector::parse("div.object-name a").unwrap());
static OBJECT_NAME_SEL: LazyLock<Selector> =
    LazyLock::new(|| Selector::parse("div.object-name").unwrap());
static OBJECT_TYPE_SEL: LazyLock<Selector> =
    LazyLock::new(|| Selector::parse("div.object-type").unwrap());
static DESCRIPTION_SEL: LazyLock<Selector> =
    LazyLock::new(|| Selector::parse("div.description").unwrap());
static CREATOR_SEL: LazyLock<Selector> =
    LazyLock::new(|| Selector::parse("div.creator").unwrap());
static TIME_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("div.time").unwrap());
static RICH_CONTENT_SEL: LazyLock<Selector> =
    LazyLock::new(|| Selector::parse("div.rich_content").unwrap());
static DETAIL_NAME_SEL: LazyLock<Selector> =
    LazyLock::new(|| Selector::parse("span.detail-object-name").unwrap());
static FT_ROW_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("div.ft_row").unwrap());
static TABLE_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("table").unwrap());
static TR_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("tr").unwrap());
static TD_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("td").unwrap());
static DOWNLOAD_LINK_SEL: LazyLock<Selector> =
    LazyLock::new(|| Selector::parse("a[href*='/handler/download/']").unwrap());

static MESSAGE_ID_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"/messages/detail\?message=(\d+)").unwrap());
static OBJECT_ID_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"updates-objectId=(\d+)").unwrap());
static OVERVIEW_DATE_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(\d{1,2})\.\s*(\d{1,2})\.\s*(\d{4})").unwrap());
static OVERVIEW_TIME_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(\d{1,2}):(\d{1,2})").unwrap());
static TITLE_DATE_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"title="(\d{1,2})\.\s*(\d{1,2})\.\s*(\d{4})""#).unwrap()
});

/// Maps inbox view names to the overview HTML path used by newer Edookit UIs.
pub fn inbox_overview_path(view: &str) -> &'static str {
    match view {
        VIEW_UNREAD => "/overview/updates?defaultView=unread",
        VIEW_ARCHIVED => "/overview/archive",
        VIEW_STARRED | VIEW_ALL | VIEW_INBOX | _ => "/overview/updates",
    }
}

/// Fetches received messages from the overview inbox page.
pub async fn list_inbox(cli: &Client, opts: InboxOptions) -> anyhow::Result<ListResult> {
    let view = if opts.view.is_empty() {
        VIEW_INBOX
    } else {
        opts.view.as_str()
    };
    let path = inbox_overview_path(view);
    let html = cli
        .get_text(path)
        .await
        .map_err(|e| anyhow!("fetch overview inbox: {e}"))?;
    parse_overview_list(&html, false, &opts.since, opts.limit, cli.timezone())
}

/// Fetches sent messages from the overview sent page.
pub async fn list_sent(cli: &Client, opts: SentOptions) -> anyhow::Result<ListResult> {
    let html = cli
        .get_text("/overview/sent")
        .await
        .map_err(|e| anyhow!("fetch overview sent: {e}"))?;
    parse_overview_list(&html, true, &opts.since, opts.limit, cli.timezone())
}

/// Fetches a single message from the overview detail page.
pub async fn get_message(cli: &Client, id_or_uid: &str) -> anyhow::Result<FullMessage> {
    let num = super::message::normalize_message_id(id_or_uid)?;
    let path = format!("/messages/detail?message={num}");
    let html = cli
        .get_text(&path)
        .await
        .map_err(|e| anyhow!("fetch overview message {id_or_uid}: {e}"))?;
    parse_overview_message(num, &html, cli.timezone())
}

fn parse_overview_list(
    html: &str,
    is_sent: bool,
    since: &str,
    limit: i64,
    tz: &TimeZone,
) -> anyhow::Result<ListResult> {
    let since_ts = super::messages::parse_since_public(since, tz)
        .with_context(|| format!("invalid since {since:?}"))?;
    let doc = Html::parse_document(html);
    let mut result = ListResult::default();
    let cap = super::messages::normalize_limit_public(limit) as usize;

    for item in doc.select(&INBOX_ITEM_SEL) {
        match parse_overview_item(item, is_sent, tz) {
            Err(e) => {
                tracing::warn!("skipping overview row: {e}");
                result.parse_warnings.push(e.to_string());
                continue;
            }
            Ok(Some(msg)) => {
                if let Some(floor) = since_ts
                    && let Ok(t) = msg.date.parse::<jiff::Timestamp>()
                    && t < floor
                {
                    continue;
                }
                result.messages.push(msg);
                if result.messages.len() >= cap {
                    break;
                }
            }
            Ok(None) => {}
        }
    }

    if !result.parse_warnings.is_empty() && result.messages.is_empty() {
        // Only hard-fail when rows looked like real messages but missed required
        // fields (subject/date/sender). Missing IDs are guide/demo rows — skip.
        let hard = result
            .parse_warnings
            .iter()
            .any(|w| !w.contains("could not determine message id"));
        if hard {
            bail!(
                "overview page had rows but none parsed — schema may have drifted (first warning: {})",
                result.parse_warnings[0]
            );
        }
    }
    Ok(result)
}

/// Returns `None` when the row is not a message (events, homework, etc.).
fn parse_overview_item(
    item: ElementRef<'_>,
    is_sent: bool,
    tz: &TimeZone,
) -> anyhow::Result<Option<Message>> {
    let type_text = item
        .select(&OBJECT_TYPE_SEL)
        .next()
        .map(|e| collapse_whitespace(&e.text().collect::<String>()))
        .unwrap_or_default();
    if type_text != MESSAGE_TYPE {
        return Ok(None);
    }

    let id_num = match extract_message_id(item) {
        Ok(n) => n,
        Err(_) => return Ok(None),
    };
    let subject = extract_subject(item)?;
    if subject.is_empty() {
        bail!("row m-{id_num} missing subject");
    }

    let date = extract_list_date(item, tz)?;
    if date.is_empty() {
        bail!("row m-{id_num} missing date");
    }

    let mut msg = Message {
        id: format!("m-{id_num}"),
        number: id_num,
        date,
        subject,
        body_preview: item
            .select(&DESCRIPTION_SEL)
            .next()
            .map(|e| {
                truncate_runes(
                    &collapse_whitespace(&e.text().collect::<String>()),
                    BODY_PREVIEW_MAX_RUNES,
                )
            })
            .unwrap_or_default(),
        attachments: 0,
        ..Default::default()
    };

    if is_sent {
        msg.status = "Publikováno".to_string();
    } else {
        let sender = item
            .select(&CREATOR_SEL)
            .next()
            .map(|e| collapse_whitespace(&e.text().collect::<String>()))
            .filter(|s| !s.is_empty());
        if sender.is_none() {
            bail!("row m-{id_num} missing sender");
        }
        msg.sender = sender.unwrap();
    }

    Ok(Some(msg))
}

fn extract_message_id(item: ElementRef<'_>) -> anyhow::Result<i64> {
    if let Some(onclick) = item.value().attr("onclick") {
        if let Some(c) = MESSAGE_ID_RE.captures(onclick) {
            return c[1].parse().map_err(|_| anyhow!("invalid message id in onclick"));
        }
    }
    if let Some(link) = item.select(&OBJECT_NAME_A_SEL).next() {
        if let Some(href) = link.value().attr("href") {
            if let Some(c) = MESSAGE_ID_RE.captures(href) {
                return c[1].parse().map_err(|_| anyhow!("invalid message id in href"));
            }
        }
    }
    let outer = item.html();
    if let Some(c) = OBJECT_ID_RE.captures(&outer) {
        if outer.contains("datatemplate.message") {
            return c[1].parse().map_err(|_| anyhow!("invalid object id"));
        }
    }
    bail!("could not determine message id for overview row");
}

fn extract_subject(item: ElementRef<'_>) -> anyhow::Result<String> {
    if let Some(a) = item.select(&OBJECT_NAME_A_SEL).next() {
        return Ok(collapse_whitespace(&a.text().collect::<String>()));
    }
    if let Some(div) = item.select(&OBJECT_NAME_SEL).next() {
        return Ok(collapse_whitespace(&div.text().collect::<String>()));
    }
    Ok(String::new())
}

fn extract_list_date(item: ElementRef<'_>, tz: &TimeZone) -> anyhow::Result<String> {
    let time_el = item
        .select(&TIME_SEL)
        .next()
        .ok_or_else(|| anyhow!("missing time element"))?;
    let outer = time_el.html();
    let text = collapse_whitespace(&time_el.text().collect::<String>());

    let (y, mo, d) = if let Some(c) = OVERVIEW_DATE_RE.captures(&text) {
        (
            parse_i16(&c[3])?,
            parse_i8(&c[2])?,
            parse_i8(&c[1])?,
        )
    } else if let Some(c) = TITLE_DATE_RE.captures(&outer) {
        (parse_i16(&c[1])?, parse_i8(&c[2])?, parse_i8(&c[3])?)
    } else if text.to_ascii_lowercase().starts_with("včera") || outer.contains("date-yesterday") {
        let now = jiff::Zoned::now().with_time_zone(tz.clone());
        let d = now
            .checked_sub(jiff::Span::new().days(1))
            .map_err(|_| anyhow!("date overflow"))?
            .date();
        (d.year(), d.month(), d.day())
    } else if text.to_ascii_lowercase().starts_with("dnes") || outer.contains("date-today") {
        let d = jiff::Zoned::now().with_time_zone(tz.clone()).date();
        (d.year(), d.month(), d.day())
    } else {
        bail!("unrecognized overview date: {text}");
    };

    let (h, mi) = if let Some(c) = OVERVIEW_TIME_RE.captures(&text) {
        (parse_i8(&c[1])?, parse_i8(&c[2])?)
    } else {
        (0, 0)
    };

    civil_to_rfc3339(y, mo, d, h, mi, tz).ok_or_else(|| anyhow!("invalid overview date parts"))
}

fn parse_overview_message(num: i64, html: &str, tz: &TimeZone) -> anyhow::Result<FullMessage> {
    let doc = Html::parse_document(html);
    let subject = doc
        .select(&DETAIL_NAME_SEL)
        .next()
        .map(|e| collapse_whitespace(&e.text().collect::<String>()))
        .filter(|s| !s.is_empty())
        .ok_or_else(|| anyhow!("message {num}: subject not found on detail page"))?;

    let body_html = doc
        .select(&RICH_CONTENT_SEL)
        .next()
        .map(|e| e.html())
        .unwrap_or_default();
    let body_text = if body_html.is_empty() {
        String::new()
    } else {
        html_to_text(&body_html)
    };

    let author = extract_label_value(&doc, "Odesílatel:");
    let date = extract_label_value(&doc, "Datum:")
        .or_else(|| extract_label_value(&doc, "Odesláno:"))
        .and_then(|raw| parse_detail_date(&raw, tz))
        .unwrap_or_default();

    let attachments = collect_overview_attachments(&doc, tz);
    let recipients = collect_overview_recipients(&doc);

    Ok(FullMessage {
        id: format!("m-{num}"),
        number: num,
        subject,
        author: author.unwrap_or_default(),
        date,
        body_html,
        body_text,
        attachments,
        recipients,
        ..Default::default()
    })
}

fn extract_label_value(doc: &Html, label: &str) -> Option<String> {
    for row in doc.select(&FT_ROW_SEL) {
        let text = collapse_whitespace(&row.text().collect::<String>());
        if text.starts_with(label) {
            let val = text.strip_prefix(label).unwrap_or("").trim().to_string();
            if !val.is_empty() {
                return Some(val);
            }
        }
    }
    None
}

fn parse_detail_date(raw: &str, tz: &TimeZone) -> Option<String> {
    if let Some(c) = OVERVIEW_DATE_RE.captures(raw) {
        let y = c[1].parse().ok()?;
        let mo = c[2].parse().ok()?;
        let d = c[3].parse().ok()?;
        let (h, mi) = if let Some(tc) = OVERVIEW_TIME_RE.captures(raw) {
            (tc[1].parse().ok()?, tc[2].parse().ok()?)
        } else {
            (0, 0)
        };
        return civil_to_rfc3339(y, mo, d, h, mi, tz);
    }
    None
}

fn collect_overview_attachments(doc: &Html, _tz: &TimeZone) -> Vec<Attachment> {
    let mut out = Vec::new();
    for (i, link) in doc.select(&DOWNLOAD_LINK_SEL).enumerate() {
        let href = link.value().attr("href").unwrap_or_default();
        let name = collapse_whitespace(&link.text().collect::<String>());
        if href.is_empty() {
            continue;
        }
        out.push(Attachment {
            id: format!("{}@{}", i + 1, out.len() + 1),
            name: if name.is_empty() {
                format!("attachment-{}", i + 1)
            } else {
                name
            },
            url: href.to_string(),
            date: String::new(),
        });
    }
    out
}

fn collect_overview_recipients(doc: &Html) -> Vec<Recipient> {
    let mut out = Vec::new();
    for table in doc.select(&TABLE_SEL) {
        let header = collapse_whitespace(&table.text().collect::<String>());
        if !header.contains("Doručen") && !header.contains("Přečten") {
            continue;
        }
        for row in table.select(&TR_SEL).skip(1) {
            let cells: Vec<String> = row
                .select(&TD_SEL)
                .map(|c| collapse_whitespace(&c.text().collect::<String>()))
                .collect();
            if cells.is_empty() {
                continue;
            }
            out.push(Recipient {
                name: cells[0].clone(),
                read_at: cells.get(1).cloned().unwrap_or_default(),
                ..Default::default()
            });
        }
    }
    out
}

fn parse_i8(s: &str) -> anyhow::Result<i8> {
    s.trim().parse().map_err(|_| anyhow!("invalid integer {s}"))
}

fn parse_i16(s: &str) -> anyhow::Result<i16> {
    s.trim().parse().map_err(|_| anyhow!("invalid integer {s}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tz() -> TimeZone {
        TimeZone::get("Europe/Prague").unwrap()
    }

    fn inbox_row() -> &'static str {
        r#"<div class="item message inboxMessage shownType shownPeople clickable" onclick="window.location.href=&quot;/messages/detail?message=309696&quot;">
            <div class="column c2">
                <div class="object-name"><a href="/messages/detail?message=309696">co na patek</a></div>
                <div class="object-type">Zpráva</div>
                <div class="description">Vážení rodiče, jen připomínám TV.</div>
            </div>
            <div class="column c3">
                <div class="creator">Mgr. Lucie Škurková</div>
                <div class="time">3.&thinsp;9.&thinsp;2026, <span class="date_time">14:51</span></div>
            </div>
        </div>"#
    }

    #[test]
    fn parse_overview_inbox_row() {
        let doc = Html::parse_fragment(inbox_row());
        let item = doc.select(&INBOX_ITEM_SEL).next().unwrap();
        let msg = parse_overview_item(item, false, &tz()).unwrap().unwrap();
        assert_eq!(msg.id, "m-309696");
        assert_eq!(msg.subject, "co na patek");
        assert_eq!(msg.sender, "Mgr. Lucie Škurková");
        assert_eq!(msg.date, "2026-09-03T14:51:00+02:00");
    }

    #[test]
    fn skips_non_message_rows() {
        let row = r#"<div class="item message inboxMessage"><div class="object-type">Událost</div></div>"#;
        let doc = Html::parse_fragment(row);
        let item = doc.select(&INBOX_ITEM_SEL).next().unwrap();
        assert!(parse_overview_item(item, false, &tz()).unwrap().is_none());
    }

    #[test]
    fn parse_overview_message_detail() {
        let html = r#"<div class="messages_detail_table">
            <div class="ft_row"><span class="ft_c1">Předmět:</span><span class="detail-object-name">co na patek</span></div>
            <div class="ft_row"><span class="ft_c1">Text zprávy:</span><div class="rich_content"><p>Ahoj</p></div></div>
            <div class="ft_row"><span class="ft_c1">Odesílatel:</span><span class="ft_c2">Mgr. Lucie Škurková</span></div>
        </div>"#;
        let msg = parse_overview_message(309696, html, &tz()).unwrap();
        assert_eq!(msg.subject, "co na patek");
        assert_eq!(msg.author, "Mgr. Lucie Škurková");
        assert!(msg.body_text.contains("Ahoj"));
    }
}
