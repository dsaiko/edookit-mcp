//! Bearer-JWT gate + DNS-rebinding host allowlist. Port of Go's
//! `internal/oauth/middleware.go`. These are axum middleware (wired onto the
//! router in the HTTP transport phase).

use std::collections::HashSet;
use std::sync::Arc;

use axum::extract::{Request, State};
use axum::http::{StatusCode, header};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use serde_json::json;

use super::server::Server;

/// Rejects requests without a valid Bearer JWT with 401 + a WWW-Authenticate
/// header pointing at the protected-resource metadata (RFC 9728 discovery).
pub async fn require_bearer(State(srv): State<Arc<Server>>, req: Request, next: Next) -> Response {
    let authz = req
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    let Some(token) = parse_bearer(authz) else {
        return unauthorized(&srv, "invalid_request", "no bearer token in request");
    };
    let sub = match srv.verify_jwt(token) {
        Ok(s) => s,
        Err(e) => return unauthorized(&srv, "invalid_token", e.as_str()),
    };
    // Per-subject token bucket — caps amplification a leaked JWT can drive.
    if !srv.mcp_allow(&sub) {
        return (
            StatusCode::TOO_MANY_REQUESTS,
            [(header::RETRY_AFTER, "1")],
            "rate-limited: too many requests for this subject",
        )
            .into_response();
    }
    next.run(req).await
}

/// Drops any request whose Host header isn't in the allow-list (closes
/// DNS-rebinding against the loopback listener). Empty Host → 421.
pub async fn require_known_host(
    State(allowed): State<Arc<HashSet<String>>>,
    req: Request,
    next: Next,
) -> Response {
    let host = req
        .headers()
        .get(header::HOST)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    if host.is_empty() {
        return (StatusCode::MISDIRECTED_REQUEST, "missing Host header").into_response();
    }
    if !allowed.contains(&normalize_host(host)) {
        return (StatusCode::MISDIRECTED_REQUEST, "unknown host").into_response();
    }
    next.run(req).await
}

/// Builds the host allow-list: loopback names + the host of `public_url` + any
/// extras, all normalized.
pub fn build_allowed_hosts(public_url: &str, extra: &[String]) -> HashSet<String> {
    let mut allow: HashSet<String> = ["127.0.0.1", "::1", "localhost"]
        .iter()
        .map(|s| s.to_string())
        .collect();
    if let Ok(u) = url::Url::parse(public_url)
        && let Some(h) = u.host_str()
        && !h.is_empty()
    {
        allow.insert(normalize_host(h));
    }
    for h in extra {
        if !h.is_empty() {
            allow.insert(normalize_host(h));
        }
    }
    allow
}

/// Canonicalizes a Host header for comparison: lowercase, strip port, strip
/// IPv6 brackets.
pub fn normalize_host(h: &str) -> String {
    let h = h.to_ascii_lowercase();
    // Bracketed IPv6: "[host]" or "[host]:port" → the inside of the brackets.
    if let Some(rest) = h.strip_prefix('[')
        && let Some(end) = rest.find(']')
    {
        return rest[..end].to_string();
    }
    // "host:port" where the host part has no ':' (IPv4 / hostname) → strip port.
    if let Some((host, port)) = h.rsplit_once(':')
        && !host.contains(':')
        && !port.is_empty()
        && port.bytes().all(|b| b.is_ascii_digit())
    {
        return host.to_string();
    }
    h
}

/// Extracts a Bearer token, case-insensitive on the scheme and tolerant of
/// surrounding whitespace. Exactly two whitespace-separated fields.
pub fn parse_bearer(authz: &str) -> Option<&str> {
    let mut it = authz.split_whitespace();
    let scheme = it.next()?;
    let token = it.next()?;
    if it.next().is_some() || !scheme.eq_ignore_ascii_case("bearer") {
        return None;
    }
    Some(token)
}

fn unauthorized(srv: &Server, code: &str, desc: &str) -> Response {
    let www = format!(
        "Bearer realm=\"edookit-mcp\", error={code:?}, error_description={desc:?}, resource_metadata={:?}",
        srv.protected_resource_metadata_url()
    );
    (
        StatusCode::UNAUTHORIZED,
        [(header::WWW_AUTHENTICATE, www)],
        axum::Json(json!({"error": code, "error_description": desc})),
    )
        .into_response()
}

#[cfg(test)]
mod tests {
    use super::*;

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
    fn normalize_host_cases() {
        assert_eq!(normalize_host("MCP.EXAMPLE:443"), "mcp.example");
        assert_eq!(normalize_host("mcp.example"), "mcp.example");
        assert_eq!(normalize_host("127.0.0.1:9000"), "127.0.0.1");
        assert_eq!(normalize_host("[::1]:9000"), "::1");
        assert_eq!(normalize_host("localhost"), "localhost");
    }

    #[test]
    fn allowed_hosts_includes_public_and_loopback() {
        let hosts = build_allowed_hosts("https://edookit.mcp.example", &[]);
        assert!(hosts.contains("edookit.mcp.example"));
        assert!(hosts.contains("127.0.0.1"));
        assert!(hosts.contains("localhost"));
        assert!(!hosts.contains("attacker.example"));
    }
}
