//! `download_attachments` — streams each attachment of a message into a local
//! directory. Port of Go's `internal/tools/attachments.go`.

use std::collections::HashMap;
use std::path::{Path, PathBuf};

use anyhow::{Context, anyhow, bail};
use serde::Serialize;

use super::message::{Attachment, get_message};
use crate::client::Client;

/// One entry per attachment Edookit listed, ordered as the message presents
/// them. A non-empty `error` means that single download failed (others
/// continued); a top-level error means the call couldn't even start.
#[derive(Debug, Clone, Serialize)]
pub struct DownloadResult {
    pub message_id: String,
    pub directory: String,
    pub files: Vec<DownloadedFile>,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct DownloadedFile {
    pub name: String,
    pub path: String,
    pub bytes: i64,
    #[serde(skip_serializing_if = "is_false")]
    pub skipped: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
}

fn is_false(b: &bool) -> bool {
    !*b
}

#[derive(Debug, Default, Clone)]
pub struct DownloadOptions {
    pub dest_dir: String,
    /// If false, an existing file at the destination is left untouched.
    pub overwrite: bool,
}

/// Resolves the message ID, downloads every (non-trashed) attachment via the
/// authenticated session, and writes each to `dest_dir/<original-filename>`.
/// Partial failures don't abort the loop.
pub async fn download_attachments(
    cli: &Client,
    message_id: &str,
    opts: DownloadOptions,
) -> anyhow::Result<DownloadResult> {
    let msg = get_message(cli, message_id).await?;

    let dest_dir = resolve_dest_dir(&opts.dest_dir, &msg.id)?;
    create_dir_secure(&dest_dir).with_context(|| format!("create destination dir {}", dest_dir.display()))?;

    let mut res = DownloadResult {
        message_id: msg.id.clone(),
        directory: dest_dir.display().to_string(),
        files: Vec::with_capacity(msg.attachments.len()),
    };

    // Tracks final base names taken inside this call so a duplicate filename
    // gets a "-2"/"-3" suffix instead of being skipped or clobbered.
    let mut used_names: HashMap<String, ()> = HashMap::new();
    for a in &msg.attachments {
        let entry = download_one(cli, a, &dest_dir, opts.overwrite, &mut used_names).await;
        res.files.push(entry);
    }
    Ok(res)
}

async fn download_one(
    cli: &Client,
    a: &Attachment,
    dest_dir: &Path,
    overwrite: bool,
    used_names: &mut HashMap<String, ()>,
) -> DownloadedFile {
    let mut out = DownloadedFile {
        name: a.name.clone(),
        ..Default::default()
    };
    if let Some(reason) = validate_attachment(a) {
        out.error = reason;
        return out;
    }
    let dst = match plan_destination(&a.name, dest_dir, used_names) {
        Ok(d) => d,
        Err(reason) => {
            out.error = reason;
            return out;
        }
    };
    out.path = dst.display().to_string();
    stream_to_dest(cli, &a.url, dest_dir, &dst, overwrite, out).await
}

/// Cheap up-front checks not depending on the destination dir: a present URL,
/// and a base name that isn't a traversal sentinel or Windows-reserved.
fn validate_attachment(a: &Attachment) -> Option<String> {
    if a.url.trim().is_empty() {
        return Some("attachment has no download URL".to_string());
    }
    let safe = base_name(&a.name);
    match safe.as_deref() {
        None | Some("") | Some(".") | Some("..") => {
            Some(format!("attachment name {:?} is not safe to use as a filename", a.name))
        }
        Some(name) => windows_unsafe_name(name).map(|r| format!("attachment name {:?} rejected: {}", a.name, r)),
    }
}

/// Computes the final on-disk path, applying within-call dedup and verifying
/// the result stays inside `dest_dir`.
fn plan_destination(
    name: &str,
    dest_dir: &Path,
    used_names: &mut HashMap<String, ()>,
) -> Result<PathBuf, String> {
    let base = base_name(name).ok_or_else(|| format!("attachment name {name:?} is not safe to use as a filename"))?;
    let safe = unique_filename(&base, used_names);
    used_names.insert(safe.to_lowercase(), ());

    let dst = dest_dir.join(&safe);
    // Containment: the chosen name is a single component, so dst's parent must
    // be exactly dest_dir. (Defends against volume-rooted Windows names too.)
    if dst.parent() != Some(dest_dir) {
        return Err(format!("attachment name {name:?} would escape destination dir"));
    }
    Ok(dst)
}

/// Writes the body of `url` to a temp file in `dest_dir`, then commits it to
/// `dst`: `persist_noclobber` (no-overwrite, race-free — fails if dst exists)
/// or `persist` (overwrite, atomic replace). A reader never sees a partial file.
async fn stream_to_dest(
    cli: &Client,
    url: &str,
    dest_dir: &Path,
    dst: &Path,
    overwrite: bool,
    mut out: DownloadedFile,
) -> DownloadedFile {
    // Fast path: skip an already-present file without downloading. Correctness
    // for the mid-download race is still enforced by persist_noclobber below.
    if !overwrite && let Ok(meta) = std::fs::metadata(dst) {
        out.skipped = true;
        out.bytes = meta.len() as i64;
        return out;
    }

    let mut tmp = match tempfile::Builder::new().prefix(".edookit-download-").suffix(".part").tempfile_in(dest_dir) {
        Ok(t) => t,
        Err(e) => {
            out.error = format!("create temp in {}: {e}", dest_dir.display());
            return out;
        }
    };
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if let Err(e) = tmp.as_file().set_permissions(std::fs::Permissions::from_mode(0o600)) {
            out.error = format!("chmod temp: {e}");
            return out;
        }
    }

    let n = match cli.get_to(url, tmp.as_file_mut()).await {
        Ok(n) => n,
        Err(e) => {
            out.error = format!("download: {e}");
            return out;
        }
    };

    if !overwrite {
        // persist_noclobber: dst becomes visible only as a complete file; an
        // existing dst makes it fail with AlreadyExists, so we keep it.
        match tmp.persist_noclobber(dst) {
            Ok(_) => {
                out.bytes = n as i64;
                tracing::info!("downloaded {n} bytes -> {}", dst.display());
            }
            Err(e) if e.error.kind() == std::io::ErrorKind::AlreadyExists => {
                out.skipped = true;
                if let Ok(meta) = std::fs::metadata(dst) {
                    out.bytes = meta.len() as i64;
                }
            }
            Err(e) => {
                out.error = format!("link -> {}: {}", dst.display(), e.error);
            }
        }
        return out;
    }

    // overwrite: atomic replace.
    match tmp.persist(dst) {
        Ok(_) => {
            out.bytes = n as i64;
            tracing::info!("downloaded {n} bytes -> {}", dst.display());
        }
        Err(e) => {
            out.error = format!("rename -> {}: {}", dst.display(), e.error);
        }
    }
    out
}

/// Returns `base` if free within this call, else "<stem>-N.<ext>" with the
/// smallest free N >= 2. Keys are lowercased so case-insensitive filesystems
/// don't silently alias.
fn unique_filename(base: &str, used_names: &HashMap<String, ()>) -> String {
    if !used_names.contains_key(&base.to_lowercase()) {
        return base.to_string();
    }
    let p = Path::new(base);
    let ext = p.extension().and_then(|e| e.to_str()).map(|e| format!(".{e}")).unwrap_or_default();
    let stem = base.strip_suffix(&ext).unwrap_or(base);
    let mut n = 2;
    loop {
        let candidate = format!("{stem}-{n}{ext}");
        if !used_names.contains_key(&candidate.to_lowercase()) {
            return candidate;
        }
        n += 1;
    }
}

/// `filepath.Base` equivalent: the final path component, or `None` for paths
/// with no usable file name (`""`, `"."`, `".."`, `"/"`).
fn base_name(name: &str) -> Option<String> {
    Path::new(name).file_name().and_then(|s| s.to_str()).map(|s| s.to_string())
}

/// Applies the default-path policy and expands a leading tilde. Empty →
/// `<temp-dir>/edookit-mcp/<message-id>`. Relative paths are rejected (the MCP
/// server's cwd is not a stable anchor).
fn resolve_dest_dir(raw: &str, msg_id: &str) -> anyhow::Result<PathBuf> {
    if raw.is_empty() {
        return Ok(std::env::temp_dir().join("edookit-mcp").join(msg_id));
    }
    if raw == "~" || raw.starts_with("~/") {
        let home = dirs::home_dir().ok_or_else(|| anyhow!("expand ~ in {raw:?}: no home dir"))?;
        return Ok(if raw == "~" { home } else { home.join(&raw[2..]) });
    }
    if !Path::new(raw).is_absolute() {
        bail!("destination_dir {raw:?} must be absolute or start with ~/ (the MCP server's cwd is not a stable anchor)");
    }
    Ok(PathBuf::from(raw))
}

#[cfg(unix)]
fn create_dir_secure(dir: &Path) -> std::io::Result<()> {
    use std::os::unix::fs::DirBuilderExt;
    std::fs::DirBuilder::new().recursive(true).mode(0o700).create(dir)
}
#[cfg(not(unix))]
fn create_dir_secure(dir: &Path) -> std::io::Result<()> {
    std::fs::create_dir_all(dir)
}

#[cfg(windows)]
fn windows_unsafe_name(name: &str) -> Option<String> {
    const RESERVED: &[&str] = &[
        "CON", "PRN", "AUX", "NUL", "COM0", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7",
        "COM8", "COM9", "LPT0", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
    ];
    for c in name.chars() {
        if (c as u32) < 32 {
            return Some("contains a control character (Windows-reserved)".into());
        }
        match c {
            ':' => return Some("contains ':' (would create an NTFS Alternate Data Stream on Windows)".into()),
            '<' | '>' | '"' | '|' | '?' | '*' => return Some(format!("contains Windows-reserved character {c:?}")),
            _ => {}
        }
    }
    if let Some(last) = name.chars().last()
        && (last == ' ' || last == '.')
    {
        return Some("ends with space or '.' (Windows silently strips trailing space/dot)".into());
    }
    let stem = name.split_once('.').map(|(s, _)| s).unwrap_or(name);
    if RESERVED.contains(&stem.to_uppercase().as_str()) {
        return Some(format!("basename {stem:?} is a reserved Windows device name"));
    }
    None
}
#[cfg(not(windows))]
fn windows_unsafe_name(_name: &str) -> Option<String> {
    None
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::client::{Client, Config, LoginCookie, LoginFn};
    use std::sync::Arc;
    use std::time::Duration;
    use wiremock::matchers::{method, path as mpath, query_param};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    #[test]
    fn resolve_dest_dir_policy() {
        // empty → temp/edookit-mcp/<id>
        let d = resolve_dest_dir("", "m-1").unwrap();
        assert!(d.ends_with("edookit-mcp/m-1"));
        // absolute passes through
        assert_eq!(resolve_dest_dir("/tmp/x", "m-1").unwrap(), PathBuf::from("/tmp/x"));
        // relative rejected
        assert!(resolve_dest_dir("rel/path", "m-1").is_err());
        // ~ expands
        assert!(resolve_dest_dir("~", "m-1").is_ok());
    }

    #[test]
    fn plan_destination_dedup_and_containment() {
        let dir = PathBuf::from("/dest");
        let mut used = HashMap::new();
        let a = plan_destination("report.pdf", &dir, &mut used).unwrap();
        assert_eq!(a, PathBuf::from("/dest/report.pdf"));
        // case-insensitive dedup: second "Report.PDF" gets -2
        let b = plan_destination("Report.PDF", &dir, &mut used).unwrap();
        assert_eq!(b, PathBuf::from("/dest/Report-2.PDF"));
    }

    #[test]
    fn traversal_name_collapses_to_basename() {
        let dir = PathBuf::from("/dest");
        let mut used = HashMap::new();
        // file_name of a traversal path is just the leaf, landing inside dest.
        let d = plan_destination("../../etc/passwd", &dir, &mut used).unwrap();
        assert_eq!(d, PathBuf::from("/dest/passwd"));
    }

    #[test]
    fn validate_attachment_rejects_empty_url() {
        let a = Attachment { id: "1".into(), name: "x.txt".into(), url: "".into(), date: String::new() };
        assert!(validate_attachment(&a).is_some());
    }

    #[test]
    fn windows_unsafe_name_is_noop_off_windows() {
        // On non-Windows, otherwise-valid Unix names are allowed.
        assert!(windows_unsafe_name("CON").is_none() || cfg!(windows));
    }

    fn build_client(uri: &str) -> Client {
        let login_fn: LoginFn = Arc::new(|| Box::pin(async { Ok(vec![LoginCookie::new("X-EdooAuthToken", "tok")]) }));
        let mut cfg = Config::new(uri, "u", "p");
        cfg.retry_base_delay = Duration::from_millis(1);
        cfg.login_fn = Some(login_fn);
        Client::new(cfg).unwrap()
    }

    fn message_edit_with_attachment(download_url: &str) -> serde_json::Value {
        serde_json::json!({
            "authenticated": true,
            "components": {"workspace": [{
                "DOMTarget": "__lc_Form_Message",
                "data": {"__form_panel_main": [{"items": [
                    {"name": "name", "val": "Subj"},
                    {"name": "object_status", "val": "<span>Publikováno</span>"},
                    {"name": "description__editor", "readValue": "<p>body</p>"}
                ]}]}
            }, {
                "DOMTarget": "__lc_Fileviewer_Slave_datatemplate_message",
                "data": {"data": [
                    {"id": "1@1", "name": "schedule.pdf", "link": download_url, "date": 0, "trashed": false}
                ]}
            }]}
        })
    }

    #[tokio::test]
    async fn downloads_attachment_to_dir() {
        let server = MockServer::start().await;
        Mock::given(method("GET")).and(mpath("/")).respond_with(ResponseTemplate::new(200)).mount(&server).await;
        let dl_url = format!("{}/handler/download/file1", server.uri());
        Mock::given(method("GET"))
            .and(mpath("/handler/page/message-edit"))
            .and(query_param("__index", "1"))
            .respond_with(ResponseTemplate::new(200).set_body_json(message_edit_with_attachment(&dl_url)))
            .mount(&server)
            .await;
        Mock::given(method("GET"))
            .and(mpath("/handler/download/file1"))
            .respond_with(
                ResponseTemplate::new(200)
                    .insert_header("Content-Type", "application/octet-stream")
                    .set_body_bytes(b"%PDF-1.4 hello".to_vec()),
            )
            .mount(&server)
            .await;

        let tmp = tempfile::tempdir().unwrap();
        let cli = build_client(&server.uri());
        let res = download_attachments(
            &cli,
            "m-1",
            DownloadOptions { dest_dir: tmp.path().display().to_string(), overwrite: false },
        )
        .await
        .unwrap();

        assert_eq!(res.files.len(), 1);
        assert_eq!(res.files[0].name, "schedule.pdf");
        assert_eq!(res.files[0].error, "");
        assert_eq!(res.files[0].bytes, 14);
        assert!(std::fs::metadata(tmp.path().join("schedule.pdf")).is_ok());
    }
}
