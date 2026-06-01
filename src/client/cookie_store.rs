//! On-disk persistence of session cookies between runs, so chromium doesn't
//! have to launch on every startup. Port of Go's `internal/client/cookie_store.go`.
//!
//! The file stores only name/value pairs (Go's `cookiejar.Cookies` likewise
//! exposes only name+value), re-scoped to the base URL host on load. Edookit's
//! persistent auth tokens (`X-EdooAuthToken` / `X-Auth-Id`) are what actually
//! authenticate the next session; a warmup `GET /` resurrects the PHP session
//! from them.

use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, anyhow, bail};
use serde::{Deserialize, Serialize};

// Re-export the external `cookie_store` crate's types under stable local names
// so the rest of the `client` module (whose own submodule is also named
// `cookie_store`) refers to them without the `::cookie_store` extern-crate
// path. `CookieStoreImpl` is the in-memory store the swappable `Jar` wraps.
pub use ::cookie_store::{CookieStore as CookieStoreImpl, RawCookie};

/// A fresh empty in-memory cookie store. Centralized so the public-suffix
/// configuration (supercookie protection) is set in one place.
pub fn new_store() -> CookieStoreImpl {
    CookieStoreImpl::default()
}

/// Bounds how far a cached file's `captured_at` may sit in the future before we
/// distrust it. A future timestamp (clock skew or tampering) would make the
/// computed age negative and keep the cache "fresh" forever, silently defeating
/// `cookie_max_age`. A few minutes of tolerance absorbs ordinary skew.
const MAX_COOKIE_CLOCK_SKEW: i64 = 5 * 60;

/// One persisted cookie. Only name/value survive a round-trip — that mirrors
/// the Go jar, which exposes nothing else through its `Cookies` accessor.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StoredCookie {
    pub name: String,
    pub value: String,
}

/// On-disk format. `captured_at_unix` expires the cache after `cookie_max_age`
/// regardless of the cookies' own attributes (Edookit's session cookies carry
/// no `Expires`).
#[derive(Debug, Serialize, Deserialize)]
struct CookieFile {
    captured_at_unix: i64,
    base_url: String,
    cookies: Vec<StoredCookie>,
}

fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Standard per-user cache location, `<UserCacheDir>/edookit-mcp/cookies.json`.
/// On macOS that resolves to `~/Library/Caches/edookit-mcp/cookies.json`.
pub fn default_cookie_cache_path() -> anyhow::Result<PathBuf> {
    let cache = dirs::cache_dir().ok_or_else(|| anyhow!("user cache dir: not available"))?;
    // Deliberately a separate dir from the Go build's `edookit-mcp/` so the two
    // binaries don't fight over one cookies.json (different on-disk format) when
    // run side by side for comparison.
    Ok(cache.join("edookit-mcp-rs").join("cookies.json"))
}

/// Reads cached cookies from `path` and returns them with their age. Returns a
/// `NotFound` error (preserved so callers can detect it) if the file is absent.
/// Mismatched base URL or unreadable payload is reported as an error so the
/// caller can decide to re-login.
pub fn load_cookies(path: &Path, base_url: &str) -> anyhow::Result<(Vec<StoredCookie>, Duration)> {
    // metadata() surfaces NotFound verbatim — the caller distinguishes it.
    let meta = std::fs::metadata(path)?;

    // These cookies grant live session access; save_cookies writes them 0600.
    // A group/world-readable cache means another local user may read the
    // session — warn loudly (we don't reject, to avoid breaking caches created
    // under a looser historical umask). POSIX perm bits are only meaningful
    // off Windows.
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let perm = meta.permissions().mode() & 0o777;
        if perm & 0o077 != 0 {
            tracing::warn!(
                "cookie cache {} is mode {:04o} (group/world-accessible); tighten with: chmod 600 {}",
                path.display(),
                perm,
                path.display()
            );
        }
    }
    #[cfg(not(unix))]
    let _ = &meta;

    let data = std::fs::read(path).with_context(|| format!("read {}", path.display()))?;
    let cf: CookieFile =
        serde_json::from_slice(&data).map_err(|e| anyhow!("parse {}: {}", path.display(), e))?;

    if cf.base_url != base_url {
        bail!("cached cookies are for {}, not {}", cf.base_url, base_url);
    }
    if cf.cookies.is_empty() {
        bail!("cookie file is empty");
    }
    // Distrust a captured_at far in the future — see MAX_COOKIE_CLOCK_SKEW.
    let now = now_unix();
    let skew = cf.captured_at_unix - now;
    if skew > MAX_COOKIE_CLOCK_SKEW {
        bail!(
            "cookie file {} captured_at is {}s in the future; treating as a cache miss",
            path.display(),
            skew
        );
    }
    let age = Duration::from_secs((now - cf.captured_at_unix).max(0) as u64);
    Ok((cf.cookies, age))
}

/// Writes cookies to `path` atomically (write-temp then rename) with 0600
/// permissions so the file is readable only by the owner. Parent dirs are
/// created on demand with 0700.
pub fn save_cookies(path: &Path, base_url: &str, cookies: Vec<StoredCookie>) -> anyhow::Result<()> {
    let dir = path
        .parent()
        .ok_or_else(|| anyhow!("cookie path {} has no parent dir", path.display()))?;
    create_dir_secure(dir)?;

    let cf = CookieFile {
        captured_at_unix: now_unix(),
        base_url: base_url.to_string(),
        cookies,
    };
    let data = serde_json::to_vec_pretty(&cf).context("marshal cookies")?;

    // NamedTempFile gives a unique name in the same dir so two concurrent
    // processes can't clobber each other's in-flight write. mkstemp creates it
    // 0600 on unix; set it explicitly to be sure.
    let mut tmp = tempfile::NamedTempFile::new_in(dir)
        .with_context(|| format!("create temp in {}", dir.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        tmp.as_file()
            .set_permissions(std::fs::Permissions::from_mode(0o600))
            .context("chmod temp cookie file")?;
    }
    use std::io::Write;
    tmp.write_all(&data).context("write temp cookie file")?;
    tmp.as_file().sync_all().ok();
    // persist atomically replaces any existing file (rename on unix,
    // MoveFileEx(REPLACE_EXISTING) on Windows).
    tmp.persist(path)
        .map_err(|e| anyhow!("rename temp -> {}: {}", path.display(), e.error))?;
    Ok(())
}

#[cfg(unix)]
fn create_dir_secure(dir: &Path) -> anyhow::Result<()> {
    use std::os::unix::fs::DirBuilderExt;
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
        .with_context(|| format!("create cookie dir {}", dir.display()))
}

#[cfg(not(unix))]
fn create_dir_secure(dir: &Path) -> anyhow::Result<()> {
    std::fs::create_dir_all(dir).with_context(|| format!("create cookie dir {}", dir.display()))
}

#[cfg(test)]
mod tests {
    use super::*;

    const BASE: &str = "https://school.edookit.net";

    fn tmp_path() -> (tempfile::TempDir, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("nested").join("cookies.json");
        (dir, path)
    }

    #[test]
    fn save_load_roundtrip_preserves_name_value() {
        let (_g, path) = tmp_path();
        let cookies = vec![
            StoredCookie {
                name: "X-EdooAuthToken".into(),
                value: "tok123".into(),
            },
            StoredCookie {
                name: "X-Auth-Id".into(),
                value: "id456".into(),
            },
        ];
        save_cookies(&path, BASE, cookies).unwrap();
        let (loaded, age) = load_cookies(&path, BASE).unwrap();
        assert_eq!(loaded.len(), 2);
        assert_eq!(loaded[0].name, "X-EdooAuthToken");
        assert_eq!(loaded[0].value, "tok123");
        assert!(age < Duration::from_secs(60), "fresh save has small age");
    }

    #[test]
    fn load_missing_file_is_not_found() {
        let (_g, path) = tmp_path();
        let err = load_cookies(&path, BASE).unwrap_err();
        let io = err
            .downcast_ref::<std::io::Error>()
            .expect("io error preserved");
        assert_eq!(io.kind(), std::io::ErrorKind::NotFound);
    }

    #[test]
    fn base_url_mismatch_rejected() {
        let (_g, path) = tmp_path();
        save_cookies(
            &path,
            BASE,
            vec![StoredCookie {
                name: "a".into(),
                value: "b".into(),
            }],
        )
        .unwrap();
        let err = load_cookies(&path, "https://other.edookit.net").unwrap_err();
        assert!(err.to_string().contains("cached cookies are for"));
    }

    #[test]
    fn empty_cookie_list_rejected() {
        let (_g, path) = tmp_path();
        create_dir_secure(path.parent().unwrap()).unwrap();
        let json = format!(
            r#"{{"captured_at_unix":{},"base_url":"{BASE}","cookies":[]}}"#,
            now_unix()
        );
        std::fs::write(&path, json).unwrap();
        let err = load_cookies(&path, BASE).unwrap_err();
        assert!(err.to_string().contains("empty"));
    }

    #[test]
    fn future_captured_at_rejected() {
        let (_g, path) = tmp_path();
        create_dir_secure(path.parent().unwrap()).unwrap();
        let future = now_unix() + 3600;
        let json = format!(
            r#"{{"captured_at_unix":{future},"base_url":"{BASE}","cookies":[{{"name":"a","value":"b"}}]}}"#
        );
        std::fs::write(&path, json).unwrap();
        let err = load_cookies(&path, BASE).unwrap_err();
        assert!(err.to_string().contains("in the future"));
    }

    #[test]
    fn small_future_skew_tolerated() {
        let (_g, path) = tmp_path();
        create_dir_secure(path.parent().unwrap()).unwrap();
        let near = now_unix() + 60; // within the 5-minute skew window
        let json = format!(
            r#"{{"captured_at_unix":{near},"base_url":"{BASE}","cookies":[{{"name":"a","value":"b"}}]}}"#
        );
        std::fs::write(&path, json).unwrap();
        assert!(load_cookies(&path, BASE).is_ok());
    }

    #[cfg(unix)]
    #[test]
    fn saved_file_is_0600() {
        use std::os::unix::fs::PermissionsExt;
        let (_g, path) = tmp_path();
        save_cookies(
            &path,
            BASE,
            vec![StoredCookie {
                name: "a".into(),
                value: "b".into(),
            }],
        )
        .unwrap();
        let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "session cookies must be owner-only");
    }
}
