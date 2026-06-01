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
#[derive(Debug, Clone)]
pub struct LoginCookie {
    pub name: String,
    pub value: String,
    pub domain: String,
    pub path: String,
    pub secure: bool,
    pub http_only: bool,
}

impl LoginCookie {
    pub fn new(name: impl Into<String>, value: impl Into<String>) -> Self {
        LoginCookie {
            name: name.into(),
            value: value.into(),
            domain: String::new(),
            path: String::new(),
            secure: false,
            http_only: false,
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
    http: reqwest::Client,
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
        let http = reqwest::Client::builder()
            .cookie_provider(jar.clone())
            .timeout(Duration::from_secs(20))
            .build()
            .context("build http client")?;

        let client = Client {
            cfg,
            http,
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

            if !same_origin(resp.url(), &self.base_url) {
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

    /// Fetches `path` as a parsed HTML document. Reserved for the rare
    /// server-rendered page; use [`get_json`](Self::get_json) for the SPA's
    /// XHR endpoints.
    pub async fn get_doc(&self, path: &str) -> Result<scraper::Html, ClientError> {
        let mut allow_retry = true;
        loop {
            self.ensure_logged_in().await?;
            self.preflight_same_origin(path)?;

            let req = self.new_request(path, false)?;
            let resp = self.send_retrying(req).await?;

            if !same_origin(resp.url(), &self.base_url) {
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
            let text = resp
                .text()
                .await
                .map_err(|e| ClientError::msg(format!("read body from {path}: {e}")))?;
            return Ok(scraper::Html::parse_document(&text));
        }
    }

    /// Streams the body of `GET <path>` into `dst`, returning the byte count.
    /// Used for binary downloads where we don't want to buffer the whole file.
    /// `path` may be relative or an absolute (same-origin) URL.
    pub async fn get_to<W: std::io::Write>(
        &self,
        path: &str,
        dst: &mut W,
    ) -> Result<u64, ClientError> {
        let mut allow_retry = true;
        loop {
            self.ensure_logged_in().await?;
            self.preflight_same_origin(path)
                .map_err(|e| ClientError::msg(format!("GET {path}: {e}")))?;

            let req = self.new_request(path, false)?;
            let resp = self.send_retrying(req).await?;

            if !same_origin(resp.url(), &self.base_url) {
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

            // Non-file response on a download endpoint: text/html is almost
            // certainly the login page (stale cookies, no off-origin redirect)
            // → invalidate + retry. application/json is a deterministic API
            // error envelope → propagate (a retry would just hit it again).
            let ct = content_type_lower(&resp);
            if ct.starts_with("text/html") {
                if !allow_retry {
                    return Err(ClientError::msg(format!(
                        "GET {path}: server returned text/html (likely login page) — re-login failed"
                    )));
                }
                self.invalidate_session().await;
                allow_retry = false;
                continue;
            }
            if ct.starts_with("application/json") {
                return Err(ClientError::msg(format!(
                    "GET {path}: server returned application/json on a binary download endpoint (likely an API error envelope, not a file)"
                )));
            }

            let mut stream = resp.bytes_stream();
            let mut written: u64 = 0;
            while let Some(chunk) = stream.next().await {
                let chunk = chunk.map_err(|e| ClientError::msg(format!("GET {path}: {e}")))?;
                dst.write_all(&chunk)
                    .map_err(|e| ClientError::msg(format!("write to dst: {e}")))?;
                written += chunk.len() as u64;
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
            self.preflight_same_origin(path)
                .map_err(|e| ClientError::msg(format!("GET {path}: {e}")))?;

            let req = self.new_request(path, false)?;
            let resp = self.send_retrying(req).await?;

            if !same_origin(resp.url(), &self.base_url) {
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
            let ct = resp
                .headers()
                .get(reqwest::header::CONTENT_TYPE)
                .and_then(|v| v.to_str().ok())
                .unwrap_or("")
                .to_string();

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

            match classify_download_body(&ct, &buf, allow_retry) {
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
        let _ = resp.bytes().await; // drain

        if !same_origin(&final_url, &self.base_url) {
            return Err(ClientError::msg(format!(
                "warmup bounced off-origin to {} (session expired)",
                final_url.host_str().unwrap_or("")
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
            match self.http.execute(attempt).await {
                Err(e) => {
                    // A client-timeout is the caller's deadline — don't burn
                    // the remaining attempts on it. Other net errors retry.
                    if e.is_timeout() {
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
