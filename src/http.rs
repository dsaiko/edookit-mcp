//! Streamable HTTP transport + axum integration. Port of the `runHTTP` /
//! `buildAuthServer` / `validatePublicURL` / `guardBindAddress` half of Go's
//! `main.go`.
//!
//! One axum app hosts everything (mirroring the Go `http.ServeMux`): the OAuth
//! AS routes, plus `/mcp` (the rmcp Streamable HTTP service) nested under a
//! Bearer-JWT gate and a 1 MiB body cap, with an outer host-allowlist layer
//! (DNS-rebinding kill-switch). No external auth gateway needed — only a TLS
//! terminator in front.

use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{Context, anyhow, bail};
use axum::Router;
use axum::extract::DefaultBodyLimit;
use axum::middleware::from_fn_with_state;
use rmcp::transport::streamable_http_server::session::local::LocalSessionManager;
use rmcp::transport::streamable_http_server::{StreamableHttpServerConfig, StreamableHttpService};

use crate::oauth::middleware::{build_allowed_hosts, require_bearer, require_known_host};
use crate::oauth::{self, Server as AuthServer};
use crate::server::EdookitServer;

/// 1 MiB cap on a POST /mcp body — rmcp reads the whole body before any
/// session/auth check, so without this a JWT holder could drive memory
/// exhaustion. Plenty for JSON-RPC tool args.
const MAX_MCP_BODY: usize = 1 << 20;

/// Serves the MCP server over Streamable HTTP at `addr`, gated by the built-in
/// OAuth AS. Shuts down gracefully on SIGINT/SIGTERM.
pub async fn run_http(template: EdookitServer, addr: String) -> anyhow::Result<()> {
    guard_bind_address(&addr)?;

    let auth = Arc::new(build_auth_server()?);
    auth.start_gc();
    let allowed_hosts = Arc::new(build_allowed_hosts(auth.public_url(), &[]));

    // rmcp Streamable HTTP service — a fresh server instance per session.
    let mcp_service = StreamableHttpService::new(
        move || Ok(template.clone()),
        Arc::new(LocalSessionManager::default()),
        StreamableHttpServerConfig::default(),
    );

    // /mcp under the bearer gate + body cap.
    let mcp_router = Router::new()
        .nest_service("/mcp", mcp_service)
        .layer(from_fn_with_state(auth.clone(), require_bearer))
        .layer(DefaultBodyLimit::max(MAX_MCP_BODY));

    // OAuth + discovery routes, merged, with the outer host allow-list.
    let app = auth
        .clone()
        .router()
        .merge(mcp_router)
        .layer(from_fn_with_state(allowed_hosts, require_known_host));

    let listener = tokio::net::TcpListener::bind(&addr)
        .await
        .with_context(|| format!("bind {addr}"))?;
    tracing::info!("edookit-mcp serving Streamable HTTP on {addr}/mcp (OAuth gated)");

    axum::serve(
        listener,
        app.into_make_service_with_connect_info::<SocketAddr>(),
    )
    .with_graceful_shutdown(shutdown_signal())
    .await
    .context("http transport")?;
    Ok(())
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

/// Assembles the OAuth AS from env. All secrets are required — a missing one
/// stops the endpoint from coming up half-configured (and thus open).
fn build_auth_server() -> anyhow::Result<AuthServer> {
    let public_url = validate_public_url(&crate::getenv_required("EDOOKIT_PUBLIC_URL")?)?;
    let password = crate::getenv_required("EDOOKIT_AUTH_PASSWORD")?;
    let secret = crate::getenv_required("EDOOKIT_JWT_SECRET")?;
    if secret.len() < 32 {
        bail!(
            "EDOOKIT_JWT_SECRET must be at least 32 bytes (got {})",
            secret.len()
        );
    }
    let mut cfg = oauth::Config::new(
        public_url.clone(),
        format!("{public_url}/mcp"),
        password,
        secret.into_bytes(),
    );
    cfg.login_username = std::env::var("EDOOKIT_AUTH_USERNAME").unwrap_or_default();
    AuthServer::new(cfg).map_err(|e| anyhow!("oauth: {e}"))
}

/// Returns the trimmed canonical PublicURL or an error if it isn't a bare https
/// origin. The value flows into JWT iss/aud + every discovery document, so a
/// stray path or plaintext scheme would silently produce wrong claims.
pub fn validate_public_url(raw: &str) -> anyhow::Result<String> {
    let raw = raw.trim_end_matches('/');
    let u = url::Url::parse(raw)
        .map_err(|e| anyhow!("EDOOKIT_PUBLIC_URL {raw:?}: malformed URL: {e}"))?;
    if u.scheme() != "https" {
        bail!(
            "EDOOKIT_PUBLIC_URL {raw:?}: must use https (got scheme {:?})",
            u.scheme()
        );
    }
    if u.host_str().is_none_or(|h| h.is_empty()) {
        bail!("EDOOKIT_PUBLIC_URL {raw:?}: must have a host");
    }
    if !u.username().is_empty() || u.password().is_some() {
        bail!("EDOOKIT_PUBLIC_URL {raw:?}: must not contain user info");
    }
    if u.path() != "/" && !u.path().is_empty() {
        bail!(
            "EDOOKIT_PUBLIC_URL {raw:?}: must be a bare origin (path {:?} not allowed)",
            u.path()
        );
    }
    if u.query().is_some() || raw.contains('#') {
        bail!("EDOOKIT_PUBLIC_URL {raw:?}: must be a bare origin (no query / fragment)");
    }
    Ok(raw.to_string())
}

/// Refuses a non-loopback bind unless EDOOKIT_BIND_NON_LOOPBACK=true. The
/// topology assumes a TLS terminator on loopback in front; a direct
/// non-loopback bind would expose the login form's plaintext password.
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

    #[test]
    fn validate_public_url_rules() {
        assert_eq!(
            validate_public_url("https://mcp.example/").unwrap(),
            "https://mcp.example"
        );
        assert_eq!(
            validate_public_url("https://mcp.example").unwrap(),
            "https://mcp.example"
        );
        assert!(
            validate_public_url("http://mcp.example").is_err(),
            "http rejected"
        );
        assert!(
            validate_public_url("https://mcp.example/path").is_err(),
            "path rejected"
        );
        assert!(
            validate_public_url("https://user:pw@mcp.example").is_err(),
            "userinfo rejected"
        );
        assert!(
            validate_public_url("https://mcp.example?x=1").is_err(),
            "query rejected"
        );
        assert!(
            validate_public_url("https://mcp.example#f").is_err(),
            "fragment rejected"
        );
        assert!(validate_public_url("not a url").is_err());
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
