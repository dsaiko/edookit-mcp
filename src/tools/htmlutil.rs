//! Shared HTML/text utilities for the parsers. The Go version leans on
//! goquery; here we use `scraper` (html5ever + CSS selectors) for selection and
//! walk the underlying `ego_tree` directly for the text renderers.

use std::sync::LazyLock;

use scraper::{ElementRef, Html, Selector};

// Row/fragment HTML is wrapped in a sentinel `<edk-root>` element and parsed as
// a fragment, so the row's top-level nodes are exactly the children of one
// unambiguous element (mirrors Go wrapping rows in `<div>` then taking
// `body > div`). A custom element name can't collide with real row content.
static ROOT_SEL: LazyLock<Selector> = LazyLock::new(|| Selector::parse("edk-root").unwrap());

/// Parses an HTML fragment wrapped in the sentinel root element.
pub fn parse_fragment(html: &str) -> Html {
    Html::parse_fragment(&format!("<edk-root>{html}</edk-root>"))
}

/// Returns the sentinel root element whose children are the fragment's
/// top-level nodes.
pub fn root_of(doc: &Html) -> Option<ElementRef<'_>> {
    doc.select(&ROOT_SEL).next()
}

/// Truncates `s` to at most `max` runes, appending `…` if anything was cut.
/// Counting in runes keeps multi-byte UTF-8 (Czech diacritics) intact.
pub fn truncate_runes(s: &str, max: usize) -> String {
    if max == 0 {
        return s.to_string();
    }
    let chars: Vec<char> = s.chars().collect();
    if chars.len() <= max {
        return s.to_string();
    }
    let mut out: String = chars[..max].iter().collect();
    out.push('…');
    out
}

/// Collapses internal whitespace on each line, dedupes consecutive blank lines,
/// and trims trailing blanks. Leading blank state starts true so a stray blank
/// line at the very top is suppressed.
pub fn collapse_text_lines(raw: &str) -> String {
    let mut out: Vec<String> = Vec::new();
    let mut prev_blank = true;
    for line in raw.split('\n') {
        let collapsed: String = line.split_whitespace().collect::<Vec<_>>().join(" ");
        if collapsed.is_empty() {
            if prev_blank {
                continue;
            }
            prev_blank = true;
            out.push(String::new());
        } else {
            prev_blank = false;
            out.push(collapsed);
        }
    }
    while out.last().is_some_and(|s| s.is_empty()) {
        out.pop();
    }
    out.join("\n")
}

/// Collapses any run of Unicode whitespace (including the NBSP html5ever emits
/// from `&nbsp;`) to single spaces and trims — Go's `strings.Fields`/`Join`.
pub fn collapse_whitespace(s: &str) -> String {
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// Concatenates the text of `root`'s subtree, skipping any element subtree
/// whose tag name is `exclude` (Go's `selection.Find(tag).Remove()` then
/// `.Text()`).
pub fn text_excluding(root: ElementRef, exclude: &str) -> String {
    fn walk(node: ego_tree::NodeRef<'_, scraper::Node>, exclude: &str, out: &mut String) {
        for c in node.children() {
            if let Some(el) = c.value().as_element() {
                if el.name() == exclude {
                    continue;
                }
                walk(c, exclude, out);
            } else if let Some(t) = c.value().as_text() {
                out.push_str(t);
            }
        }
    }
    let mut out = String::new();
    walk(*root, exclude, &mut out);
    out
}
