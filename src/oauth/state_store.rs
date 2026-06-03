//! On-disk persistence for the OAuth Authorization Server state (DCR client
//! registrations + refresh-token records), so a server restart/upgrade doesn't
//! invalidate every connected client. Without this, the in-memory store is lost
//! on restart and clients that cached their `client_id` hit
//! `invalid_request: unknown client_id` on their next `/authorize` and must be
//! removed + re-added.
//!
//! The file holds refresh tokens (bearer-equivalent for minting access tokens),
//! so it is written atomically with `0600` permissions, owner-only — the same
//! posture as the session cookie cache and the JWT-secret env file.

use std::path::Path;

use anyhow::{Context, anyhow};
use serde::Serialize;
use serde::de::DeserializeOwned;

/// Reads and parses the persisted state from `path`. A `NotFound` io error is
/// preserved (so the caller can treat a first run as "empty, not an error").
pub fn load<T: DeserializeOwned>(path: &Path) -> anyhow::Result<T> {
    // metadata() surfaces NotFound verbatim — the caller distinguishes it.
    let meta = std::fs::metadata(path)?;

    // The file contains refresh tokens; warn loudly if it became
    // group/world-readable (another local user could read live grants). Don't
    // reject — avoid breaking a file created under a looser historical umask.
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let perm = meta.permissions().mode() & 0o777;
        if perm & 0o077 != 0 {
            tracing::warn!(
                "oauth state {} is mode {:04o} (group/world-accessible); tighten with: chmod 600 {}",
                path.display(),
                perm,
                path.display()
            );
        }
    }
    #[cfg(not(unix))]
    let _ = &meta;

    let data = std::fs::read(path).with_context(|| format!("read {}", path.display()))?;
    serde_json::from_slice(&data).map_err(|e| anyhow!("parse {}: {e}", path.display()))
}

/// Writes `value` to `path` as JSON atomically (write-temp then rename) with
/// `0600` permissions. Parent dirs are created on demand with `0700`.
pub fn save<T: Serialize>(path: &Path, value: &T) -> anyhow::Result<()> {
    let dir = path
        .parent()
        .ok_or_else(|| anyhow!("oauth state path {} has no parent dir", path.display()))?;
    create_dir_secure(dir)?;

    let data = serde_json::to_vec(value).context("marshal oauth state")?;

    // NamedTempFile gives a unique name in the same dir so concurrent writes
    // can't clobber each other mid-write; mkstemp creates it 0600 on unix, set
    // it explicitly to be sure before the data lands.
    let mut tmp = tempfile::NamedTempFile::new_in(dir)
        .with_context(|| format!("create temp in {}", dir.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        tmp.as_file()
            .set_permissions(std::fs::Permissions::from_mode(0o600))
            .context("chmod temp oauth state file")?;
    }
    use std::io::Write;
    tmp.write_all(&data)
        .context("write temp oauth state file")?;
    tmp.as_file().sync_all().ok();
    tmp.persist(path)
        .map_err(|e| anyhow!("rename temp -> {}: {}", path.display(), e.error))?;
    Ok(())
}

#[cfg(unix)]
fn create_dir_secure(dir: &Path) -> anyhow::Result<()> {
    use std::os::unix::fs::DirBuilderExt;
    if dir.as_os_str().is_empty() {
        return Ok(());
    }
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
        .with_context(|| format!("create oauth state dir {}", dir.display()))
}

#[cfg(not(unix))]
fn create_dir_secure(dir: &Path) -> anyhow::Result<()> {
    if dir.as_os_str().is_empty() {
        return Ok(());
    }
    std::fs::create_dir_all(dir)
        .with_context(|| format!("create oauth state dir {}", dir.display()))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde::Deserialize;

    #[derive(Debug, PartialEq, Serialize, Deserialize)]
    struct Sample {
        a: String,
        b: Vec<i64>,
    }

    #[test]
    fn save_load_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("nested").join("oauth-state.json");
        let v = Sample {
            a: "mcp_abc".into(),
            b: vec![1, 2, 3],
        };
        save(&path, &v).unwrap();
        let got: Sample = load(&path).unwrap();
        assert_eq!(got, v);
    }

    #[test]
    fn load_missing_is_not_found() {
        let dir = tempfile::tempdir().unwrap();
        let err = load::<Sample>(&dir.path().join("absent.json")).unwrap_err();
        let io = err
            .downcast_ref::<std::io::Error>()
            .expect("io error preserved");
        assert_eq!(io.kind(), std::io::ErrorKind::NotFound);
    }

    #[cfg(unix)]
    #[test]
    fn saved_file_is_0600() {
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("oauth-state.json");
        save(
            &path,
            &Sample {
                a: "x".into(),
                b: vec![],
            },
        )
        .unwrap();
        let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "refresh tokens must be owner-only");
    }
}
