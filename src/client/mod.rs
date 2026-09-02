//! Session-aware HTTP client. Port of Go's `internal/client/client.go`.
//!
//! Edookit federates login through Plus4U OIDC (uuidentity.plus4u.net), which
//! is rendered by a JS SPA and protected by reCAPTCHA. There is no static form
//! to POST to, so authentication is performed in a real chromium instance via
//! chromiumoxide (see [`login`]); the resulting session cookie is then handed
//! off to reqwest for all subsequent reads.

mod cookie_store;
pub mod login;

#[cfg(test)]
mod client_tests;

use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, anyhow, bail};
use arc_swap::ArcSwap;
use futures::StreamExt;
use parking_lot::Mutex;
use reqwest::header::{ACCEPT, HeaderValue, USER_AGENT};
use serde::Deserialize;
use serde::de::DeserializeOwned;
use url::{Host, Url};

pub use cookie_store::{StoredCookie, default_cookie_cache_path};

const DEFAULT_USER_AGENT: &str = "edookit-mcp/0.1 (+https://github.com/dsaiko/edookit-mcp)";
const SCHEME_HTTP: &str = "http";
const SCHEME_HTTPS: &str = "https";

/// Bytes of a streamed download buffered up front to classify it (real
/// attachment vs. stale-session login page / `authenticated:false` envelope).
/// Both artifacts are far smaller than this, so the window never truncates a
/// classification decision.
const DOWNLOAD_SNIFF_CAP: usize = 64 * 1024;

/// Default value for [`Config::download_hosts`] — Edookit's own file-storage
/// CDN (`dataN.edookit.net`), which `/handler/download/*` redirects to.
pub const DEFAULT_DOWNLOAD_HOSTS: &[&str] = &["*.edookit.net"];

/// Redirect hops we follow before giving up — the same budget as reqwest's own
/// default (`Policy::limited(10)`), which [`redirect_guard`]'s custom policy
/// replaces. Counted the way reqwest counts it: `Attempt::previous()` starts
/// with the *initial* URL, so the comparison must be `>` (not `>=`) to allow
/// the full ten hops.
const MAX_REDIRECT_HOPS: usize = 10;

/// Errors returned by the client's `get_*` methods. The
/// [`AttachmentTooLarge`](ClientError::AttachmentTooLarge) variant is a
/// sentinel matched by the inline-view tool; everything else carries a message.
#[derive(Debug, thiserror::Error)]
pub enum ClientError {
    /// Returned by [`Client::get_bytes`] when the body exceeds the caller's
    /// limit. Callers streaming to disk should use [`Client::get_to`], which is
    /// unbounded.
    #[error("attachment exceeds inline size limit")]
    AttachmentTooLarge,
    #[error("{0}")]
    Message(String),
}

impl ClientError {
    fn msg(s: impl Into<String>) -> Self {
        ClientError::Message(s.into())
    }
}

impl From<anyhow::Error> for ClientError {
    fn from(e: anyhow::Error) -> Self {
        ClientError::Message(e.to_string())
    }
}

/// A login callback that replaces the default chromiumoxide-driven OIDC flow.
/// Production leaves this `None`; tests inject a fake so `ensure_logged_in`'s
/// retry/invalidation paths can run without bringing up a real browser.
pub type LoginFn = Arc<
    dyn Fn() -> futures::future::BoxFuture<'static, anyhow::Result<Vec<LoginCookie>>> + Send + Sync,
>;

/// A cookie captured from the browser at login (or returned by a test
/// [`LoginFn`]). Only name/value are used when seeding the jar — for the
/// same-origin requests this client makes, host-only scoping is equivalent to
/// the captured domain/path, and persistence flattens to name/value anyway
/// (matching Go's jar, which exposes nothing else).
/// A cookie captured from a successful login. The jar is keyed by name/value
/// only (see the cookie-jar divergence in the README): the browser flow already
/// filters cookies to the target host, so the per-cookie domain/path/secure
/// attributes the Go port carried are not needed to replay the session.
#[derive(Debug, Clone)]
pub struct LoginCookie {
    pub name: String,
    pub value: String,
}

impl LoginCookie {
    pub fn new(name: impl Into<String>, value: impl Into<String>) -> Self {
        LoginCookie {
            name: name.into(),
            value: value.into(),
        }
    }
}

/// Controls how [`Client`] authenticates against Edookit.
pub struct Config {
    /// Your school's Edookit URL, e.g. `https://your-school-login.edookit.net`.
    pub base_url: String,
    /// Plus4U identity (email or login name).
    pub username: String,
    pub password: String,
    /// Permit a plain `http://` base pointing at a non-loopback host. Off by
    /// default: a real tenant is always https, and http:// would put the
    /// session cookie on the wire in the clear. Loopback is always allowed.
    pub allow_insecure_http: bool,
    /// Whether the chromium login instance is invisible. Default true.
    pub headless_login: bool,
    /// Caps the entire login flow. Default 90s.
    pub login_timeout: Duration,
    /// Where session cookies are persisted between runs. `None` disables it.
    pub cookie_cache_path: Option<std::path::PathBuf>,
    /// How long cached cookies are trusted before forcing a fresh login,
    /// regardless of the cookies' own attributes. Default 10h.
    pub cookie_max_age: Duration,
    /// Total HTTP attempts before giving up (1 initial + retries). Default 3;
    /// set 1 to disable retries.
    pub max_attempts: u32,
    /// Base for exponential backoff between retries. Default 500ms.
    pub retry_base_delay: Duration,
    /// School wall-clock timezone (Edookit row dates carry no offset suffix).
    /// `None` → Europe/Prague.
    pub timezone: Option<jiff::tz::TimeZone>,
    /// Extra hosts an **attachment download** may be redirected to, on top of
    /// the base origin. Edookit serves uploaded files from its own storage CDN:
    /// `/handler/download/file<uuid>` 302s to `https://dataN.edookit.net/v1/
    /// fetch/<uuid>?token=…`, which the same-origin session-expiry check would
    /// otherwise read as a login bounce. Patterns are exact hostnames or
    /// `*.suffix` wildcards (any *sub*domain of `suffix`; `suffix` itself does
    /// not match). Applies **only** to the download paths — `/handler/*` JSON
    /// calls and the warmup stay strictly same-origin, and a scheme downgrade
    /// (https base → http redirect) is never allowed. Empty ⇒ strict
    /// same-origin. Default [`DEFAULT_DOWNLOAD_HOSTS`].
    pub download_hosts: Vec<String>,
    /// Test hook replacing the chromedp login. Production leaves it `None`.
    pub login_fn: Option<LoginFn>,
}

impl Config {
    /// A minimal config with the required credentials and all-default knobs.
    pub fn new(
        base_url: impl Into<String>,
        username: impl Into<String>,
        password: impl Into<String>,
    ) -> Self {
        Config {
            base_url: base_url.into(),
            username: username.into(),
            password: password.into(),
            allow_insecure_http: false,
            headless_login: true,
            login_timeout: Duration::ZERO,
            cookie_cache_path: None,
            download_hosts: DEFAULT_DOWNLOAD_HOSTS
                .iter()
                .map(|s| (*s).to_string())
                .collect(),
            cookie_max_age: Duration::ZERO,
            max_attempts: 0,
            retry_base_delay: Duration::ZERO,
            timezone: None,
            login_fn: None,
        }
    }
}

/// Session-aware HTTP client. Performs OIDC login on demand, captures the
/// session cookie, and reuses it for subsequent requests.
pub struct Client {
    cfg: Config,
    /// Strict client: follows same-origin redirects only. Used for `/handler/*`
    /// JSON and the warmup.
    http: reqwest::Client,
    /// Download client: same-origin plus the [`Config::download_hosts`]
    /// allow-list. Separate from `http` because the guard has to live in the
    /// redirect *policy* (see [`redirect_guard`]) and the two request kinds
    /// need different allow-lists. Shares the cookie jar.
    download_http: reqwest::Client,
    base_url: Url,
    jar: Arc<Jar>,
    tz: jiff::tz::TimeZone,
    // Guards the logged-in flag AND serializes the login flow: a concurrent
    // burst of first calls all block here so only one chromium launch happens.
    logged_in: tokio::sync::Mutex<bool>,
}

impl Client {
    /// Constructs a client from `cfg`, applying defaults for zero-valued knobs.
    /// If a fresh cookie cache exists, cookies are preloaded but the client
    /// still starts logged-out: the first `ensure_logged_in` does a cheap
    /// warmup `GET /` that validates the cached session before any
    /// `/handler/page/*` hit.
    pub fn new(mut cfg: Config) -> anyhow::Result<Self> {
        if cfg.username.is_empty() || cfg.password.is_empty() {
            bail!("username and password are required");
        }
        if cfg.cookie_max_age.is_zero() {
            cfg.cookie_max_age = Duration::from_secs(10 * 3600);
        }
        if cfg.max_attempts == 0 {
            cfg.max_attempts = 3;
        }
        if cfg.retry_base_delay.is_zero() {
            cfg.retry_base_delay = Duration::from_millis(500);
        }
        if cfg.login_timeout.is_zero() {
            cfg.login_timeout = Duration::from_secs(90);
        }
        let tz = cfg.timezone.clone().unwrap_or_else(default_timezone);

        let base_url = parse_base_url(&cfg.base_url, cfg.allow_insecure_http)?;

        let jar = Arc::new(Jar::new());
        // The origin guard must sit in the redirect policy, not on the final
        // response: reqwest walks the whole chain itself, so a post-hoc check
        // of `resp.url()` would (a) already have contacted every intermediate
        // host — twice, once the re-login retry fires — and (b) accept a chain
        // that detours through a disallowed host and returns to an allowed one.
        let http = build_http(jar.clone(), redirect_guard(base_url.clone(), Vec::new()))?;
        // The download client sees the jar only for the tenant origin, so an
        // allow-listed CDN hop can neither be handed a cookie nor set one.
        let download_http = build_http(
            Arc::new(TenantOnlyCookies {
                jar: jar.clone(),
                base_url: base_url.clone(),
            }),
            redirect_guard(base_url.clone(), cfg.download_hosts.clone()),
        )?;

        let client = Client {
            cfg,
            http,
            download_http,
            base_url,
            jar,
            tz,
            logged_in: tokio::sync::Mutex::new(false),
        };
        if let Some(path) = client.cfg.cookie_cache_path.clone() {
            client.preload_cookies(&path);
        }
        Ok(client)
    }

    /// The timezone callers should use when interpreting Edookit's wall-clock
    /// timestamps (row dates have no offset suffix).
    pub fn timezone(&self) -> &jiff::tz::TimeZone {
        &self.tz
    }

    /// Forces a login if we don't already have a session. Normally callers
    /// don't need this — the `get_*` methods authenticate lazily. Exposed for
    /// smoke tests and eager-login flows.
    pub async fn ensure_logged_in(&self) -> Result<(), ClientError> {
        let mut guard = self.logged_in.lock().await;
        if *guard {
            return Ok(());
        }

        // Fast path: cookies already loaded (from cache). Warm them up; if the
        // session is still alive, we're done without launching chromium.
        if self.jar.has_cookies_for(&self.base_url) {
            match self.warmup().await {
                Ok(()) => {
                    *guard = true;
                    return Ok(());
                }
                Err(e) => {
                    tracing::info!("cached session invalid ({e}); falling back to fresh login");
                }
            }
        }

        let cookies = self
            .login()
            .await
            .map_err(|e| ClientError::msg(format!("oidc login: {e}")))?;
        self.jar.set_login_cookies(&self.base_url, &cookies);

        // Warm up so /handler/page/* calls find a valid PHP session.
        self.warmup()
            .await
            .map_err(|e| ClientError::msg(format!("warmup after login failed: {e}")))?;
        *guard = true;

        if let Some(path) = &self.cfg.cookie_cache_path {
            let exported = self.jar.export_name_values(&self.base_url);
            match cookie_store::save_cookies(path, &self.cfg.base_url, exported) {
                Ok(()) => tracing::info!("cached cookies to {}", path.display()),
                Err(e) => tracing::warn!("failed to cache cookies (non-fatal): {e}"),
            }
        }
        Ok(())
    }

    /// The cookies currently held for the target host (diagnostics only —
    /// name/value pairs; do not log these in production).
    pub fn session_cookies(&self) -> Vec<StoredCookie> {
        self.jar.export_name_values(&self.base_url)
    }

    /// Fetches `path` and decodes the JSON response into `T`. Re-authenticates
    /// once on session expiry (off-origin bounce or `authenticated:false`).
    pub async fn get_json<T: DeserializeOwned>(&self, path: &str) -> Result<T, ClientError> {
        let mut allow_retry = true;
        loop {
            self.ensure_logged_in().await?;
            self.preflight_same_origin(path)?;

            let req = self.new_request(path, true)?;
            let resp = self.send_retrying(req).await?;

            // Either the chain bounced off-origin (the policy declined the hop,
            // leaving us holding the 3xx) or it somehow ended off-origin.
            if resp.status().is_redirection() || !same_origin(resp.url(), &self.base_url) {
                if !allow_retry {
                    return Err(ClientError::msg("session expired and re-login failed"));
                }
                self.invalidate_session().await;
                allow_retry = false;
                continue;
            }
            let status = resp.status().as_u16();
            if status >= 400 {
                return Err(ClientError::msg(format!("GET {path}: HTTP {status}")));
            }
            let body = resp
                .bytes()
                .await
                .map_err(|e| ClientError::msg(format!("read body from {path}: {e}")))?;

            // Server may return HTTP 200 with authenticated:false instead of
            // bouncing. Treat it the same as session expiry.
            if parse_auth_envelope(&body) == Some(false) {
                if !allow_retry {
                    return Err(ClientError::msg(
                        "session reported authenticated=false and re-login failed",
                    ));
                }
                self.invalidate_session().await;
                allow_retry = false;
                continue;
            }

            return serde_json::from_slice(&body)
                .map_err(|e| ClientError::msg(format!("decode JSON from {path}: {e}")));
        }
    }

    /// Streams the body of `GET <path>` into `dst`, returning the byte count.
    /// Used for binary downloads where we don't want to buffer the whole file.
    /// `path` may be relative or an absolute (same-origin) URL. Writes at most
    /// `limit` bytes; a larger body yields [`ClientError::AttachmentTooLarge`]
    /// (a guard against a runaway or hostile response filling the disk).
    ///
    /// Diverges from the Go original (which rejected text/html and
    /// application/json outright): a bounded prefix is sniffed and run through
    /// [`classify_download_body`] — the same logic `get_bytes` uses — so genuine
    /// `.html`/`.json` attachments stream to disk while a stale-session login
    /// page or `authenticated:false` envelope still triggers re-login.
    pub async fn get_to<W: std::io::Write>(
        &self,
        path: &str,
        dst: &mut W,
        limit: u64,
    ) -> Result<u64, ClientError> {
        let mut allow_retry = true;
        loop {
            self.ensure_logged_in().await?;
            self.preflight_download_origin(path)
                .map_err(|e| ClientError::msg(format!("GET {path}: {e}")))?;

            let req = self.new_request(path, false)?;
            let resp = self.send_retrying_with(&self.download_http, req).await?;

            // Edookit redirects real file downloads to its storage CDN, so an
            // allow-listed hop is a success, not a login bounce. A 3xx here
            // means the policy refused the next hop.
            if resp.status().is_redirection() || !self.download_origin_ok(resp.url()) {
                if !allow_retry {
                    return Err(download_bounce_error(
                        &resp,
                        &self.base_url,
                        &self.cfg.download_hosts,
                    ));
                }
                self.invalidate_session().await;
                allow_retry = false;
                continue;
            }
            let status = resp.status().as_u16();
            if status >= 400 {
                return Err(ClientError::msg(format!("GET {path}: HTTP {status}")));
            }

            let ct = content_type_lower(&resp);
            // A tenant login page or an `authenticated:false` envelope can only
            // come from the tenant. An allow-listed CDN response is a file
            // whatever its content type, so it must skip the session sniff:
            // otherwise every legitimate HTML attachment would trigger a fresh
            // chromium login (and fail outright if that login failed), and a
            // CDN JSON that happens to carry `authenticated:false` could never
            // be returned at all.
            let from_tenant = same_origin(resp.url(), &self.base_url);
            let mut stream = resp.bytes_stream();

            // Buffer a bounded prefix so we can distinguish a real attachment
            // from a stale-session artifact without reading the whole (possibly
            // huge) body into memory. The auth envelope and login page both fit
            // easily within the sniff window.
            let mut sniff: Vec<u8> = Vec::new();
            let mut stream_drained = false;
            while sniff.len() < DOWNLOAD_SNIFF_CAP {
                match stream.next().await {
                    Some(chunk) => {
                        let chunk =
                            chunk.map_err(|e| ClientError::msg(format!("GET {path}: {e}")))?;
                        sniff.extend_from_slice(&chunk);
                    }
                    None => {
                        stream_drained = true;
                        break;
                    }
                }
            }

            match classify_session_body(from_tenant, &ct, &sniff, allow_retry) {
                DownloadDisposition::Reauth => {
                    self.invalidate_session().await;
                    allow_retry = false;
                    continue;
                }
                DownloadDisposition::Fail => {
                    return Err(ClientError::msg(format!(
                        "GET {path}: stale session (authenticated=false) and re-login failed"
                    )));
                }
                DownloadDisposition::Accept => {}
            }

            // Commit the sniffed prefix, then stream the remainder — enforcing
            // the byte cap throughout.
            if sniff.len() as u64 > limit {
                return Err(ClientError::AttachmentTooLarge);
            }
            dst.write_all(&sniff)
                .map_err(|e| ClientError::msg(format!("write to dst: {e}")))?;
            let mut written = sniff.len() as u64;

            if !stream_drained {
                while let Some(chunk) = stream.next().await {
                    let chunk = chunk.map_err(|e| ClientError::msg(format!("GET {path}: {e}")))?;
                    written += chunk.len() as u64;
                    if written > limit {
                        return Err(ClientError::AttachmentTooLarge);
                    }
                    dst.write_all(&chunk)
                        .map_err(|e| ClientError::msg(format!("write to dst: {e}")))?;
                }
            }
            return Ok(written);
        }
    }

    /// Fetches `GET <path>` fully into memory (up to `limit` bytes) and returns
    /// the body with its `Content-Type`. Returns
    /// [`ClientError::AttachmentTooLarge`] if the body would exceed `limit`.
    pub async fn get_bytes(
        &self,
        path: &str,
        limit: u64,
    ) -> Result<(Vec<u8>, String), ClientError> {
        let mut allow_retry = true;
        loop {
            self.ensure_logged_in().await?;
            self.preflight_download_origin(path)
                .map_err(|e| ClientError::msg(format!("GET {path}: {e}")))?;

            let req = self.new_request(path, false)?;
            let resp = self.send_retrying_with(&self.download_http, req).await?;

            // Edookit redirects real file downloads to its storage CDN, so an
            // allow-listed hop is a success, not a login bounce. A 3xx here
            // means the policy refused the next hop.
            if resp.status().is_redirection() || !self.download_origin_ok(resp.url()) {
                if !allow_retry {
                    return Err(download_bounce_error(
                        &resp,
                        &self.base_url,
                        &self.cfg.download_hosts,
                    ));
                }
                self.invalidate_session().await;
                allow_retry = false;
                continue;
            }
            let status = resp.status().as_u16();
            if status >= 400 {
                return Err(ClientError::msg(format!("GET {path}: HTTP {status}")));
            }
            let ct = resp
                .headers()
                .get(reqwest::header::CONTENT_TYPE)
                .and_then(|v| v.to_str().ok())
                .unwrap_or("")
                .to_string();

            // A tenant login page or an `authenticated:false` envelope can only
            // come from the tenant. An allow-listed CDN response is a file
            // whatever its content type, so it must skip the session sniff:
            // otherwise every legitimate HTML attachment would trigger a fresh
            // chromium login (and fail outright if that login failed), and a
            // CDN JSON that happens to carry `authenticated:false` could never
            // be returned at all.
            let from_tenant = same_origin(resp.url(), &self.base_url);

            let mut stream = resp.bytes_stream();
            let mut buf: Vec<u8> = Vec::new();
            while let Some(chunk) = stream.next().await {
                let chunk =
                    chunk.map_err(|e| ClientError::msg(format!("read body from {path}: {e}")))?;
                buf.extend_from_slice(&chunk);
                if buf.len() as u64 > limit {
                    return Err(ClientError::AttachmentTooLarge);
                }
            }

            match classify_session_body(from_tenant, &ct, &buf, allow_retry) {
                DownloadDisposition::Reauth => {
                    self.invalidate_session().await;
                    allow_retry = false;
                    continue;
                }
                DownloadDisposition::Fail => {
                    return Err(ClientError::msg(
                        "session reported authenticated=false and re-login failed",
                    ));
                }
                DownloadDisposition::Accept => return Ok((buf, ct)),
            }
        }
    }

    // --- internals ---

    fn preload_cookies(&self, path: &Path) {
        match cookie_store::load_cookies(path, &self.cfg.base_url) {
            Ok((cookies, age)) if age < self.cfg.cookie_max_age => {
                let n = cookies.len();
                self.jar.set_name_values(&self.base_url, &cookies);
                tracing::info!(
                    "loaded {n} cached cookies (age {age:?}); will verify on first call"
                );
            }
            Ok((_, age)) => {
                tracing::info!(
                    "cached cookies are stale (age {age:?} > max {:?}); will re-login on first request",
                    self.cfg.cookie_max_age
                );
            }
            Err(e) => {
                let missing = e
                    .downcast_ref::<std::io::Error>()
                    .is_some_and(|io| io.kind() == std::io::ErrorKind::NotFound);
                if !missing {
                    tracing::info!("cookie cache load failed: {e}");
                }
            }
        }
    }

    async fn login(&self) -> anyhow::Result<Vec<LoginCookie>> {
        if let Some(f) = &self.cfg.login_fn {
            return f().await;
        }
        login::login_via_browser(login::BrowserLoginConfig {
            base_url: self.base_url.to_string(),
            username: self.cfg.username.clone(),
            password: self.cfg.password.clone(),
            headless: self.cfg.headless_login,
            timeout: self.cfg.login_timeout,
        })
        .await
    }

    async fn invalidate_session(&self) {
        let mut guard = self.logged_in.lock().await;
        *guard = false;
        self.jar.reset();
    }

    async fn warmup(&self) -> Result<(), ClientError> {
        let req = self.new_request("/", false)?;
        let resp = self
            .send_retrying(req)
            .await
            .map_err(|e| ClientError::msg(format!("warmup GET /: {e}")))?;
        let final_url = resp.url().clone();
        let status = resp.status().as_u16();
        let bounced = resp.status().is_redirection() || !same_origin(&final_url, &self.base_url);
        let target = refused_target(&resp)
            .map(|u| origin_str(&u))
            .unwrap_or_default();
        let _ = resp.bytes().await; // drain

        if bounced {
            return Err(ClientError::msg(format!(
                "warmup bounced off-origin to {target} (session expired)"
            )));
        }
        if status >= 400 {
            return Err(ClientError::msg(format!("warmup got HTTP {status}")));
        }
        Ok(())
    }

    fn resolve(&self, path: &str) -> Result<Url, ClientError> {
        self.base_url
            .join(path)
            .map_err(|e| ClientError::msg(format!("parse request path {path:?}: {e}")))
    }

    /// Resolves `path` and rejects it before dispatch if it points off-origin.
    /// `new_request` accepts absolute URLs (attachment links arrive fully
    /// qualified), so a drifted or hostile value could otherwise be sent
    /// outbound — an SSRF footgun. The post-response same-origin check still
    /// catches mid-flight redirects.
    fn preflight_same_origin(&self, path: &str) -> Result<(), ClientError> {
        let resolved = self.resolve(path)?;
        if !same_origin(&resolved, &self.base_url) {
            return Err(ClientError::msg(format!(
                "refusing off-origin URL {} (must be same origin as {})",
                resolved.host_str().unwrap_or(""),
                self.base_url.host_str().unwrap_or("")
            )));
        }
        Ok(())
    }

    /// Whether a **download** may live at `url`: the base origin, or one of the
    /// [`Config::download_hosts`] patterns. A scheme downgrade is never
    /// allowed (an https base can only be redirected to https), so the
    /// allow-list cannot put a session or a token on the wire in the clear.
    fn download_origin_ok(&self, url: &Url) -> bool {
        origin_allowed(url, &self.base_url, &self.cfg.download_hosts)
    }

    /// Pre-dispatch guard for the download paths — the SSRF fence of
    /// [`Self::preflight_same_origin`], widened to the download allow-list
    /// because attachment URLs arrive fully qualified from Edookit and may
    /// point straight at its storage CDN.
    fn preflight_download_origin(&self, path: &str) -> Result<(), ClientError> {
        let resolved = self.resolve(path)?;
        let verdict = origin_verdict(&resolved, &self.base_url, &self.cfg.download_hosts);
        if !verdict.is_allowed() {
            return Err(ClientError::msg(format!(
                "refusing off-origin download URL {}: {} (tenant is {}, allow-list {:?})",
                origin_str(&resolved),
                verdict.advice(&self.base_url),
                origin_str(&self.base_url),
                self.cfg.download_hosts
            )));
        }
        Ok(())
    }

    fn new_request(&self, path: &str, accept_json: bool) -> Result<reqwest::Request, ClientError> {
        let url = self.resolve(path)?;
        let mut builder = self
            .http
            .get(url)
            .header(USER_AGENT, HeaderValue::from_static(DEFAULT_USER_AGENT));
        if accept_json {
            // Mark as XHR so the server returns JSON rather than the SPA loader.
            builder = builder
                .header(ACCEPT, HeaderValue::from_static("application/json"))
                .header(
                    "X-Requested-With",
                    HeaderValue::from_static("XMLHttpRequest"),
                );
        } else {
            builder = builder.header(
                ACCEPT,
                HeaderValue::from_static(
                    "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
                ),
            );
        }
        builder
            .build()
            .map_err(|e| ClientError::msg(format!("build request {path}: {e}")))
    }

    /// Dispatches `req` with transient-failure retry: net errors (other than
    /// timeout, which is caller-deadline intent) and HTTP 408/502/503/504
    /// trigger a retry with exponential backoff up to `max_attempts`. Other
    /// 5xx and all 4xx propagate immediately. Safe because every request is a
    /// GET; a non-idempotent verb would need gating on the method.
    async fn send_retrying(&self, req: reqwest::Request) -> Result<reqwest::Response, ClientError> {
        self.send_retrying_with(&self.http, req).await
    }

    /// As [`Self::send_retrying`], but over `http` — the download paths pass
    /// [`Self::download_http`], whose redirect policy honours the allow-list.
    async fn send_retrying_with(
        &self,
        http: &reqwest::Client,
        req: reqwest::Request,
    ) -> Result<reqwest::Response, ClientError> {
        let attempts = self.cfg.max_attempts.max(1);
        let mut last_err = String::new();
        for i in 0..attempts {
            if i > 0 {
                let delay = self.cfg.retry_base_delay.saturating_mul(1u32 << (i - 1));
                tracing::warn!(
                    "retrying {} {} after {:?} (attempt {}/{}, last: {})",
                    req.method(),
                    req.url().path(),
                    delay,
                    i + 1,
                    attempts,
                    last_err
                );
                tokio::time::sleep(delay).await;
            }
            let attempt = req
                .try_clone()
                .expect("GET requests with no body are always clonable");
            match http.execute(attempt).await {
                Err(e) => {
                    // A client-timeout is the caller's deadline, and a redirect
                    // failure (loop / hop budget, see `redirect_guard`) is
                    // deterministic — neither is worth burning the remaining
                    // attempts on. Other net errors retry.
                    if e.is_timeout() || e.is_redirect() {
                        return Err(ClientError::msg(e.to_string()));
                    }
                    last_err = e.to_string();
                }
                Ok(resp) => {
                    if is_transient_status(resp.status().as_u16()) {
                        last_err = format!("HTTP {}", resp.status().as_u16());
                        let _ = resp.bytes().await; // drain so the conn can be reused
                    } else {
                        return Ok(resp);
                    }
                }
            }
        }
        Err(ClientError::msg(format!(
            "after {attempts} attempt(s): {last_err}"
        )))
    }
}

/// Reports whether an HTTP status is one we expect to succeed on retry —
/// load-balancer / upstream timeouts and overload signals. Deliberately
/// excludes the rest of 5xx: a 500 from Edookit is more likely a deterministic
/// application bug than a transient condition, and retrying would mask it.
fn is_transient_status(code: u16) -> bool {
    matches!(code, 408 | 502 | 503 | 504)
}

fn content_type_lower(resp: &reqwest::Response) -> String {
    resp.headers()
        .get(reqwest::header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_ascii_lowercase()
}

/// Reports whether two URLs share the same origin (scheme + host + effective
/// port). Used to detect when a request was bounced to a foreign origin
/// (Plus4U after session expiry, a different port, a different scheme) — any of
/// which should trigger re-login rather than be treated as a valid response.
fn same_origin(a: &Url, b: &Url) -> bool {
    a.scheme() == b.scheme()
        && a.host_str() == b.host_str()
        && a.port_or_known_default() == b.port_or_known_default()
}

/// Builds a reqwest client with `cookies` as its cookie store and `policy`
/// governing redirects.
fn build_http<C: reqwest::cookie::CookieStore + 'static>(
    cookies: Arc<C>,
    policy: reqwest::redirect::Policy,
) -> anyhow::Result<reqwest::Client> {
    reqwest::Client::builder()
        .cookie_provider(cookies)
        .timeout(Duration::from_secs(20))
        .redirect(policy)
        .build()
        .context("build http client")
}

/// Redirect policy that vets **every hop before it is requested**: a hop is
/// followed only if it is same-origin with `base` or matches one of
/// `extra_hosts` (see [`origin_allowed`]).
///
/// The two failure modes are deliberately distinct, because they mean different
/// things to the caller:
///   * **Refused hop** → `stop()`, so reqwest hands back the 3xx and the caller
///     reads it as a session bounce worth one re-login (`Location` names the
///     host we declined).
///   * **Hop budget exhausted** → `error()`, i.e. a `reqwest::Error` with
///     `is_redirect()`. A redirect loop is deterministic, so it must NOT look
///     like an expired session: no re-login, no retry (see
///     [`Client::send_retrying_with`]), and no misleading advice about
///     `EDOOKIT_DOWNLOAD_HOSTS`.
fn redirect_guard(base: Url, extra_hosts: Vec<String>) -> reqwest::redirect::Policy {
    reqwest::redirect::Policy::custom(move |attempt| {
        if attempt.previous().len() > MAX_REDIRECT_HOPS {
            let msg = format!(
                "exceeded {MAX_REDIRECT_HOPS} redirect hops (loop?) at {}",
                attempt.url()
            );
            return attempt.error(msg);
        }
        if origin_allowed(attempt.url(), &base, &extra_hosts) {
            attempt.follow()
        } else {
            attempt.stop()
        }
    })
}

/// Why an origin was refused — the allow-list rejects on three independent
/// grounds, and the advice differs for each (adding a host to the allow-list
/// does nothing about a wrong port).
#[derive(Debug, PartialEq, Eq)]
enum OriginVerdict {
    Allowed,
    /// The hostname matches no allow-list pattern (or the list is empty).
    HostNotAllowed,
    /// Allow-listed host, but not on the tenant's scheme (downgrade refused).
    SchemeMismatch,
    /// Allow-listed host and scheme, but not on the tenant's effective port.
    PortMismatch,
}

impl OriginVerdict {
    fn is_allowed(&self) -> bool {
        *self == OriginVerdict::Allowed
    }

    /// The actionable half of an error message.
    fn advice(&self, base: &Url) -> String {
        match self {
            OriginVerdict::Allowed => String::new(),
            OriginVerdict::HostNotAllowed => {
                "host is not in EDOOKIT_DOWNLOAD_HOSTS (add it if it is a legitimate Edookit \
                 file store)"
                    .to_string()
            }
            OriginVerdict::SchemeMismatch => format!(
                "scheme must be {} like the tenant — a downgrade is never followed",
                base.scheme()
            ),
            OriginVerdict::PortMismatch => format!(
                "port must be {} like the tenant (an allow-listed host does not widen to other \
                 ports, because cookies are not port-scoped)",
                base.port_or_known_default().unwrap_or_default()
            ),
        }
    }
}

/// Whether `url` may be requested: the `base` origin itself, or — when
/// `extra_hosts` is non-empty (downloads only) — a host matching one of its
/// patterns on the *same scheme and port* as `base`.
///
/// The port is pinned deliberately: cookies are not port-scoped, so a hop to
/// `https://<same-host>:8443/` would still be sent the session cookie while
/// escaping [`same_origin`]. Requiring the base origin's effective port (443
/// in every real deployment) closes that without needing per-pattern ports.
fn origin_verdict(url: &Url, base: &Url, extra_hosts: &[String]) -> OriginVerdict {
    if same_origin(url, base) {
        return OriginVerdict::Allowed;
    }
    let host_ok = url.host_str().is_some_and(|host| {
        // The tenant's own host always counts as host-allowed: reaching here
        // with it means the *scheme or port* differed, and that is what the
        // caller needs to hear — including with an empty allow-list, where
        // "host is not allowed" would be actively misleading for an
        // https→http downgrade on the tenant itself.
        Some(host) == base.host_str()
            || extra_hosts
                .iter()
                .any(|pattern| host_matches_pattern(host, pattern))
    });
    if !host_ok {
        return OriginVerdict::HostNotAllowed;
    }
    if url.scheme() != base.scheme() {
        return OriginVerdict::SchemeMismatch;
    }
    if url.port_or_known_default() != base.port_or_known_default() {
        return OriginVerdict::PortMismatch;
    }
    OriginVerdict::Allowed
}

fn origin_allowed(url: &Url, base: &Url, extra_hosts: &[String]) -> bool {
    origin_verdict(url, base, extra_hosts).is_allowed()
}

/// `scheme://host:port` — the whole origin, so a refusal names what it refused
/// rather than just the hostname.
fn origin_str(url: &Url) -> String {
    match (url.host_str(), url.port_or_known_default()) {
        (Some(host), Some(port)) => format!("{}://{host}:{port}", url.scheme()),
        (Some(host), None) => format!("{}://{host}", url.scheme()),
        _ => url.to_string(),
    }
}

/// Where a response wanted to take us but we would not go: the `Location` of a
/// 3xx the redirect policy declined to follow, else the final URL.
fn refused_target(resp: &reqwest::Response) -> Option<Url> {
    if resp.status().is_redirection()
        && let Some(loc) = resp
            .headers()
            .get(reqwest::header::LOCATION)
            .and_then(|v| v.to_str().ok())
        && let Ok(url) = resp.url().join(loc)
    {
        return Some(url);
    }
    Some(resp.url().clone())
}

/// A download redirect landed on a host outside the allow-list and survived a
/// re-login, so it is not a session bounce. Names the host and the knob — the
/// misdiagnosis this replaces ("session expired and re-login failed") sent us
/// hunting a login bug when Edookit had simply moved files to a CDN.
fn download_bounce_error(
    resp: &reqwest::Response,
    base: &Url,
    extra_hosts: &[String],
) -> ClientError {
    let target = refused_target(resp);
    let reason = match &target {
        Some(url) => format!(
            "{}: {}",
            origin_str(url),
            origin_verdict(url, base, extra_hosts).advice(base)
        ),
        None => "unknown target".to_string(),
    };
    ClientError::msg(format!(
        "download bounced off-origin and re-login did not help — {reason}"
    ))
}

/// Whether `host` is covered by `pattern` — an exact hostname, or a
/// `*.suffix` wildcard matching any **sub**domain of `suffix`. The wildcard
/// deliberately does not match `suffix` itself, and the leading dot is part of
/// the comparison so `*.edookit.net` cannot match `evil-edookit.net`.
/// Hostnames are compared case-insensitively (DNS is case-insensitive).
fn host_matches_pattern(host: &str, pattern: &str) -> bool {
    let host = host.trim_end_matches('.').to_ascii_lowercase();
    let pattern = pattern.trim().to_ascii_lowercase();
    match pattern.strip_prefix("*.") {
        Some(suffix) => {
            !suffix.is_empty()
                && host.len() > suffix.len() + 1
                && host.ends_with(&format!(".{suffix}"))
        }
        None => !pattern.is_empty() && host == pattern,
    }
}

/// The subset of every `/handler/*` response we read to detect a server-side
/// session expiry that did NOT bounce off-origin: HTTP 200 with
/// `authenticated:false`. Returns the flag if present.
fn parse_auth_envelope(body: &[u8]) -> Option<bool> {
    #[derive(Deserialize)]
    struct Env {
        authenticated: Option<bool>,
    }
    serde_json::from_slice::<Env>(body)
        .ok()
        .and_then(|e| e.authenticated)
}

#[derive(Debug, PartialEq, Eq)]
enum DownloadDisposition {
    Accept,
    Reauth,
    Fail,
}

/// Runs [`classify_download_body`] only where it can mean anything: a stale
/// session is something the **tenant** serves in place of the file. A response
/// that came from an allow-listed off-origin host (the file CDN) is always the
/// file itself.
fn classify_session_body(
    from_tenant: bool,
    content_type: &str,
    body: &[u8],
    retry: bool,
) -> DownloadDisposition {
    if from_tenant {
        classify_download_body(content_type, body, retry)
    } else {
        DownloadDisposition::Accept
    }
}

/// Decides whether a download response is a real attachment or a stale-session
/// artifact. Unlike [`Client::get_to`], the inline viewer legitimately handles
/// JSON and HTML files, so those types can't be rejected outright:
///   - application/json: only an `authenticated:false` envelope is a stale
///     session; any other JSON is a real attachment.
///   - text/html: a stale session serves the login page; retry once, but if
///     it's STILL html after a successful re-login, treat it as a genuine HTML
///     attachment.
fn classify_download_body(content_type: &str, body: &[u8], retry: bool) -> DownloadDisposition {
    let lc = content_type.to_ascii_lowercase();
    if lc.starts_with("application/json") {
        if parse_auth_envelope(body) == Some(false) {
            return if retry {
                DownloadDisposition::Reauth
            } else {
                DownloadDisposition::Fail
            };
        }
        return DownloadDisposition::Accept;
    }
    if lc.starts_with("text/html") {
        return if retry {
            DownloadDisposition::Reauth
        } else {
            DownloadDisposition::Accept
        };
    }
    DownloadDisposition::Accept
}

/// Validates the configured base URL. The `url` crate rejects schemeless input
/// and normalizes default ports, so the manual port-stripping the Go version
/// needed isn't required here.
fn parse_base_url(raw: &str, allow_insecure_http: bool) -> anyhow::Result<Url> {
    if raw.is_empty() {
        bail!("BaseURL is required");
    }
    let u = Url::parse(raw).map_err(|e| {
        anyhow!("BaseURL {raw:?} must use http or https scheme (e.g. https://your-school-login.edookit.net): {e}")
    })?;
    if u.scheme() != SCHEME_HTTP && u.scheme() != SCHEME_HTTPS {
        bail!(
            "BaseURL {raw:?} must use http or https scheme (e.g. https://your-school-login.edookit.net)"
        );
    }
    if u.host().is_none() || u.host_str().is_none_or(|h| h.is_empty()) {
        bail!("BaseURL {raw:?} has no host");
    }
    if u.scheme() == SCHEME_HTTP && !allow_insecure_http && !is_loopback_url(&u) {
        bail!(
            "BaseURL {raw:?} uses insecure http:// to a non-loopback host; use https:// or set allow_insecure_http"
        );
    }
    Ok(u)
}

/// Reports whether a URL's host refers to the local machine: literal
/// `localhost`, or any loopback IP (127.0.0.0/8, ::1).
fn is_loopback_url(u: &Url) -> bool {
    match u.host() {
        Some(Host::Domain(d)) => d.eq_ignore_ascii_case("localhost"),
        Some(Host::Ipv4(a)) => a.is_loopback(),
        Some(Host::Ipv6(a)) => a.is_loopback(),
        None => false,
    }
}

fn default_timezone() -> jiff::tz::TimeZone {
    jiff::tz::TimeZone::get("Europe/Prague").unwrap_or(jiff::tz::TimeZone::UTC)
}

/// Swappable cookie jar. The inner store is held behind an [`ArcSwap`] so the
/// whole jar can be replaced atomically by [`Jar::reset`] (from
/// `invalidate_session`): concurrent reqwest cookie reads/writes see either the
/// old generation or the new one, never a torn state. A full swap (rather than
/// expiring individual cookies) is what lets invalidation clear path-scoped
/// cookies the per-name accessor can't enumerate.
struct Jar {
    inner: ArcSwap<Mutex<cookie_store::CookieStoreImpl>>,
}

impl Jar {
    fn new() -> Self {
        Jar {
            inner: ArcSwap::from_pointee(Mutex::new(cookie_store::new_store())),
        }
    }

    fn reset(&self) {
        self.inner
            .store(Arc::new(Mutex::new(cookie_store::new_store())));
    }

    fn has_cookies_for(&self, url: &Url) -> bool {
        let guard = self.inner.load();
        let store = guard.lock();
        store.get_request_values(url).next().is_some()
    }

    fn export_name_values(&self, url: &Url) -> Vec<StoredCookie> {
        let guard = self.inner.load();
        let store = guard.lock();
        store
            .get_request_values(url)
            .map(|(n, v)| StoredCookie {
                name: n.to_string(),
                value: v.to_string(),
            })
            .collect()
    }

    fn set_name_values(&self, base: &Url, cookies: &[StoredCookie]) {
        let guard = self.inner.load();
        let mut store = guard.lock();
        for c in cookies {
            insert_name_value(&mut store, base, &c.name, &c.value);
        }
    }

    fn set_login_cookies(&self, base: &Url, cookies: &[LoginCookie]) {
        let guard = self.inner.load();
        let mut store = guard.lock();
        for c in cookies {
            insert_name_value(&mut store, base, &c.name, &c.value);
        }
    }
}

fn insert_name_value(
    store: &mut cookie_store::CookieStoreImpl,
    base: &Url,
    name: &str,
    value: &str,
) {
    let raw = cookie_store::RawCookie::new(name.to_string(), value.to_string());
    let _ = store.insert_raw(&raw, base);
}

/// The cookie view handed to the **download** client: the shared [`Jar`] for
/// the tenant origin, and *nothing at all* for the allow-listed off-origin
/// hosts — neither sent nor stored.
///
/// The download client has to keep the session for the tenant hop (the
/// `/handler/download/*` request that issues the redirect is authenticated),
/// but the CDN hop must be cookie-free. Relying on cookie attributes for that
/// would be a bet on upstream: a tenant cookie with `Domain=.edookit.net`
/// would domain-match `dataN.edookit.net` and ride along, and a `Set-Cookie`
/// from the CDN could likewise be stored against the shared registrable domain
/// and come back to influence the tenant session. Today's cookies happen to be
/// host-only; this makes it structural instead of incidental.
struct TenantOnlyCookies {
    jar: Arc<Jar>,
    base_url: Url,
}

impl reqwest::cookie::CookieStore for TenantOnlyCookies {
    fn set_cookies(&self, cookie_headers: &mut dyn Iterator<Item = &HeaderValue>, url: &Url) {
        if same_origin(url, &self.base_url) {
            self.jar.set_cookies(cookie_headers, url);
        }
    }

    fn cookies(&self, url: &Url) -> Option<HeaderValue> {
        if same_origin(url, &self.base_url) {
            self.jar.cookies(url)
        } else {
            None
        }
    }
}

impl reqwest::cookie::CookieStore for Jar {
    fn set_cookies(&self, cookie_headers: &mut dyn Iterator<Item = &HeaderValue>, url: &Url) {
        let guard = self.inner.load();
        let mut store = guard.lock();
        let iter = cookie_headers
            .filter_map(|h| h.to_str().ok())
            .filter_map(|s| cookie_store::RawCookie::parse(s.to_owned()).ok());
        store.store_response_cookies(iter, url);
    }

    fn cookies(&self, url: &Url) -> Option<HeaderValue> {
        let guard = self.inner.load();
        let store = guard.lock();
        let joined: Vec<String> = store
            .get_request_values(url)
            .map(|(n, v)| format!("{n}={v}"))
            .collect();
        if joined.is_empty() {
            return None;
        }
        HeaderValue::from_str(&joined.join("; ")).ok()
    }
}
