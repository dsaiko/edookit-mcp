//! White-box tests for the client core. Port of Go's `client_test.go`:
//! config/URL validation, retry behaviour, off-origin re-login, the
//! `authenticated:false` envelope path, and the download-body classifier.
//! `super::*` gives access to the module's private items.

use super::*;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use wiremock::matchers::{method, path as mpath};
use wiremock::{Mock, MockServer, Request as WReq, Respond, ResponseTemplate};

// --- pure helpers -----------------------------------------------------------

#[test]
fn parse_base_url_validation() {
    assert!(parse_base_url("", false).is_err(), "empty rejected");
    assert!(
        parse_base_url("school.edookit.net", false).is_err(),
        "schemeless rejected"
    );
    assert!(
        parse_base_url("ftp://x.test", false).is_err(),
        "non-http scheme rejected"
    );
    assert!(parse_base_url("https://school.edookit.net", false).is_ok());

    // Plain http to a non-loopback host is rejected unless opted in.
    assert!(parse_base_url("http://school.edookit.net", false).is_err());
    assert!(parse_base_url("http://school.edookit.net", true).is_ok());
    // Loopback http is always allowed.
    assert!(parse_base_url("http://127.0.0.1:9000", false).is_ok());
    assert!(parse_base_url("http://localhost:9000", false).is_ok());
    assert!(parse_base_url("http://[::1]:9000", false).is_ok());
}

#[test]
fn base_url_default_port_is_normalized() {
    // url crate drops the default port, so :443 and bare denote one origin.
    let a = parse_base_url("https://school.test:443", false).unwrap();
    let b = parse_base_url("https://school.test", false).unwrap();
    assert!(same_origin(&a, &b));
    // A custom port is preserved and is a different origin.
    let c = parse_base_url("https://school.test:8443", false).unwrap();
    assert!(!same_origin(&a, &c));
}

#[test]
fn same_origin_compares_scheme_host_port() {
    let base = Url::parse("https://h.test").unwrap();
    assert!(same_origin(
        &Url::parse("https://h.test/path").unwrap(),
        &base
    ));
    assert!(
        !same_origin(&Url::parse("http://h.test").unwrap(), &base),
        "scheme differs"
    );
    assert!(
        !same_origin(&Url::parse("https://other.test").unwrap(), &base),
        "host differs"
    );
    assert!(
        !same_origin(&Url::parse("https://h.test:8443").unwrap(), &base),
        "port differs"
    );
}

#[test]
fn transient_status_set() {
    for c in [408, 502, 503, 504] {
        assert!(is_transient_status(c), "{c} is transient");
    }
    for c in [200, 400, 401, 404, 500, 501, 505] {
        assert!(!is_transient_status(c), "{c} is not transient");
    }
}

#[test]
fn classify_download_body_cases() {
    // auth-envelope JSON → reauth while retries remain, else fail.
    let env = br#"{"authenticated":false}"#;
    assert_eq!(
        classify_download_body("application/json", env, true),
        DownloadDisposition::Reauth
    );
    assert_eq!(
        classify_download_body("application/json", env, false),
        DownloadDisposition::Fail
    );
    // any other JSON is a real attachment.
    assert_eq!(
        classify_download_body("application/json", br#"{"k":1}"#, true),
        DownloadDisposition::Accept
    );
    // html → reauth once, but a genuine html attachment after re-login is kept.
    assert_eq!(
        classify_download_body("text/html", b"<html>", true),
        DownloadDisposition::Reauth
    );
    assert_eq!(
        classify_download_body("text/html", b"<html>", false),
        DownloadDisposition::Accept
    );
    // binary → always accept.
    assert_eq!(
        classify_download_body("application/pdf", b"%PDF", true),
        DownloadDisposition::Accept
    );
}

#[test]
fn auth_envelope_parsing() {
    assert_eq!(
        parse_auth_envelope(br#"{"authenticated":false}"#),
        Some(false)
    );
    assert_eq!(
        parse_auth_envelope(br#"{"authenticated":true}"#),
        Some(true)
    );
    assert_eq!(parse_auth_envelope(br#"{"other":1}"#), None);
    assert_eq!(parse_auth_envelope(b"not json"), None);
}

#[test]
fn requires_credentials() {
    let mut cfg = Config::new("https://school.edookit.net", "", "");
    assert!(Client::new(cfg).is_err());
    cfg = Config::new("https://school.edookit.net", "u", "p");
    assert!(Client::new(cfg).is_ok());
}

// --- HTTP behaviour (wiremock) ----------------------------------------------

/// A stateful responder: returns `steps[i]` for the i-th call, clamping to the
/// last step thereafter. Deterministic regardless of wiremock mount precedence.
struct Seq {
    i: AtomicUsize,
    steps: Vec<(u16, Option<serde_json::Value>)>,
}

impl Seq {
    fn new(steps: Vec<(u16, Option<serde_json::Value>)>) -> Self {
        Seq {
            i: AtomicUsize::new(0),
            steps,
        }
    }
}

impl Respond for Seq {
    fn respond(&self, _: &WReq) -> ResponseTemplate {
        let idx = self.i.fetch_add(1, Ordering::SeqCst);
        let (code, body) = self
            .steps
            .get(idx)
            .or_else(|| self.steps.last())
            .unwrap()
            .clone();
        let mut t = ResponseTemplate::new(code);
        if let Some(b) = body {
            t = t.set_body_json(b);
        }
        t
    }
}

fn build_client(uri: &str, login_calls: Arc<AtomicUsize>) -> Client {
    let lc = login_calls;
    let login_fn: LoginFn = Arc::new(move || {
        let lc = lc.clone();
        Box::pin(async move {
            lc.fetch_add(1, Ordering::SeqCst);
            Ok(vec![LoginCookie::new("X-EdooAuthToken", "tok")])
        })
    });
    let mut cfg = Config::new(uri, "user", "pass");
    cfg.retry_base_delay = Duration::from_millis(1); // keep retry tests fast
    cfg.login_fn = Some(login_fn);
    Client::new(cfg).unwrap()
}

/// Mounts the warmup `GET /` → 200 every server needs.
async fn mount_warmup(server: &MockServer) {
    Mock::given(method("GET"))
        .and(mpath("/"))
        .respond_with(ResponseTemplate::new(200))
        .mount(server)
        .await;
}

#[tokio::test]
async fn get_json_happy_path() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/handler/x"))
        .respond_with(Seq::new(vec![(200, Some(serde_json::json!({"ok": true})))]))
        .mount(&server)
        .await;

    let calls = Arc::new(AtomicUsize::new(0));
    let cli = build_client(&server.uri(), calls.clone());
    let v: serde_json::Value = cli.get_json("/handler/x").await.unwrap();
    assert_eq!(v["ok"], serde_json::json!(true));
    assert_eq!(calls.load(Ordering::SeqCst), 1, "logged in once");
}

#[tokio::test]
async fn retries_503_then_succeeds() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/handler/x"))
        .respond_with(Seq::new(vec![
            (503, None),
            (503, None),
            (200, Some(serde_json::json!({"ok": 1}))),
        ]))
        .mount(&server)
        .await;

    let cli = build_client(&server.uri(), Arc::new(AtomicUsize::new(0)));
    // default max_attempts = 3 → 503, 503, 200
    let v: serde_json::Value = cli.get_json("/handler/x").await.unwrap();
    assert_eq!(v["ok"], serde_json::json!(1));
}

#[tokio::test]
async fn persistent_503_errors_after_attempts() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/handler/x"))
        .respond_with(Seq::new(vec![(503, None)]))
        .mount(&server)
        .await;

    let cli = build_client(&server.uri(), Arc::new(AtomicUsize::new(0)));
    let err = cli
        .get_json::<serde_json::Value>("/handler/x")
        .await
        .unwrap_err();
    let msg = err.to_string();
    assert!(msg.contains("503"), "got: {msg}");
    assert!(msg.contains("3 attempt"), "got: {msg}");
}

#[tokio::test]
async fn http_500_is_not_retried() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    // expect exactly one hit — a 500 must not be retried.
    Mock::given(method("GET"))
        .and(mpath("/handler/x"))
        .respond_with(ResponseTemplate::new(500))
        .expect(1)
        .mount(&server)
        .await;

    let cli = build_client(&server.uri(), Arc::new(AtomicUsize::new(0)));
    let err = cli
        .get_json::<serde_json::Value>("/handler/x")
        .await
        .unwrap_err();
    assert!(err.to_string().contains("HTTP 500"), "got: {err}");
    server.verify().await;
}

#[tokio::test]
async fn authenticated_false_triggers_reauth() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/handler/x"))
        .respond_with(Seq::new(vec![
            (200, Some(serde_json::json!({"authenticated": false}))),
            (
                200,
                Some(serde_json::json!({"authenticated": true, "ok": 1})),
            ),
        ]))
        .mount(&server)
        .await;

    let calls = Arc::new(AtomicUsize::new(0));
    let cli = build_client(&server.uri(), calls.clone());
    let v: serde_json::Value = cli.get_json("/handler/x").await.unwrap();
    assert_eq!(v["ok"], serde_json::json!(1));
    assert!(
        calls.load(Ordering::SeqCst) >= 2,
        "re-logged in after authenticated:false"
    );
}

#[tokio::test]
async fn off_origin_redirect_exhausts_and_errors() {
    // server_b is where the redirect lands (different port → off-origin).
    let server_b = MockServer::start().await;
    Mock::given(method("GET"))
        .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({"ok": 1})))
        .mount(&server_b)
        .await;

    let server_a = MockServer::start().await;
    mount_warmup(&server_a).await;
    let location = format!("{}/handler/x", server_b.uri());
    Mock::given(method("GET"))
        .and(mpath("/handler/x"))
        .respond_with(ResponseTemplate::new(302).insert_header("Location", location.as_str()))
        .mount(&server_a)
        .await;

    let cli = build_client(&server_a.uri(), Arc::new(AtomicUsize::new(0)));
    let err = cli
        .get_json::<serde_json::Value>("/handler/x")
        .await
        .unwrap_err();
    assert!(err.to_string().contains("session expired"), "got: {err}");
}

#[tokio::test]
async fn off_origin_absolute_url_rejected_preflight() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    let cli = build_client(&server.uri(), Arc::new(AtomicUsize::new(0)));
    // An absolute URL on a foreign origin must be refused before dispatch.
    let err = cli
        .get_json::<serde_json::Value>("https://evil.example/steal")
        .await
        .unwrap_err();
    assert!(err.to_string().contains("off-origin"), "got: {err}");
}

#[tokio::test]
async fn get_bytes_enforces_size_cap() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/file"))
        .respond_with(ResponseTemplate::new(200).set_body_bytes(vec![0u8; 1000]))
        .mount(&server)
        .await;

    let cli = build_client(&server.uri(), Arc::new(AtomicUsize::new(0)));
    let err = cli.get_bytes("/file", 100).await.unwrap_err();
    assert!(matches!(err, ClientError::AttachmentTooLarge));
}

#[tokio::test]
async fn get_bytes_accepts_real_json_attachment() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/data.json"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(serde_json::json!({"hello": "world"})),
        )
        .mount(&server)
        .await;

    let cli = build_client(&server.uri(), Arc::new(AtomicUsize::new(0)));
    let (body, ct) = cli.get_bytes("/data.json", 1_000_000).await.unwrap();
    assert!(ct.contains("application/json"));
    assert!(!body.is_empty());
}
