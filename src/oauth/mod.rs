//! Built-in OAuth 2.1 Authorization Server for the Streamable HTTP transport.
//! Port of Go's `internal/oauth`. HS256 JWTs, open DCR, PKCE-S256, refresh
//! rotation with replay detection, per-IP/per-sub rate limiting, host
//! allowlist. No external auth gateway needed — only a TLS terminator in front.

pub mod jwt;
mod login;
pub mod middleware;
pub mod ratelimit;
pub mod server;

pub use server::{Config, Server};

use std::sync::Arc;

/// Injectable clock returning unix seconds. JWT iat/exp, code/refresh expiry,
/// and the rate limiters all read from this so tests can pin time.
pub type Clock = Arc<dyn Fn() -> i64 + Send + Sync>;

/// The production clock: wall-clock unix seconds.
pub fn system_clock() -> Clock {
    Arc::new(|| {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs() as i64)
            .unwrap_or(0)
    })
}

#[cfg(test)]
mod flow_tests {
    use super::*;
    use axum::Router;
    use axum::body::Body;
    use axum::http::{Request, StatusCode, header};
    use base64::Engine;
    use base64::engine::general_purpose::URL_SAFE_NO_PAD as B64URL;
    use sha2::{Digest, Sha256};
    use tower::ServiceExt;

    const PASSWORD: &str = "secret-pass";

    fn test_server() -> Arc<Server> {
        let clock: Clock = Arc::new(|| 1_000_000);
        let cfg = Config::new(
            "https://mcp.example".to_string(),
            "https://mcp.example/mcp".to_string(),
            PASSWORD.to_string(),
            b"0123456789012345678901234567890123".to_vec(),
        );
        Arc::new(Server::with_clock(cfg, clock).unwrap())
    }

    fn app(srv: Arc<Server>) -> Router {
        srv.router()
    }

    fn form(pairs: &[(&str, &str)]) -> String {
        let mut s = url::form_urlencoded::Serializer::new(String::new());
        for (k, v) in pairs {
            s.append_pair(k, v);
        }
        s.finish()
    }

    async fn body_string(resp: axum::response::Response) -> String {
        let b = axum::body::to_bytes(resp.into_body(), usize::MAX)
            .await
            .unwrap();
        String::from_utf8(b.to_vec()).unwrap()
    }

    async fn post_form(srv: &Arc<Server>, path: &str, body: String) -> axum::response::Response {
        app(srv.clone())
            .oneshot(
                Request::post(path)
                    .header(header::CONTENT_TYPE, "application/x-www-form-urlencoded")
                    .body(Body::from(body))
                    .unwrap(),
            )
            .await
            .unwrap()
    }

    async fn register_client(srv: &Arc<Server>) -> String {
        let resp = app(srv.clone())
            .oneshot(
                Request::post("/oauth/register")
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(
                        r#"{"redirect_uris":["https://client.example/cb"],"client_name":"Test"}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::CREATED);
        let v: serde_json::Value = serde_json::from_str(&body_string(resp).await).unwrap();
        v["client_id"].as_str().unwrap().to_string()
    }

    #[tokio::test]
    async fn full_flow_dcr_authorize_token_refresh_replay() {
        let srv = test_server();
        let client_id = register_client(&srv).await;

        // PKCE pair.
        let verifier = "a".repeat(43);
        let challenge = B64URL.encode(Sha256::digest(verifier.as_bytes()));

        // authorize submit → 302 with code.
        let authz = post_form(
            &srv,
            "/oauth/authorize",
            form(&[
                ("response_type", "code"),
                ("client_id", &client_id),
                ("redirect_uri", "https://client.example/cb"),
                ("scope", "offline_access"),
                ("state", "xyz"),
                ("code_challenge", &challenge),
                ("code_challenge_method", "S256"),
                ("username", "dusan"),
                ("password", PASSWORD),
            ]),
        )
        .await;
        assert_eq!(authz.status(), StatusCode::FOUND, "login should redirect");
        let loc = authz
            .headers()
            .get(header::LOCATION)
            .unwrap()
            .to_str()
            .unwrap();
        let loc_url = url::Url::parse(loc).unwrap();
        let code = loc_url
            .query_pairs()
            .find(|(k, _)| k == "code")
            .unwrap()
            .1
            .into_owned();
        assert_eq!(
            loc_url.query_pairs().find(|(k, _)| k == "state").unwrap().1,
            "xyz"
        );

        // exchange code → access + refresh.
        let tok = post_form(
            &srv,
            "/oauth/token",
            form(&[
                ("grant_type", "authorization_code"),
                ("code", &code),
                ("redirect_uri", "https://client.example/cb"),
                ("client_id", &client_id),
                ("code_verifier", &verifier),
            ]),
        )
        .await;
        assert_eq!(tok.status(), StatusCode::OK);
        let tok_json: serde_json::Value = serde_json::from_str(&body_string(tok).await).unwrap();
        let access = tok_json["access_token"].as_str().unwrap();
        let refresh1 = tok_json["refresh_token"].as_str().unwrap().to_string();
        // The access token verifies, and the subject is the fixed constant —
        // NOT the "dusan" typed into the form. With no EDOOKIT_AUTH_USERNAME
        // configured the password alone authenticates, so echoing the form
        // username into `sub` would let the client choose its own identity.
        assert_eq!(srv.verify_jwt(access).unwrap(), "edookit-mcp-user");

        // refresh → new tokens.
        let r = post_form(
            &srv,
            "/oauth/token",
            form(&[
                ("grant_type", "refresh_token"),
                ("refresh_token", &refresh1),
                ("client_id", &client_id),
            ]),
        )
        .await;
        assert_eq!(r.status(), StatusCode::OK);
        let r_json: serde_json::Value = serde_json::from_str(&body_string(r).await).unwrap();
        assert!(r_json["refresh_token"].is_string());
        assert_ne!(
            r_json["refresh_token"].as_str().unwrap(),
            refresh1,
            "rotated"
        );

        // replay the retired refresh token → chain invalidated.
        let replay = post_form(
            &srv,
            "/oauth/token",
            form(&[
                ("grant_type", "refresh_token"),
                ("refresh_token", &refresh1),
                ("client_id", &client_id),
            ]),
        )
        .await;
        assert_eq!(replay.status(), StatusCode::BAD_REQUEST);
        assert!(body_string(replay).await.contains("invalid_grant"));
    }

    #[tokio::test]
    async fn authorize_rejects_unregistered_redirect_uri() {
        let srv = test_server();
        let client_id = register_client(&srv).await;
        let resp = post_form(
            &srv,
            "/oauth/authorize",
            form(&[
                ("response_type", "code"),
                ("client_id", &client_id),
                ("redirect_uri", "https://attacker.example/steal"),
                ("code_challenge", &"a".repeat(43)),
                ("code_challenge_method", "S256"),
                ("password", PASSWORD),
            ]),
        )
        .await;
        // Bad redirect_uri → 400 inline, NEVER a 302 to the attacker.
        assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn wrong_password_re_renders_form_401() {
        let srv = test_server();
        let client_id = register_client(&srv).await;
        let resp = post_form(
            &srv,
            "/oauth/authorize",
            form(&[
                ("response_type", "code"),
                ("client_id", &client_id),
                ("redirect_uri", "https://client.example/cb"),
                ("code_challenge", &"a".repeat(43)),
                ("code_challenge_method", "S256"),
                ("password", "wrong"),
            ]),
        )
        .await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn discovery_metadata_served() {
        let srv = test_server();
        let resp = app(srv.clone())
            .oneshot(
                Request::get("/.well-known/oauth-authorization-server")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        let v: serde_json::Value = serde_json::from_str(&body_string(resp).await).unwrap();
        assert_eq!(v["issuer"], "https://mcp.example");
        assert_eq!(v["code_challenge_methods_supported"][0], "S256");

        // path-qualified protected-resource metadata (audience has /mcp).
        let pr = app(srv.clone())
            .oneshot(
                Request::get("/.well-known/oauth-protected-resource/mcp")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(pr.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn dcr_throttle_blocks_after_max_registrations() {
        let srv = test_server();
        let body = r#"{"redirect_uris":["https://c.example/cb"]}"#;
        let mut statuses = Vec::new();
        // Fixed clock → window never advances; the 6th (dcr max = 5) is blocked.
        for _ in 0..6 {
            let resp = app(srv.clone())
                .oneshot(
                    Request::post("/oauth/register")
                        .header(header::CONTENT_TYPE, "application/json")
                        .body(Body::from(body))
                        .unwrap(),
                )
                .await
                .unwrap();
            statuses.push(resp.status());
        }
        assert_eq!(statuses[0], StatusCode::CREATED);
        assert_eq!(
            *statuses.last().unwrap(),
            StatusCode::TOO_MANY_REQUESTS,
            "6th DCR from one IP is rate-limited"
        );
    }

    #[tokio::test]
    async fn register_rejects_oversized_body() {
        let srv = test_server();
        // > 16 KiB body → the DefaultBodyLimit on /oauth/register rejects it.
        let big = format!(
            r#"{{"redirect_uris":["https://c.example/cb"],"client_name":"{}"}}"#,
            "x".repeat(20_000)
        );
        let resp = post_form(&srv, "/oauth/register", big).await;
        assert_eq!(resp.status(), StatusCode::PAYLOAD_TOO_LARGE);
    }
}
