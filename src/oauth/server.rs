//! In-memory OAuth 2.1 Authorization Server. Port of Go's
//! `internal/oauth/server.go`. Open DCR (RFC 7591), PKCE-S256-only,
//! authorization_code + refresh_token with rotation + replay detection. All
//! state lives behind one `parking_lot::Mutex` (no `.await` held while locked);
//! time comes from an injectable clock.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;

use std::convert::Infallible;

use askama::Template;
use axum::Json;
use axum::Router;
use axum::body::Bytes;
use axum::extract::{ConnectInfo, DefaultBodyLimit, Form, FromRequestParts, Query, State};
use axum::http::request::Parts;
use axum::http::{HeaderMap, StatusCode, header};
use axum::response::{Html, IntoResponse, Response};
use axum::routing::{get, post};
use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD as B64URL;
use parking_lot::Mutex;
use serde::{Deserialize, Serialize};
use serde_json::json;
use subtle::ConstantTimeEq;

use super::jwt::{self, Claims};
use super::login::LoginPage;
use super::ratelimit::{SubLimiter, Throttle, client_ip};
use super::{Clock, system_clock};

// OAuth wire-format constants.
const RESPONSE_TYPE_CODE: &str = "code";
const GRANT_AUTHORIZATION_CODE: &str = "authorization_code";
const GRANT_REFRESH_TOKEN: &str = "refresh_token";
const PKCE_S256: &str = "S256";
const SCOPE_OFFLINE_ACCESS: &str = "offline_access";

// Limits bounding the open-to-the-internet endpoints (tuned for single-user).
const MAX_REGISTER_BODY: usize = 16 * 1024;
const MAX_REDIRECT_URIS: usize = 8;
const MAX_REDIRECT_URI_LEN: usize = 2048;
const MAX_CLIENT_NAME_LEN: usize = 256;
const MAX_CLIENTS: usize = 256;
const MAX_OAUTH_FORM_BODY: usize = 32 * 1024;

const LOGIN_FAILURE_WINDOW: i64 = 60;
const LOGIN_FAILURE_MAX: i32 = 8;
const LOGIN_BLOCK: i64 = 15 * 60;
const LOGIN_FAILURE_DELAY_MS: u64 = 500;

const DCR_WINDOW: i64 = 60 * 60;
const DCR_MAX: i32 = 5;
const DCR_BLOCK: i64 = 60 * 60;

const MCP_RATE_PER_SEC: f64 = 5.0;
const MCP_BURST: u32 = 30;
const SUB_BUCKET_TTL: i64 = 30 * 60;

const MAX_USED_REFRESH: usize = 4096;
const MAX_ROTATIONS_PER_FAMILY: i32 = 64;
const MAX_REFRESH_TOKENS: usize = 1024;

/// Deployment-specific parameters, wired from env at startup.
#[derive(Clone)]
pub struct Config {
    pub public_url: String,
    pub audience: String,
    pub login_username: String, // may be empty (single-user "any username" mode)
    pub login_password: String,
    pub jwt_secret: Vec<u8>,
    pub access_ttl: i64,
    pub refresh_ttl: i64,
    pub code_ttl: i64,
}

impl Config {
    pub fn new(
        public_url: String,
        audience: String,
        login_password: String,
        jwt_secret: Vec<u8>,
    ) -> Self {
        Self {
            public_url,
            audience,
            login_username: String::new(),
            login_password,
            jwt_secret,
            access_ttl: 24 * 3600,
            refresh_ttl: 30 * 24 * 3600,
            code_ttl: 60,
        }
    }
}

#[derive(Clone, Serialize)]
struct ClientReg {
    client_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    client_name: String,
    redirect_uris: Vec<String>,
    #[serde(rename = "client_id_issued_at")]
    issued_at: i64,
    token_endpoint_auth_method: &'static str,
    grant_types: Vec<&'static str>,
    response_types: Vec<&'static str>,
}

struct AuthCode {
    client_id: String,
    redirect_uri: String,
    challenge: String,
    method: String,
    scope: String,
    sub: String,
    expires_at: i64,
}

struct RefreshRecord {
    client_id: String,
    sub: String,
    scope: String,
    expires_at: i64,
    generation: i32,
}

struct RefreshTombstone {
    client_id: String,
    expires_at: i64,
}

#[derive(Default)]
struct ServerState {
    clients: HashMap<String, ClientReg>,
    codes: HashMap<String, AuthCode>,
    refresh: HashMap<String, RefreshRecord>,
    used_refresh: HashMap<String, RefreshTombstone>,
}

pub struct Server {
    cfg: Config,
    clock: Clock,
    state: Mutex<ServerState>,
    throttle: Throttle,
    dcr_throttle: Throttle,
    mcp_rate: SubLimiter,
}

impl Server {
    pub fn new(cfg: Config) -> Result<Self, String> {
        Self::with_clock(cfg, system_clock())
    }

    pub fn with_clock(cfg: Config, clock: Clock) -> Result<Self, String> {
        if cfg.public_url.is_empty() {
            return Err("PublicURL is required".into());
        }
        if cfg.audience.is_empty() {
            return Err("Audience is required".into());
        }
        if cfg.login_password.is_empty() {
            return Err("LoginPassword is required".into());
        }
        if cfg.jwt_secret.len() < 32 {
            return Err(format!(
                "JWTSecret must be at least 32 bytes (got {})",
                cfg.jwt_secret.len()
            ));
        }
        Ok(Self {
            throttle: Throttle::new(
                LOGIN_FAILURE_WINDOW,
                LOGIN_FAILURE_MAX,
                LOGIN_BLOCK,
                LOGIN_FAILURE_DELAY_MS,
                clock.clone(),
            ),
            dcr_throttle: Throttle::new(DCR_WINDOW, DCR_MAX, DCR_BLOCK, 0, clock.clone()),
            mcp_rate: SubLimiter::new(MCP_RATE_PER_SEC, MCP_BURST, SUB_BUCKET_TTL, clock.clone()),
            cfg,
            clock,
            state: Mutex::new(ServerState::default()),
        })
    }

    fn now(&self) -> i64 {
        (self.clock)()
    }

    /// Per-subject `/mcp` rate gate, called by the bearer middleware.
    pub fn mcp_allow(&self, sub: &str) -> bool {
        self.mcp_rate.allow(sub)
    }

    pub fn verify_jwt(&self, token: &str) -> Result<String, jwt::JwtError> {
        jwt::verify(
            &self.cfg.jwt_secret,
            token,
            &self.cfg.public_url,
            &self.cfg.audience,
            self.now(),
        )
        .map(|c| c.sub)
    }

    pub fn public_url(&self) -> &str {
        &self.cfg.public_url
    }

    /// RFC 9728 path-qualified well-known URL path for our resource.
    pub fn protected_resource_metadata_path(&self) -> String {
        const BASE: &str = "/.well-known/oauth-protected-resource";
        match url::Url::parse(&self.cfg.audience) {
            Ok(u) if u.path() != "" && u.path() != "/" => format!("{BASE}{}", u.path()),
            _ => BASE.to_string(),
        }
    }

    pub fn protected_resource_metadata_url(&self) -> String {
        format!(
            "{}{}",
            self.cfg.public_url,
            self.protected_resource_metadata_path()
        )
    }

    /// Periodic purge of expired codes/refresh/tombstones + throttle GC.
    pub fn gc(&self) {
        let now = self.now();
        {
            let mut st = self.state.lock();
            st.codes.retain(|_, c| now <= c.expires_at);
            st.refresh.retain(|_, r| now <= r.expires_at);
            st.used_refresh.retain(|_, t| now <= t.expires_at);
        }
        self.throttle.gc();
        self.dcr_throttle.gc();
        self.mcp_rate.gc();
    }

    /// Spawns the background GC loop (every 5 min). Call after wrapping in Arc.
    pub fn start_gc(self: &Arc<Self>) {
        let srv = self.clone();
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(std::time::Duration::from_secs(5 * 60));
            loop {
                tick.tick().await;
                srv.gc();
            }
        });
    }

    /// Builds the OAuth + discovery routes as a fully-stated `Router` the
    /// caller merges into the app. `/mcp` is NOT mounted here — the caller
    /// mounts it wrapped in the bearer middleware (Phase 6).
    pub fn router(self: Arc<Self>) -> Router {
        let pr_path = self.protected_resource_metadata_path();
        let mut r: Router<Arc<Server>> = Router::new()
            .route("/.well-known/oauth-authorization-server", get(as_metadata))
            .route("/.well-known/openid-configuration", get(as_metadata))
            .route(
                "/.well-known/oauth-protected-resource",
                get(resource_metadata),
            )
            .route(
                "/oauth/register",
                post(register).layer(DefaultBodyLimit::max(MAX_REGISTER_BODY)),
            )
            .route(
                "/oauth/authorize",
                get(authorize_show)
                    .post(authorize_submit)
                    .layer(DefaultBodyLimit::max(MAX_OAUTH_FORM_BODY)),
            )
            .route(
                "/oauth/token",
                post(token).layer(DefaultBodyLimit::max(MAX_OAUTH_FORM_BODY)),
            );
        if pr_path != "/.well-known/oauth-protected-resource" {
            r = r.route(&pr_path, get(resource_metadata));
        }
        r.with_state(self)
    }
}

/// Infallible peer-address extractor: reads the `ConnectInfo<SocketAddr>` the
/// listener injects (production), or `None` (e.g. in `oneshot` tests). Avoids
/// `Option<ConnectInfo>`, which axum 0.8 doesn't accept as an extractor.
struct PeerAddr(Option<SocketAddr>);

impl<S: Send + Sync> FromRequestParts<S> for PeerAddr {
    type Rejection = Infallible;
    async fn from_request_parts(parts: &mut Parts, _state: &S) -> Result<Self, Self::Rejection> {
        Ok(PeerAddr(
            parts
                .extensions
                .get::<ConnectInfo<SocketAddr>>()
                .map(|c| c.0),
        ))
    }
}

// --- discovery ---

async fn as_metadata(State(srv): State<Arc<Server>>) -> Response {
    let base = &srv.cfg.public_url;
    write_json(
        StatusCode::OK,
        json!({
            "issuer": base,
            "authorization_endpoint": format!("{base}/oauth/authorize"),
            "token_endpoint": format!("{base}/oauth/token"),
            "registration_endpoint": format!("{base}/oauth/register"),
            "response_types_supported": [RESPONSE_TYPE_CODE],
            "grant_types_supported": [GRANT_AUTHORIZATION_CODE, GRANT_REFRESH_TOKEN],
            "code_challenge_methods_supported": [PKCE_S256],
            "token_endpoint_auth_methods_supported": ["none"],
            "scopes_supported": ["openid", SCOPE_OFFLINE_ACCESS],
        }),
    )
}

async fn resource_metadata(State(srv): State<Arc<Server>>) -> Response {
    write_json(
        StatusCode::OK,
        json!({
            "resource": srv.cfg.audience,
            "authorization_servers": [srv.cfg.public_url],
            "bearer_methods_supported": ["header"],
        }),
    )
}

// --- DCR ---

#[derive(Deserialize, Default)]
struct RegisterRequest {
    #[serde(default)]
    client_name: String,
    #[serde(default)]
    redirect_uris: Vec<String>,
}

async fn register(
    State(srv): State<Arc<Server>>,
    peer: PeerAddr,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    let ip = peer_ip(&peer, &headers);
    if let (false, retry) = srv.dcr_throttle.try_acquire(&ip) {
        tracing::warn!("oauth: DCR blocked for {ip:?} (retry in {retry}s)");
        return (
            [(header::RETRY_AFTER, retry.to_string())],
            write_json_error(
                StatusCode::TOO_MANY_REQUESTS,
                "too_many_requests",
                "registration rate-limited; try again later",
            ),
        )
            .into_response();
    }
    let req: RegisterRequest = match serde_json::from_slice(&body) {
        Ok(r) => r,
        Err(_) => {
            return write_json_error(
                StatusCode::BAD_REQUEST,
                "invalid_client_metadata",
                "invalid JSON body",
            );
        }
    };
    if req.redirect_uris.is_empty() {
        return write_json_error(
            StatusCode::BAD_REQUEST,
            "invalid_redirect_uri",
            "redirect_uris is required and must be non-empty",
        );
    }
    if req.redirect_uris.len() > MAX_REDIRECT_URIS {
        return write_json_error(
            StatusCode::BAD_REQUEST,
            "invalid_redirect_uri",
            &format!("too many redirect_uris (max {MAX_REDIRECT_URIS})"),
        );
    }
    if req.client_name.len() > MAX_CLIENT_NAME_LEN {
        return write_json_error(
            StatusCode::BAD_REQUEST,
            "invalid_client_metadata",
            "client_name too long",
        );
    }
    for u in &req.redirect_uris {
        if let Err(e) = validate_redirect_uri(u) {
            return write_json_error(StatusCode::BAD_REQUEST, "invalid_redirect_uri", &e);
        }
    }
    let id = match random_token(24) {
        Ok(id) => id,
        Err(_) => {
            return write_json_error(
                StatusCode::INTERNAL_SERVER_ERROR,
                "server_error",
                "could not generate client_id",
            );
        }
    };
    let reg = ClientReg {
        client_id: format!("mcp_{id}"),
        client_name: req.client_name,
        redirect_uris: req.redirect_uris,
        issued_at: srv.now(),
        token_endpoint_auth_method: "none",
        grant_types: vec![GRANT_AUTHORIZATION_CODE, GRANT_REFRESH_TOKEN],
        response_types: vec![RESPONSE_TYPE_CODE],
    };
    {
        let mut st = srv.state.lock();
        if st.clients.len() >= MAX_CLIENTS {
            evict_oldest_client(&mut st);
        }
        st.clients.insert(reg.client_id.clone(), reg.clone());
    }
    tracing::info!(
        "oauth: registered client {} (name={:?})",
        reg.client_id,
        reg.client_name
    );
    write_json(
        StatusCode::CREATED,
        serde_json::to_value(&reg).unwrap_or_default(),
    )
}

// --- authorize ---

#[derive(Deserialize, Default)]
struct AuthorizeRaw {
    #[serde(default)]
    response_type: String,
    #[serde(default)]
    client_id: String,
    #[serde(default)]
    redirect_uri: String,
    #[serde(default)]
    scope: String,
    #[serde(default)]
    state: String,
    #[serde(default)]
    code_challenge: String,
    #[serde(default)]
    code_challenge_method: String,
    // submit-only:
    #[serde(default)]
    username: String,
    #[serde(default)]
    password: String,
}

struct AuthorizeParams {
    response_type: String,
    client_id: String,
    redirect_uri: String,
    scope: String,
    state: String,
    code_challenge: String,
    code_challenge_method: String,
}

fn parse_authorize(raw: &AuthorizeRaw) -> Result<AuthorizeParams, String> {
    if raw.response_type != RESPONSE_TYPE_CODE {
        return Err(format!("response_type must be {RESPONSE_TYPE_CODE:?}"));
    }
    if raw.client_id.is_empty() {
        return Err("client_id is required".into());
    }
    if raw.redirect_uri.is_empty() {
        return Err("redirect_uri is required".into());
    }
    if raw.code_challenge.is_empty() {
        return Err("code_challenge is required (PKCE)".into());
    }
    let method = if raw.code_challenge_method.is_empty() {
        "plain"
    } else {
        raw.code_challenge_method.as_str()
    };
    if method != PKCE_S256 {
        return Err(format!(
            "code_challenge_method must be {PKCE_S256} (got {method:?})"
        ));
    }
    validate_pkce_challenge(&raw.code_challenge)?;
    Ok(AuthorizeParams {
        response_type: raw.response_type.clone(),
        client_id: raw.client_id.clone(),
        redirect_uri: raw.redirect_uri.clone(),
        scope: raw.scope.clone(),
        state: raw.state.clone(),
        code_challenge: raw.code_challenge.clone(),
        code_challenge_method: method.to_string(),
    })
}

async fn authorize_show(
    State(srv): State<Arc<Server>>,
    Query(raw): Query<AuthorizeRaw>,
) -> Response {
    let p = match parse_authorize(&raw) {
        Ok(p) => p,
        Err(e) => {
            return (StatusCode::BAD_REQUEST, format!("invalid_request: {e}")).into_response();
        }
    };
    let client = match srv.lookup_client_for_redirect(&p.client_id, &p.redirect_uri) {
        Ok(c) => c,
        // Bad client / redirect — render inline, NEVER 302 to an attacker URL.
        Err(e) => {
            return (StatusCode::BAD_REQUEST, format!("invalid_request: {e}")).into_response();
        }
    };
    render_login(&srv, &p, &client, "", false)
}

async fn authorize_submit(
    State(srv): State<Arc<Server>>,
    peer: PeerAddr,
    headers: HeaderMap,
    Form(raw): Form<AuthorizeRaw>,
) -> Response {
    let p = match parse_authorize(&raw) {
        Ok(p) => p,
        Err(e) => {
            return (StatusCode::BAD_REQUEST, format!("invalid_request: {e}")).into_response();
        }
    };
    let client = match srv.lookup_client_for_redirect(&p.client_id, &p.redirect_uri) {
        Ok(c) => c,
        Err(e) => {
            return (StatusCode::BAD_REQUEST, format!("invalid_request: {e}")).into_response();
        }
    };

    let ip = peer_ip(&peer, &headers);
    if let (false, retry) = srv.throttle.try_acquire(&ip) {
        tracing::warn!("oauth: login blocked for {ip:?} (retry in {retry}s)");
        return (
            StatusCode::TOO_MANY_REQUESTS,
            [(header::RETRY_AFTER, retry.to_string())],
            "too many failed login attempts — try again later",
        )
            .into_response();
    }

    if !srv.check_login(&raw.username, &raw.password) {
        tokio::time::sleep(std::time::Duration::from_millis(
            srv.throttle.failure_delay_ms,
        ))
        .await;
        tracing::warn!(
            "oauth: login failure for {ip:?} (client={})",
            client.client_id
        );
        return render_login(&srv, &p, &client, "Invalid credentials", true);
    }

    srv.throttle.record_success(&ip);
    let code = match random_token(32) {
        Ok(c) => c,
        Err(_) => {
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                "server_error: could not mint code",
            )
                .into_response();
        }
    };
    let now = srv.now();
    {
        let mut st = srv.state.lock();
        st.codes.insert(
            code.clone(),
            AuthCode {
                client_id: client.client_id.clone(),
                redirect_uri: p.redirect_uri.clone(),
                challenge: p.code_challenge.clone(),
                method: p.code_challenge_method.clone(),
                scope: p.scope.clone(),
                sub: srv.subject(),
                expires_at: now + srv.cfg.code_ttl,
            },
        );
    }
    // 302 to redirect_uri?code=&state= with no-store.
    let mut u = url::Url::parse(&p.redirect_uri).expect("redirect_uri validated at registration");
    u.query_pairs_mut().append_pair("code", &code);
    if !p.state.is_empty() {
        u.query_pairs_mut().append_pair("state", &p.state);
    }
    Response::builder()
        .status(StatusCode::FOUND)
        .header(header::LOCATION, u.to_string())
        .header(header::CACHE_CONTROL, "no-store")
        .header("Pragma", "no-cache")
        .body(axum::body::Body::empty())
        .unwrap()
}

// --- token ---

#[derive(Deserialize, Default)]
struct TokenRaw {
    #[serde(default)]
    grant_type: String,
    #[serde(default)]
    code: String,
    #[serde(default)]
    redirect_uri: String,
    #[serde(default)]
    client_id: String,
    #[serde(default)]
    code_verifier: String,
    #[serde(default)]
    refresh_token: String,
}

async fn token(State(srv): State<Arc<Server>>, Form(raw): Form<TokenRaw>) -> Response {
    match raw.grant_type.as_str() {
        GRANT_AUTHORIZATION_CODE => srv.token_auth_code(&raw),
        GRANT_REFRESH_TOKEN => srv.token_refresh(&raw),
        other => write_json_error(
            StatusCode::BAD_REQUEST,
            "unsupported_grant_type",
            &format!(
                "grant_type {other:?} is not supported (use authorization_code or refresh_token)"
            ),
        ),
    }
}

impl Server {
    fn lookup_client_for_redirect(
        &self,
        client_id: &str,
        redirect_uri: &str,
    ) -> Result<ClientReg, String> {
        let st = self.state.lock();
        let c = st.clients.get(client_id).ok_or("unknown client_id")?;
        if c.redirect_uris.iter().any(|u| u == redirect_uri) {
            Ok(c.clone())
        } else {
            Err(format!(
                "redirect_uri {redirect_uri:?} is not registered for this client"
            ))
        }
    }

    fn check_login(&self, username: &str, password: &str) -> bool {
        if !self.cfg.login_username.is_empty() && !ct_eq(username, &self.cfg.login_username) {
            // Run the password check anyway so timing doesn't leak which field was wrong.
            let _ = ct_eq(password, &self.cfg.login_password);
            return false;
        }
        ct_eq(password, &self.cfg.login_password)
    }

    /// The JWT `sub` for a freshly minted code. This must NOT be derived from
    /// the login form: when no expected username is configured, `check_login`
    /// accepts any username for the configured password, so echoing the typed
    /// username back into `sub` would let the client pick its own subject.
    /// Use the configured username if present, otherwise a fixed constant.
    fn subject(&self) -> String {
        if self.cfg.login_username.is_empty() {
            "edookit-mcp-user".to_string()
        } else {
            self.cfg.login_username.clone()
        }
    }

    fn issue_jwt(&self, sub: &str, scope: &str) -> Result<String, String> {
        let jti = random_token(16)?;
        let now = self.now();
        let claims = Claims {
            iss: self.cfg.public_url.clone(),
            sub: sub.to_string(),
            aud: self.cfg.audience.clone(),
            iat: now,
            exp: now + self.cfg.access_ttl,
            jti,
            scope: scope.to_string(),
        };
        jwt::sign(&self.cfg.jwt_secret, &claims)
    }

    fn token_auth_code(&self, raw: &TokenRaw) -> Response {
        if raw.code.is_empty()
            || raw.redirect_uri.is_empty()
            || raw.client_id.is_empty()
            || raw.code_verifier.is_empty()
        {
            return write_json_error(
                StatusCode::BAD_REQUEST,
                "invalid_request",
                "code, redirect_uri, client_id, code_verifier are all required",
            );
        }
        if let Err(e) = validate_pkce_verifier(&raw.code_verifier) {
            return write_json_error(StatusCode::BAD_REQUEST, "invalid_grant", &e);
        }
        // Atomically consume the code (single-use).
        let ac = {
            let mut st = self.state.lock();
            st.codes.remove(&raw.code)
        };
        let Some(ac) = ac else {
            return write_json_error(
                StatusCode::BAD_REQUEST,
                "invalid_grant",
                "unknown or already-used code",
            );
        };
        if self.now() > ac.expires_at {
            return write_json_error(StatusCode::BAD_REQUEST, "invalid_grant", "code expired");
        }
        if ac.client_id != raw.client_id {
            return write_json_error(
                StatusCode::BAD_REQUEST,
                "invalid_grant",
                "client_id does not match the code",
            );
        }
        if ac.redirect_uri != raw.redirect_uri {
            return write_json_error(
                StatusCode::BAD_REQUEST,
                "invalid_grant",
                "redirect_uri does not match the code",
            );
        }
        if let Err(e) = jwt::pkce_verify(&raw.code_verifier, &ac.challenge, &ac.method) {
            return write_json_error(StatusCode::BAD_REQUEST, "invalid_grant", &e);
        }
        let access = match self.issue_jwt(&ac.sub, &ac.scope) {
            Ok(a) => a,
            Err(_) => {
                return write_json_error(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    "server_error",
                    "could not mint access token",
                );
            }
        };
        let mut resp = json!({
            "access_token": access,
            "token_type": "Bearer",
            "expires_in": self.cfg.access_ttl,
            "scope": ac.scope,
        });
        if scope_has(&ac.scope, SCOPE_OFFLINE_ACCESS) {
            let rt = match random_token(48) {
                Ok(rt) => rt,
                Err(_) => {
                    return write_json_error(
                        StatusCode::INTERNAL_SERVER_ERROR,
                        "server_error",
                        "could not mint refresh token",
                    );
                }
            };
            let mut st = self.state.lock();
            st.refresh.insert(
                rt.clone(),
                RefreshRecord {
                    client_id: ac.client_id.clone(),
                    sub: ac.sub.clone(),
                    scope: ac.scope.clone(),
                    expires_at: self.now() + self.cfg.refresh_ttl,
                    generation: 0,
                },
            );
            trim_active_refresh(&mut st, &rt);
            resp["refresh_token"] = json!(rt);
        }
        write_json(StatusCode::OK, resp)
    }

    fn token_refresh(&self, raw: &TokenRaw) -> Response {
        if raw.refresh_token.is_empty() || raw.client_id.is_empty() {
            return write_json_error(
                StatusCode::BAD_REQUEST,
                "invalid_request",
                "refresh_token and client_id are required",
            );
        }
        // Whole exchange under one lock; fallible work (random + JWT) runs
        // BEFORE committing so a failure leaves no orphan state.
        let mut st = self.state.lock();

        if let Some(tomb) = st.used_refresh.get(&raw.refresh_token) {
            // Replay — purge every active RT for the tombstone's ORIGINAL
            // client (never trust the request's client_id here).
            let victim = tomb.client_id.clone();
            invalidate_client_refresh(&mut st, &victim);
            tracing::warn!(
                "oauth: refresh-token REPLAY for client={victim:?} — invalidated all RTs for that client"
            );
            return write_json_error(
                StatusCode::BAD_REQUEST,
                "invalid_grant",
                "refresh_token chain has been invalidated (suspected replay)",
            );
        }
        let Some(rec) = st.refresh.get(&raw.refresh_token) else {
            return write_json_error(
                StatusCode::BAD_REQUEST,
                "invalid_grant",
                "unknown refresh_token",
            );
        };
        if rec.client_id != raw.client_id {
            return write_json_error(
                StatusCode::BAD_REQUEST,
                "invalid_grant",
                "client_id does not match the refresh_token",
            );
        }
        if self.now() > rec.expires_at {
            st.refresh.remove(&raw.refresh_token);
            return write_json_error(
                StatusCode::BAD_REQUEST,
                "invalid_grant",
                "refresh_token expired",
            );
        }
        if rec.generation + 1 > MAX_ROTATIONS_PER_FAMILY {
            let cid = rec.client_id.clone();
            invalidate_client_refresh(&mut st, &cid);
            return write_json_error(
                StatusCode::BAD_REQUEST,
                "invalid_grant",
                "refresh_token rotation cap reached for this chain; re-authenticate",
            );
        }

        // Snapshot what we need, then run fallible work before mutating.
        let (rec_client, rec_sub, rec_scope, rec_expires, rec_gen) = (
            rec.client_id.clone(),
            rec.sub.clone(),
            rec.scope.clone(),
            rec.expires_at,
            rec.generation,
        );

        let new_rt = match random_token(48) {
            Ok(rt) => rt,
            Err(_) => {
                return write_json_error(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    "server_error",
                    "could not mint refresh token",
                );
            }
        };
        let access = match self.issue_jwt(&rec_sub, &rec_scope) {
            Ok(a) => a,
            Err(_) => {
                return write_json_error(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    "server_error",
                    "could not mint access token",
                );
            }
        };

        // Eviction-survival guard: if usedRefresh is at cap and the tombstone
        // we're about to add would be the oldest (evicted immediately), the
        // replay signal would be lost — refuse + tear down the family.
        if st.used_refresh.len() >= MAX_USED_REFRESH {
            let oldest = st
                .used_refresh
                .values()
                .map(|t| t.expires_at)
                .min()
                .unwrap_or(i64::MAX);
            if rec_expires <= oldest {
                invalidate_client_refresh(&mut st, &rec_client);
                tracing::warn!(
                    "oauth: rotation refused, replay-state full; tore down client={rec_client:?}"
                );
                return write_json_error(
                    StatusCode::BAD_REQUEST,
                    "invalid_grant",
                    "rotation refused: replay-detection state is full; re-authenticate",
                );
            }
        }

        // Commit: retire old, tombstone (with ORIGINAL client_id), write new.
        st.refresh.remove(&raw.refresh_token);
        st.used_refresh.insert(
            raw.refresh_token.clone(),
            RefreshTombstone {
                client_id: rec_client.clone(),
                expires_at: rec_expires,
            },
        );
        trim_used_refresh(&mut st);
        st.refresh.insert(
            new_rt.clone(),
            RefreshRecord {
                client_id: rec_client,
                sub: rec_sub,
                scope: rec_scope.clone(),
                expires_at: self.now() + self.cfg.refresh_ttl,
                generation: rec_gen + 1,
            },
        );
        trim_active_refresh(&mut st, &new_rt);

        write_json(
            StatusCode::OK,
            json!({
                "access_token": access,
                "token_type": "Bearer",
                "expires_in": self.cfg.access_ttl,
                "refresh_token": new_rt,
                "scope": rec_scope,
            }),
        )
    }
}

fn render_login(
    srv: &Server,
    p: &AuthorizeParams,
    client: &ClientReg,
    err: &str,
    is_error: bool,
) -> Response {
    let page = LoginPage {
        title: "edookit-mcp".to_string(),
        login_hint: srv.cfg.login_username.clone(),
        client_id: p.client_id.clone(),
        client_name: client.client_name.clone(),
        redirect_uri: p.redirect_uri.clone(),
        scope: p.scope.clone(),
        state: p.state.clone(),
        code_challenge: p.code_challenge.clone(),
        code_challenge_method: p.code_challenge_method.clone(),
        response_type: p.response_type.clone(),
        error: err.to_string(),
    };
    let body = match page.render() {
        Ok(b) => b,
        Err(e) => {
            tracing::error!("oauth: render login: {e}");
            return (StatusCode::INTERNAL_SERVER_ERROR, "render error").into_response();
        }
    };
    let status = if is_error {
        StatusCode::UNAUTHORIZED
    } else {
        StatusCode::OK
    };
    // Frame-busting + cache hygiene. form-action intentionally omitted (the
    // form POSTs here then 302s cross-origin to the client's redirect_uri).
    (
        status,
        [
            (header::CONTENT_TYPE, "text/html; charset=utf-8"),
            (header::CACHE_CONTROL, "no-store"),
            (header::X_FRAME_OPTIONS, "DENY"),
            (
                header::CONTENT_SECURITY_POLICY,
                "default-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'",
            ),
            (header::REFERRER_POLICY, "no-referrer"),
            (header::X_CONTENT_TYPE_OPTIONS, "nosniff"),
        ],
        Html(body),
    )
        .into_response()
}

// --- state-mutating helpers (called with the state lock held) ---

fn evict_oldest_client(st: &mut ServerState) {
    if let Some((k, _)) = st
        .clients
        .iter()
        .min_by_key(|(_, c)| c.issued_at)
        .map(|(k, c)| (k.clone(), c.issued_at))
    {
        tracing::info!("oauth: clients at cap ({MAX_CLIENTS}) — evicting oldest {k}");
        st.clients.remove(&k);
    }
}

fn trim_used_refresh(st: &mut ServerState) {
    while st.used_refresh.len() > MAX_USED_REFRESH {
        if let Some(k) = st
            .used_refresh
            .iter()
            .min_by_key(|(_, t)| t.expires_at)
            .map(|(k, _)| k.clone())
        {
            st.used_refresh.remove(&k);
        } else {
            break;
        }
    }
}

fn trim_active_refresh(st: &mut ServerState, protect_key: &str) {
    while st.refresh.len() > MAX_REFRESH_TOKENS {
        let oldest = st
            .refresh
            .iter()
            .filter(|(k, _)| k.as_str() != protect_key)
            .min_by_key(|(_, r)| r.expires_at)
            .map(|(k, r)| (k.clone(), r.client_id.clone(), r.expires_at));
        let Some((k, client_id, expires_at)) = oldest else {
            return; // nothing evictable (all protected)
        };
        // Tombstone the evicted RT so a holder gets the replay path, not "unknown".
        st.used_refresh.insert(
            k.clone(),
            RefreshTombstone {
                client_id,
                expires_at,
            },
        );
        st.refresh.remove(&k);
    }
    trim_used_refresh(st);
}

fn invalidate_client_refresh(st: &mut ServerState, client_id: &str) {
    let victims: Vec<(String, i64)> = st
        .refresh
        .iter()
        .filter(|(_, r)| r.client_id == client_id)
        .map(|(k, r)| (k.clone(), r.expires_at))
        .collect();
    for (k, expires_at) in victims {
        st.used_refresh.insert(
            k.clone(),
            RefreshTombstone {
                client_id: client_id.to_string(),
                expires_at,
            },
        );
        st.refresh.remove(&k);
    }
    trim_used_refresh(st);
}

// --- validation helpers ---

fn validate_redirect_uri(raw: &str) -> Result<(), String> {
    if raw.len() > MAX_REDIRECT_URI_LEN {
        return Err(format!(
            "redirect_uri too long (max {MAX_REDIRECT_URI_LEN})"
        ));
    }
    let u = url::Url::parse(raw).map_err(|e| format!("malformed redirect_uri {raw:?}: {e}"))?;
    if u.host_str().is_none_or(|h| h.is_empty()) {
        return Err(format!("redirect_uri {raw:?} must have a host"));
    }
    if !u.username().is_empty() || u.password().is_some() {
        return Err(format!("redirect_uri {raw:?} must not contain user info"));
    }
    if u.fragment().is_some() || raw.contains('#') {
        return Err(format!("redirect_uri {raw:?} must not have a fragment"));
    }
    match u.scheme() {
        "https" => Ok(()),
        "http" => {
            let loopback = matches!(u.host(), Some(url::Host::Ipv4(a)) if a.is_loopback())
                || matches!(u.host(), Some(url::Host::Ipv6(a)) if a.is_loopback())
                || u.host_str() == Some("localhost");
            if loopback {
                Ok(())
            } else {
                Err(format!(
                    "redirect_uri {raw:?}: http is only allowed for loopback hosts"
                ))
            }
        }
        _ => Err(format!(
            "redirect_uri {raw:?} must use https (or http on loopback)"
        )),
    }
}

/// RFC 7636 §4.2: S256 challenge is 43 unpadded base64url chars.
fn validate_pkce_challenge(c: &str) -> Result<(), String> {
    if c.len() != 43 {
        return Err(format!(
            "code_challenge must be 43 base64url chars (got {})",
            c.len()
        ));
    }
    if let Some(i) = c.bytes().position(|b| !is_base64url_byte(b)) {
        return Err(format!(
            "code_challenge contains non-base64url character at index {i}"
        ));
    }
    Ok(())
}

/// RFC 7636 §4.1: 43-128 chars from the unreserved set.
fn validate_pkce_verifier(v: &str) -> Result<(), String> {
    if v.len() < 43 || v.len() > 128 {
        return Err(format!(
            "code_verifier must be 43-128 chars (got {})",
            v.len()
        ));
    }
    if let Some(i) = v.bytes().position(|b| !is_pkce_verifier_byte(b)) {
        return Err(format!(
            "code_verifier contains disallowed character at index {i}"
        ));
    }
    Ok(())
}

fn is_base64url_byte(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b == b'-' || b == b'_'
}

fn is_pkce_verifier_byte(b: u8) -> bool {
    b.is_ascii_alphanumeric() || matches!(b, b'-' | b'.' | b'_' | b'~')
}

fn scope_has(scope: &str, want: &str) -> bool {
    scope.split_whitespace().any(|s| s == want)
}

fn ct_eq(a: &str, b: &str) -> bool {
    a.as_bytes().ct_eq(b.as_bytes()).unwrap_u8() == 1
}

fn random_token(n: usize) -> Result<String, String> {
    let mut buf = vec![0u8; n];
    getrandom::fill(&mut buf).map_err(|e| format!("getrandom: {e}"))?;
    Ok(B64URL.encode(buf))
}

fn peer_ip(peer: &PeerAddr, headers: &HeaderMap) -> String {
    let remote = peer
        .0
        .map(|a| a.ip())
        .unwrap_or_else(|| std::net::IpAddr::from([127, 0, 0, 1]));
    let xff = headers.get("x-forwarded-for").and_then(|v| v.to_str().ok());
    client_ip(remote, xff)
}

// --- response helpers ---

fn write_json(status: StatusCode, value: serde_json::Value) -> Response {
    (
        status,
        [
            (header::CONTENT_TYPE, "application/json; charset=utf-8"),
            (header::CACHE_CONTROL, "no-store"),
        ],
        Json(value),
    )
        .into_response()
}

fn write_json_error(status: StatusCode, code: &str, desc: &str) -> Response {
    write_json(status, json!({"error": code, "error_description": desc}))
}
