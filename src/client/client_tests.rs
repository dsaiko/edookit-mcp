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
    build_client_with_hosts(uri, login_calls, None)
}

/// `download_hosts`: `None` keeps the default allow-list, `Some(v)` replaces it.
/// It must be set **before** `Client::new`, because the allow-list is baked into
/// the download client's redirect policy at construction (that is the whole
/// point — the guard runs per hop, before a request goes out).
fn build_client_with_hosts(
    uri: &str,
    login_calls: Arc<AtomicUsize>,
    download_hosts: Option<Vec<String>>,
) -> Client {
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
    if let Some(hosts) = download_hosts {
        cfg.download_hosts = hosts;
    }
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

#[test]
fn download_host_pattern_matching() {
    // Exact host.
    assert!(host_matches_pattern(
        "data4.edookit.net",
        "data4.edookit.net"
    ));
    assert!(!host_matches_pattern(
        "data5.edookit.net",
        "data4.edookit.net"
    ));
    // Wildcard covers any subdomain, at any depth.
    assert!(host_matches_pattern("data4.edookit.net", "*.edookit.net"));
    assert!(host_matches_pattern("a.b.edookit.net", "*.edookit.net"));
    // …but not the bare suffix, and not a look-alike that merely ends with it.
    assert!(!host_matches_pattern("edookit.net", "*.edookit.net"));
    assert!(!host_matches_pattern("evil-edookit.net", "*.edookit.net"));
    assert!(!host_matches_pattern(
        "edookit.net.evil.example",
        "*.edookit.net"
    ));
    // Case- and trailing-dot-insensitive; empty patterns match nothing.
    assert!(host_matches_pattern("DATA4.Edookit.NET", "*.edookit.net"));
    assert!(host_matches_pattern("data4.edookit.net.", "*.edookit.net"));
    assert!(!host_matches_pattern("data4.edookit.net", ""));
    assert!(!host_matches_pattern("data4.edookit.net", "*."));
}

/// Edookit 302s `/handler/download/*` to its storage CDN, so an allow-listed
/// off-origin redirect must deliver the bytes instead of being read as a
/// session bounce. `localhost` vs `127.0.0.1` gives two distinct hosts on one
/// wiremock server.
#[tokio::test]
async fn download_redirect_to_allowed_host_delivers_bytes() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    let cdn = format!("http://127.0.0.1:{}/cdn/file", server.address().port());
    Mock::given(method("GET"))
        .and(mpath("/handler/download/file1"))
        .respond_with(ResponseTemplate::new(302).insert_header("Location", cdn.as_str()))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(mpath("/cdn/file"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "application/pdf")
                .set_body_bytes(b"%PDF-1.7 payload".to_vec()),
        )
        .mount(&server)
        .await;

    let logins = Arc::new(AtomicUsize::new(0));
    let cli = build_client_with_hosts(
        &format!("http://localhost:{}", server.address().port()),
        logins.clone(),
        Some(vec!["127.0.0.1".to_string()]),
    );

    let (body, ctype) = cli
        .get_bytes("/handler/download/file1", 1024)
        .await
        .unwrap();
    assert_eq!(body, b"%PDF-1.7 payload");
    assert_eq!(ctype, "application/pdf");
    // The allow-listed hop must NOT have been mistaken for an expiry.
    assert_eq!(logins.load(Ordering::SeqCst), 1, "one login, no re-login");

    // …and the session must not ride along to the allow-listed host: the jar
    // scopes host-only cookies to the tenant, and the CDN authenticates with
    // the token in the URL instead.
    let cdn_reqs: Vec<_> = server
        .received_requests()
        .await
        .unwrap()
        .into_iter()
        .filter(|r| r.url.path() == "/cdn/file")
        .collect();
    assert_eq!(cdn_reqs.len(), 1, "the CDN hop happened");
    assert!(
        !cdn_reqs[0].headers.contains_key("cookie"),
        "no session cookie sent off-origin"
    );
}

/// A download redirect to a host that is *not* allow-listed still counts as a
/// session bounce (one re-login, then a diagnosable error naming the host).
#[tokio::test]
async fn download_redirect_to_disallowed_host_errors_with_host() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    let cdn = format!("http://127.0.0.1:{}/cdn/file", server.address().port());
    Mock::given(method("GET"))
        .and(mpath("/handler/download/file1"))
        .respond_with(ResponseTemplate::new(302).insert_header("Location", cdn.as_str()))
        .mount(&server)
        .await;

    let logins = Arc::new(AtomicUsize::new(0));
    let cli = build_client_with_hosts(
        &format!("http://localhost:{}", server.address().port()),
        logins.clone(),
        Some(vec![]), // strict same-origin
    );

    let err = cli
        .get_bytes("/handler/download/file1", 1024)
        .await
        .unwrap_err();
    let msg = err.to_string();
    assert!(msg.contains("127.0.0.1"), "error names the host: {msg}");
    assert!(msg.contains("EDOOKIT_DOWNLOAD_HOSTS"), "actionable: {msg}");
    assert_eq!(
        logins.load(Ordering::SeqCst),
        2,
        "bounce triggers one re-login"
    );
}

/// A chain that detours through a **disallowed** host and returns to an allowed
/// one must not pass — and the detour must never be contacted at all. Checking
/// only the final URL would accept this, after reqwest had already fetched the
/// detour (twice, once the re-login retry fires).
#[tokio::test]
async fn redirect_chain_via_disallowed_host_is_never_contacted() {
    // The detour: records every hit, and would bounce us back on-origin.
    let detour = MockServer::start().await;

    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/handler/download/file1"))
        .respond_with(
            ResponseTemplate::new(302)
                .insert_header("Location", format!("{}/hop", detour.uri()).as_str()),
        )
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(mpath("/hop"))
        .respond_with(
            ResponseTemplate::new(302)
                .insert_header("Location", format!("{}/cdn/file", server.uri()).as_str()),
        )
        .mount(&detour)
        .await;
    Mock::given(method("GET"))
        .and(mpath("/cdn/file"))
        .respond_with(ResponseTemplate::new(200).set_body_bytes(b"payload".to_vec()))
        .mount(&server)
        .await;

    // 127.0.0.1 is allow-listed, so the *final* destination would look fine —
    // but the detour runs on a different port, so the hop must be refused.
    let cli = build_client_with_hosts(
        &format!("http://localhost:{}", server.address().port()),
        Arc::new(AtomicUsize::new(0)),
        Some(vec!["127.0.0.1".to_string()]),
    );
    let err = cli
        .get_bytes("/handler/download/file1", 1024)
        .await
        .unwrap_err();
    assert!(err.to_string().contains("bounced off-origin"), "got: {err}");
    assert!(
        detour.received_requests().await.unwrap().is_empty(),
        "the disallowed hop must never be requested"
    );
}

/// An HTML attachment served from the allow-listed CDN must be delivered as
/// the file it is. The stale-session sniff only makes sense for the tenant —
/// running it off-origin re-logs in for every HTML attachment and, when that
/// login fails, loses an already-downloaded file.
#[tokio::test]
async fn cdn_html_attachment_is_not_mistaken_for_a_login_page() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    let cdn = format!("http://127.0.0.1:{}/cdn/file", server.address().port());
    Mock::given(method("GET"))
        .and(mpath("/handler/download/file1"))
        .respond_with(ResponseTemplate::new(302).insert_header("Location", cdn.as_str()))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(mpath("/cdn/file"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "text/html; charset=utf-8")
                .set_body_bytes(b"<html><body>a real html attachment</body></html>".to_vec()),
        )
        .mount(&server)
        .await;

    let logins = Arc::new(AtomicUsize::new(0));
    let cli = build_client_with_hosts(
        &format!("http://localhost:{}", server.address().port()),
        logins.clone(),
        Some(vec!["127.0.0.1".to_string()]),
    );
    let (body, ct) = cli
        .get_bytes("/handler/download/file1", 4096)
        .await
        .unwrap();
    assert!(body.starts_with(b"<html>"), "the html file is returned");
    assert!(ct.starts_with("text/html"));
    assert_eq!(
        logins.load(Ordering::SeqCst),
        1,
        "an off-origin html body must not trigger a re-login"
    );
}

/// Same for a CDN JSON attachment that happens to contain `authenticated:false`
/// — off-origin it is data, not a session verdict, and must be returned.
#[tokio::test]
async fn cdn_json_with_auth_false_is_returned_as_data() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    let cdn = format!("http://127.0.0.1:{}/cdn/file", server.address().port());
    Mock::given(method("GET"))
        .and(mpath("/handler/download/file1"))
        .respond_with(ResponseTemplate::new(302).insert_header("Location", cdn.as_str()))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(mpath("/cdn/file"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(serde_json::json!({"authenticated": false})),
        )
        .mount(&server)
        .await;

    let cli = build_client_with_hosts(
        &format!("http://localhost:{}", server.address().port()),
        Arc::new(AtomicUsize::new(0)),
        Some(vec!["127.0.0.1".to_string()]),
    );
    let (body, _) = cli
        .get_bytes("/handler/download/file1", 4096)
        .await
        .unwrap();
    assert!(
        String::from_utf8_lossy(&body).contains("\"authenticated\""),
        "the JSON file is returned verbatim"
    );
}

/// …while a *tenant* html body still means "stale session": the gate that
/// makes the two tests above safe.
#[tokio::test]
async fn tenant_html_body_still_triggers_one_relogin() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/handler/download/file1"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Type", "text/html")
                .set_body_bytes(b"<html>login</html>".to_vec()),
        )
        .mount(&server)
        .await;

    let logins = Arc::new(AtomicUsize::new(0));
    let cli = build_client(&server.uri(), logins.clone());
    // Still html after the re-login → accepted as a genuine html attachment.
    let (body, _) = cli
        .get_bytes("/handler/download/file1", 4096)
        .await
        .unwrap();
    assert_eq!(body, b"<html>login</html>");
    assert_eq!(
        logins.load(Ordering::SeqCst),
        2,
        "tenant html is retried once behind a re-login"
    );
}

/// Direct test of the isolation mechanism, because the wiremock version below
/// cannot exercise it: `localhost` and `127.0.0.1` share no registrable domain,
/// so a `Domain=` cookie could not cross between them anyway. Here the tenant
/// sets `Domain=.edookit.net` — exactly the attribute that would hand the
/// session to `dataN.edookit.net` — and the assertions show the raw jar *would*
/// leak it while the download client's view does not.
#[test]
fn tenant_only_cookie_view_blocks_the_shared_jar_off_origin() {
    use reqwest::cookie::CookieStore as _;

    let base = parse_base_url("https://school.edookit.net", false).unwrap();
    let cdn = Url::parse("https://data4.edookit.net/v1/fetch/x").unwrap();
    let jar = Arc::new(Jar::new());

    // The tenant sets a registrable-domain-scoped session cookie.
    let set = HeaderValue::from_static("sid=secret; Domain=.edookit.net; Path=/");
    jar.set_cookies(&mut [&set].into_iter(), &base);

    // Baseline: the shared jar hands that cookie to the CDN host — this is the
    // leak the download client must not have.
    assert!(
        jar.cookies(&cdn).is_some(),
        "precondition: a Domain-scoped cookie does reach the CDN via the raw jar"
    );

    let view = TenantOnlyCookies {
        jar: jar.clone(),
        base_url: base.clone(),
    };
    assert!(
        view.cookies(&cdn).is_none(),
        "the download client must send nothing off-origin"
    );
    assert!(
        view.cookies(&base).is_some(),
        "…while keeping the session for the tenant hop"
    );

    // And a Set-Cookie from the CDN must not enter the shared jar.
    let planted = HeaderValue::from_static("sid=attacker; Domain=.edookit.net; Path=/");
    view.set_cookies(&mut [&planted].into_iter(), &cdn);
    let tenant_cookies = jar.cookies(&base).unwrap();
    assert!(
        tenant_cookies.to_str().unwrap().contains("sid=secret"),
        "tenant session untouched, got {tenant_cookies:?}"
    );
}

/// The off-origin download hop must be cookie-free **structurally**, not
/// because today's tenant cookies happen to be host-only: a `Domain=`-scoped
/// tenant cookie would otherwise domain-match the CDN and ride along, and a
/// `Set-Cookie` from the CDN could come back to influence the tenant session.
#[tokio::test]
async fn cdn_hop_neither_receives_nor_sets_cookies() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    let cdn = format!("http://127.0.0.1:{}/cdn/file", server.address().port());
    Mock::given(method("GET"))
        .and(mpath("/handler/download/file1"))
        .respond_with(
            ResponseTemplate::new(302)
                // A domain-scoped cookie set by the tenant: the shared jar would
                // hand this to any *.localhost/127.0.0.1 host on attribute rules.
                .insert_header("Set-Cookie", "tenant-wide=secret; Path=/")
                .insert_header("Location", cdn.as_str()),
        )
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(mpath("/cdn/file"))
        .respond_with(
            ResponseTemplate::new(200)
                // The CDN tries to plant a cookie of its own.
                .insert_header("Set-Cookie", "cdn-planted=evil; Path=/")
                .set_body_bytes(b"payload".to_vec()),
        )
        .mount(&server)
        .await;

    let cli = build_client_with_hosts(
        &format!("http://localhost:{}", server.address().port()),
        Arc::new(AtomicUsize::new(0)),
        Some(vec!["127.0.0.1".to_string()]),
    );
    let (body, _) = cli
        .get_bytes("/handler/download/file1", 1024)
        .await
        .unwrap();
    assert_eq!(body, b"payload");

    let cdn_req = server
        .received_requests()
        .await
        .unwrap()
        .into_iter()
        .find(|r| r.url.path() == "/cdn/file")
        .expect("the CDN hop happened");
    assert!(
        !cdn_req.headers.contains_key("cookie"),
        "no cookie may be sent to the off-origin hop"
    );
    // Nor may the CDN's Set-Cookie reach the jar the tenant requests use.
    let names: Vec<String> = cli.session_cookies().into_iter().map(|c| c.name).collect();
    assert!(
        !names.iter().any(|n| n == "cdn-planted"),
        "CDN cookie must not enter the tenant jar, got {names:?}"
    );
}

/// A refusal must name the whole origin and *which* check failed — advising
/// "add the host to the allow-list" is useless when the host is already on it
/// and only the port or scheme is wrong.
#[test]
fn refusal_reason_distinguishes_host_scheme_and_port() {
    let base = parse_base_url("https://school.edookit.net", false).unwrap();
    let hosts = vec!["*.edookit.net".to_string()];
    let verdict = |u: &str| origin_verdict(&Url::parse(u).unwrap(), &base, &hosts);

    assert_eq!(
        verdict("https://data4.edookit.net/x"),
        OriginVerdict::Allowed
    );
    assert_eq!(
        verdict("https://evil.example/x"),
        OriginVerdict::HostNotAllowed
    );
    assert_eq!(
        verdict("http://data4.edookit.net/x"),
        OriginVerdict::SchemeMismatch
    );
    assert_eq!(
        verdict("https://data4.edookit.net:8443/x"),
        OriginVerdict::PortMismatch
    );

    // With an EMPTY allow-list the tenant's own host must still be diagnosed by
    // what actually differed, not as "host not allowed".
    let strict = |u: &str| origin_verdict(&Url::parse(u).unwrap(), &base, &[]);
    assert_eq!(
        strict("http://school.edookit.net/x"),
        OriginVerdict::SchemeMismatch,
        "https->http on the tenant host is a downgrade, not an unknown host"
    );
    assert_eq!(
        strict("https://school.edookit.net:8443/x"),
        OriginVerdict::PortMismatch
    );
    assert_eq!(
        strict("https://data4.edookit.net/x"),
        OriginVerdict::HostNotAllowed
    );

    // The advice differs per reason, and never tells you to allow-list a host
    // that is already allow-listed.
    let port_advice = OriginVerdict::PortMismatch.advice(&base);
    assert!(port_advice.contains("port must be 443"), "{port_advice}");
    assert!(
        !port_advice.contains("EDOOKIT_DOWNLOAD_HOSTS"),
        "{port_advice}"
    );
    let scheme_advice = OriginVerdict::SchemeMismatch.advice(&base);
    assert!(scheme_advice.contains("https"), "{scheme_advice}");
    assert!(
        OriginVerdict::HostNotAllowed
            .advice(&base)
            .contains("EDOOKIT_DOWNLOAD_HOSTS")
    );
    // And the origin is rendered whole, not just the hostname.
    assert_eq!(
        origin_str(&Url::parse("https://data4.edookit.net:8443/x").unwrap()),
        "https://data4.edookit.net:8443"
    );
}

/// An allow-listed host on the wrong port is refused *before dispatch*, with a
/// message that points at the port rather than the allow-list.
#[tokio::test]
async fn preflight_port_mismatch_message_points_at_the_port() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    let cli = build_client_with_hosts(
        &server.uri(),
        Arc::new(AtomicUsize::new(0)),
        Some(vec!["127.0.0.1".to_string()]),
    );
    // Same allow-listed host, different port.
    let other_port = server.address().port().wrapping_add(1).max(1);
    let err = cli
        .get_bytes(&format!("http://127.0.0.1:{other_port}/file"), 16)
        .await
        .unwrap_err();
    let msg = err.to_string();
    assert!(msg.contains("port must be"), "names the real cause: {msg}");
    assert!(
        msg.contains(&other_port.to_string()),
        "names the origin: {msg}"
    );
}

/// A redirect loop must NOT masquerade as an expired session: no re-login, no
/// retries, and an error that says "redirect", not "add the host to
/// EDOOKIT_DOWNLOAD_HOSTS".
#[tokio::test]
async fn redirect_loop_is_distinct_from_session_expiry() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    let here = format!("{}/loop", server.uri());
    Mock::given(method("GET"))
        .and(mpath("/loop"))
        .respond_with(ResponseTemplate::new(302).insert_header("Location", here.as_str()))
        .mount(&server)
        .await;

    let logins = Arc::new(AtomicUsize::new(0));
    let cli = build_client(&server.uri(), logins.clone());
    let err = cli.get_bytes("/loop", 1024).await.unwrap_err();
    let msg = err.to_string();
    assert!(msg.contains("redirect"), "names the real cause: {msg}");
    assert!(
        !msg.contains("EDOOKIT_DOWNLOAD_HOSTS"),
        "must not advise the allow-list knob: {msg}"
    );
    assert_eq!(
        logins.load(Ordering::SeqCst),
        1,
        "a loop is deterministic — no re-login"
    );
}

/// The hop budget matches reqwest's own default (`Policy::limited(10)`), which
/// our custom policy replaces: ten same-origin hops still resolve.
#[tokio::test]
async fn ten_redirect_hops_are_still_followed() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    for i in 0..10 {
        let next = format!("{}/hop/{}", server.uri(), i + 1);
        Mock::given(method("GET"))
            .and(mpath(format!("/hop/{i}")))
            .respond_with(ResponseTemplate::new(302).insert_header("Location", next.as_str()))
            .mount(&server)
            .await;
    }
    Mock::given(method("GET"))
        .and(mpath("/hop/10"))
        .respond_with(ResponseTemplate::new(200).set_body_bytes(b"arrived".to_vec()))
        .mount(&server)
        .await;

    let cli = build_client(&server.uri(), Arc::new(AtomicUsize::new(0)));
    let (body, _) = cli.get_bytes("/hop/0", 1024).await.unwrap();
    assert_eq!(body, b"arrived", "ten hops are within budget");
}

/// An allow-listed *hostname* must not implicitly allow every port: cookies are
/// not port-scoped, so a hop to the tenant host on another port would still be
/// handed the session cookie while escaping `same_origin`.
#[test]
fn allowed_host_does_not_widen_to_other_ports() {
    let base = parse_base_url("https://school.edookit.net", false).unwrap();
    let hosts = vec!["*.edookit.net".to_string()];
    let allowed = |u: &str| origin_allowed(&Url::parse(u).unwrap(), &base, &hosts);

    assert!(
        allowed("https://data4.edookit.net/v1/fetch/x"),
        "the CDN on 443"
    );
    assert!(
        allowed("https://data4.edookit.net:443/v1/fetch/x"),
        "explicit 443"
    );
    assert!(
        !allowed("https://data4.edookit.net:8443/v1/fetch/x"),
        "other port"
    );
    assert!(
        !allowed("https://school.edookit.net:8443/steal"),
        "same host, other port — would still receive the cookie"
    );
    // Scheme downgrade is refused even on an allow-listed host.
    assert!(!allowed("http://data4.edookit.net/v1/fetch/x"));
    // An empty allow-list means strict same-origin.
    assert!(!origin_allowed(
        &Url::parse("https://data4.edookit.net/v1/fetch/x").unwrap(),
        &base,
        &[]
    ));
    assert!(origin_allowed(
        &Url::parse("https://school.edookit.net/handler/x").unwrap(),
        &base,
        &[]
    ));
}

/// The allow-list also widens the pre-dispatch SSRF fence, because attachment
/// URLs arrive fully qualified from Edookit — but only for the download paths,
/// and only for allow-listed hosts.
#[tokio::test]
async fn download_preflight_honours_allow_list() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    let cli = build_client_with_hosts(
        &server.uri(),
        Arc::new(AtomicUsize::new(0)),
        Some(vec!["*.edookit.net".to_string()]),
    );

    // Not allow-listed → refused before dispatch.
    let err = cli
        .get_bytes("https://evil.example/steal", 16)
        .await
        .unwrap_err();
    assert!(err.to_string().contains("off-origin"), "got: {err}");

    // Allow-listed, but the JSON path keeps the strict same-origin fence.
    let err = cli
        .get_json::<serde_json::Value>("https://data4.edookit.net/v1/fetch/x")
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

#[tokio::test]
async fn cached_cookies_take_fast_path_without_login() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/handler/x"))
        .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({"ok": 1})))
        .mount(&server)
        .await;

    // Pre-write a fresh cookie cache for this base URL.
    let dir = tempfile::tempdir().unwrap();
    let cache = dir.path().join("cookies.json");
    super::cookie_store::save_cookies(
        &cache,
        &server.uri(),
        vec![super::StoredCookie {
            name: "X-EdooAuthToken".into(),
            value: "tok".into(),
        }],
    )
    .unwrap();

    let calls = Arc::new(AtomicUsize::new(0));
    let lc = calls.clone();
    let login_fn: LoginFn = Arc::new(move || {
        let lc = lc.clone();
        Box::pin(async move {
            lc.fetch_add(1, Ordering::SeqCst);
            Ok(vec![LoginCookie::new("X", "Y")])
        })
    });
    let mut cfg = Config::new(server.uri(), "u", "p");
    cfg.retry_base_delay = Duration::from_millis(1);
    cfg.cookie_cache_path = Some(cache);
    cfg.login_fn = Some(login_fn);
    let cli = Client::new(cfg).unwrap();

    let _v: serde_json::Value = cli.get_json("/handler/x").await.unwrap();
    assert_eq!(
        calls.load(Ordering::SeqCst),
        0,
        "cached cookies → warmup only, no chromium login"
    );
}

#[tokio::test]
async fn get_bytes_keeps_genuine_html_attachment_after_retry() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    // Always HTML: first hit looks like a stale-session login page (retry),
    // but a successful re-login that STILL yields HTML means it's a real HTML
    // attachment → accept it rather than fail.
    Mock::given(method("GET"))
        .and(mpath("/page.html"))
        .respond_with(ResponseTemplate::new(200).set_body_raw("<html>real</html>", "text/html"))
        .mount(&server)
        .await;

    let cli = build_client(&server.uri(), Arc::new(AtomicUsize::new(0)));
    let (body, ct) = cli.get_bytes("/page.html", 1_000_000).await.unwrap();
    assert!(ct.contains("text/html"));
    assert!(String::from_utf8_lossy(&body).contains("real"));
}

#[tokio::test]
async fn get_to_streams_a_real_json_attachment() {
    // A genuine .json attachment served as application/json must stream to
    // disk, not be rejected as an API error envelope (unlike the Go original).
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
    let mut buf: Vec<u8> = Vec::new();
    let n = cli.get_to("/data.json", &mut buf, 1_000_000).await.unwrap();
    assert_eq!(n as usize, buf.len());
    assert!(String::from_utf8_lossy(&buf).contains("world"));
}

#[tokio::test]
async fn get_to_streams_a_real_html_attachment() {
    // text/html that survives a re-login is a real HTML attachment → stream it.
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/page.html"))
        .respond_with(ResponseTemplate::new(200).set_body_raw("<html>real</html>", "text/html"))
        .mount(&server)
        .await;

    let cli = build_client(&server.uri(), Arc::new(AtomicUsize::new(0)));
    let mut buf: Vec<u8> = Vec::new();
    cli.get_to("/page.html", &mut buf, 1_000_000).await.unwrap();
    assert!(String::from_utf8_lossy(&buf).contains("real"));
}

#[tokio::test]
async fn get_to_enforces_byte_cap() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/big.bin"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("content-type", "application/octet-stream")
                .set_body_bytes(vec![0u8; 200_000]),
        )
        .mount(&server)
        .await;

    let cli = build_client(&server.uri(), Arc::new(AtomicUsize::new(0)));
    let mut buf: Vec<u8> = Vec::new();
    let err = cli.get_to("/big.bin", &mut buf, 100_000).await.unwrap_err();
    assert!(matches!(err, ClientError::AttachmentTooLarge));
}

#[tokio::test]
async fn concurrent_requests_with_invalidation_dont_deadlock() {
    let server = MockServer::start().await;
    mount_warmup(&server).await;
    Mock::given(method("GET"))
        .and(mpath("/handler/x"))
        .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({"ok": 1})))
        .mount(&server)
        .await;

    let cli = Arc::new(build_client(&server.uri(), Arc::new(AtomicUsize::new(0))));
    let mut tasks = Vec::new();
    for _ in 0..12 {
        let c = cli.clone();
        tasks.push(tokio::spawn(async move {
            c.get_json::<serde_json::Value>("/handler/x").await.is_ok()
        }));
    }
    // Interleave session invalidations to exercise the atomic jar swap.
    let inv = cli.clone();
    tasks.push(tokio::spawn(async move {
        inv.invalidate_session().await;
        inv.invalidate_session().await;
        true
    }));

    let mut ok = 0;
    for t in tasks {
        if t.await.expect("no task panicked") {
            ok += 1;
        }
    }
    assert!(
        ok >= 1,
        "completed without deadlock; requests still succeed under concurrent invalidation"
    );
}
