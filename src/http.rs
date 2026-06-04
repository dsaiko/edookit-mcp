//! Streamable HTTP transport. Serves `/mcp` (the rmcp Streamable HTTP service)
//! behind a single **static bearer token** (`EDOOKIT_API_TOKEN`), a 1 MiB body
//! cap, and rmcp's own Host allow-list (DNS-rebinding guard, loopback by
//! default; widen with `EDOOKIT_ALLOWED_HOST` when behind a Host-preserving
//! proxy). The endpoint is otherwise unauthenticated — there is no built-in
//! OAuth/identity layer, so put TLS in front and keep the bind on loopback
//! unless you know what you're doing (`guard_bind_address`).

use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{Context, anyhow, bail};
use axum::Router;
use axum::extract::{DefaultBodyLimit, Request, State};
use axum::http::{StatusCode, header};
use axum::middleware::{Next, from_fn_with_state};
use axum::response::{IntoResponse, Response};
use rmcp::transport::streamable_http_server::session::local::LocalSessionManager;
use rmcp::transport::streamable_http_server::{StreamableHttpServerConfig, StreamableHttpService};
use subtle::ConstantTimeEq;

use crate::server::EdookitServer;

/// 1 MiB cap on a POST /mcp body — rmcp reads the whole body before the bearer
/// check, so without this a caller could drive memory exhaustion. Plenty for
/// JSON-RPC tool args.
const MAX_MCP_BODY: usize = 1 << 20;

/// Minimum length for `EDOOKIT_API_TOKEN` — a static shared secret short enough
/// to brute-force would defeat the gate.
const MIN_API_TOKEN_LEN: usize = 16;

/// Serves the MCP server over Streamable HTTP at `addr`, gated by a static
/// bearer token. Shuts down gracefully on SIGINT/SIGTERM.
pub async fn run_http(template: EdookitServer, addr: String) -> anyhow::Result<()> {
    guard_bind_address(&addr)?;
    // Fail closed: no token → the HTTP transport refuses to start, so `/mcp` is
    // never accidentally exposed unauthenticated.
    let token = Arc::new(api_token()?);
    let allowed = allowed_hosts();

    // rmcp Streamable HTTP service — a fresh server instance per session, with
    // rmcp's built-in Host allow-list (DNS-rebinding guard). Defaults to
    // loopback; `EDOOKIT_ALLOWED_HOST` adds the public name when behind a
    // Host-preserving reverse proxy.
    let mcp_service = StreamableHttpService::new(
        move || Ok(template.clone()),
        Arc::new(LocalSessionManager::default()),
        StreamableHttpServerConfig::default().with_allowed_hosts(allowed.iter().cloned()),
    );

    let app = Router::new()
        .nest_service("/mcp", mcp_service)
        .layer(from_fn_with_state(token, require_api_token))
        .layer(DefaultBodyLimit::max(MAX_MCP_BODY));

    let listener = tokio::net::TcpListener::bind(&addr)
        .await
        .with_context(|| format!("bind {addr}"))?;
    tracing::info!("edookit-mcp serving Streamable HTTP on {addr}/mcp (static bearer token)");

    axum::serve(
        listener,
        app.into_make_service_with_connect_info::<SocketAddr>(),
    )
    .with_graceful_shutdown(shutdown_signal())
    .await
    .context("http transport")?;
    Ok(())
}

/// Bearer-token gate: constant-time compares the request's `Authorization:
/// Bearer <token>` against `EDOOKIT_API_TOKEN`. 401 on absence/mismatch.
async fn require_api_token(State(token): State<Arc<String>>, req: Request, next: Next) -> Response {
    let provided = req
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(parse_bearer)
        .unwrap_or("");
    // Constant-time to avoid leaking the token via comparison timing. (Slice
    // ct_eq still short-circuits on length, which only reveals token length.)
    let ok: bool = provided.as_bytes().ct_eq(token.as_bytes()).into();
    if ok {
        next.run(req).await
    } else {
        (
            StatusCode::UNAUTHORIZED,
            [(header::WWW_AUTHENTICATE, "Bearer realm=\"edookit-mcp\"")],
            "unauthorized: missing or invalid bearer token",
        )
            .into_response()
    }
}

/// Extracts a Bearer token, case-insensitive on the scheme and tolerant of
/// surrounding whitespace. Exactly two whitespace-separated fields.
fn parse_bearer(authz: &str) -> Option<&str> {
    let mut it = authz.split_whitespace();
    let scheme = it.next()?;
    let token = it.next()?;
    if it.next().is_some() || !scheme.eq_ignore_ascii_case("bearer") {
        return None;
    }
    Some(token)
}

/// Reads + validates `EDOOKIT_API_TOKEN`. Required for the HTTP transport.
fn api_token() -> anyhow::Result<String> {
    let token = crate::getenv_required("EDOOKIT_API_TOKEN").map_err(|_| {
        anyhow!(
            "EDOOKIT_API_TOKEN is required for the HTTP transport — /mcp is gated by a static bearer token. Generate one with: openssl rand -base64 24"
        )
    })?;
    if token.len() < MIN_API_TOKEN_LEN {
        bail!(
            "EDOOKIT_API_TOKEN must be at least {MIN_API_TOKEN_LEN} characters (got {})",
            token.len()
        );
    }
    Ok(token)
}

/// rmcp Host allow-list: loopback always, plus any comma-separated hosts in
/// `EDOOKIT_ALLOWED_HOST` (the public name(s) when behind a Host-preserving
/// reverse proxy). Lowercased; ports are not included (rmcp strips the port
/// before matching).
fn allowed_hosts() -> Vec<String> {
    let mut hosts = vec![
        "127.0.0.1".to_string(),
        "localhost".to_string(),
        "::1".to_string(),
    ];
    if let Ok(extra) = std::env::var("EDOOKIT_ALLOWED_HOST") {
        hosts.extend(
            extra
                .split(',')
                .map(|h| h.trim().to_ascii_lowercase())
                .filter(|h| !h.is_empty()),
        );
    }
    hosts
}

async fn shutdown_signal() {
    let ctrl_c = async {
        let _ = tokio::signal::ctrl_c().await;
    };
    #[cfg(unix)]
    let term = async {
        if let Ok(mut s) = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        {
            s.recv().await;
        }
    };
    #[cfg(not(unix))]
    let term = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {}
        _ = term => {}
    }
    tracing::info!("shutdown signal received");
}

/// Refuses a non-loopback bind unless `EDOOKIT_BIND_NON_LOOPBACK=true`. The
/// topology assumes a TLS terminator on loopback in front; a direct
/// non-loopback bind would put the bearer token and Edookit data on the wire in
/// cleartext.
pub fn guard_bind_address(addr: &str) -> anyhow::Result<()> {
    if crate::getenv_bool("EDOOKIT_BIND_NON_LOOPBACK", false)? {
        return Ok(());
    }
    let (host, _port) = addr
        .rsplit_once(':')
        .ok_or_else(|| anyhow!("missing host in --http address {addr:?}: expected 127.0.0.1:PORT (set EDOOKIT_BIND_NON_LOOPBACK=true to override)"))?;
    if host.is_empty() {
        bail!(
            "--http {addr:?} binds all interfaces; use 127.0.0.1:PORT or set EDOOKIT_BIND_NON_LOOPBACK=true to override"
        );
    }
    let host = host
        .trim_start_matches('[')
        .trim_end_matches(']')
        .to_ascii_lowercase();
    let is_loopback = host == "localhost"
        || host
            .parse::<std::net::IpAddr>()
            .map(|ip| ip.is_loopback())
            .unwrap_or(false);
    if is_loopback {
        Ok(())
    } else {
        bail!(
            "--http {addr:?} binds a non-loopback address; the deployment topology expects a TLS terminator on loopback in front. Set EDOOKIT_BIND_NON_LOOPBACK=true to override"
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::Body;
    use axum::http::Request as HttpRequest;
    use axum::routing::get;
    use tower::ServiceExt;

    #[test]
    fn parse_bearer_cases() {
        assert_eq!(parse_bearer("Bearer abc"), Some("abc"));
        assert_eq!(parse_bearer("bearer abc"), Some("abc"));
        assert_eq!(parse_bearer("BEARER\tabc"), Some("abc"));
        assert_eq!(parse_bearer("  Bearer   abc  "), Some("abc"));
        assert_eq!(parse_bearer("Bearer"), None);
        assert_eq!(
            parse_bearer("Bearer a b"),
            None,
            "multi-word token rejected"
        );
        assert_eq!(parse_bearer("Basic abc"), None);
    }

    #[test]
    fn allowed_hosts_default_is_loopback_only() {
        // SAFETY: env mutation local to this test.
        unsafe { std::env::remove_var("EDOOKIT_ALLOWED_HOST") };
        let h = allowed_hosts();
        assert!(h.contains(&"127.0.0.1".to_string()));
        assert!(h.contains(&"localhost".to_string()));
        assert!(!h.iter().any(|x| x == "edookit.mcp.example"));
    }

    fn token_app(secret: &str) -> Router {
        Router::new()
            .route("/mcp", get(|| async { "ok" }))
            .layer(from_fn_with_state(
                Arc::new(secret.to_string()),
                require_api_token,
            ))
    }

    async fn status(app: Router, authz: Option<&str>) -> StatusCode {
        let mut req = HttpRequest::get("/mcp");
        if let Some(a) = authz {
            req = req.header(header::AUTHORIZATION, a);
        }
        app.oneshot(req.body(Body::empty()).unwrap())
            .await
            .unwrap()
            .status()
    }

    #[tokio::test]
    async fn token_gate_accepts_only_the_exact_token() {
        let secret = "super-secret-token-1234";
        assert_eq!(
            status(token_app(secret), Some(&format!("Bearer {secret}"))).await,
            StatusCode::OK
        );
        assert_eq!(
            status(token_app(secret), Some("Bearer wrong")).await,
            StatusCode::UNAUTHORIZED
        );
        assert_eq!(
            status(token_app(secret), None).await,
            StatusCode::UNAUTHORIZED,
            "no header → 401"
        );
        assert_eq!(
            status(token_app(secret), Some("Basic super-secret-token-1234")).await,
            StatusCode::UNAUTHORIZED,
            "wrong scheme → 401"
        );
    }

    #[test]
    fn guard_bind_address_rules() {
        // SAFETY: tests run single-threaded enough here; env var is local to these checks.
        unsafe { std::env::remove_var("EDOOKIT_BIND_NON_LOOPBACK") };
        assert!(guard_bind_address("127.0.0.1:9000").is_ok());
        assert!(guard_bind_address("localhost:9000").is_ok());
        assert!(guard_bind_address("[::1]:9000").is_ok());
        assert!(
            guard_bind_address("0.0.0.0:9000").is_err(),
            "non-loopback rejected"
        );
        assert!(guard_bind_address(":9000").is_err(), "bare port rejected");
        unsafe { std::env::set_var("EDOOKIT_BIND_NON_LOOPBACK", "true") };
        assert!(
            guard_bind_address("0.0.0.0:9000").is_ok(),
            "override allows non-loopback"
        );
        unsafe { std::env::remove_var("EDOOKIT_BIND_NON_LOOPBACK") };
    }
}
