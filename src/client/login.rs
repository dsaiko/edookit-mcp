//! Plus4U OIDC login driven by a real chromium instance (chromiumoxide).
//! Port of Go's `internal/client/login_chromedp.go`.
//!
//! Edookit federates to Plus4U OIDC, rendered by a uu5loader SPA with
//! reCAPTCHA; the token endpoint needs `client_secret_basic` (so ROPC is
//! closed). The cheapest reliable answer: drive a real chromium once per ~10h,
//! then hand the session cookie to reqwest for all reads.
//!
//! The one quirk: the OIDC client lib hardcodes `prompt=none` (silent SSO),
//! which fails with `interaction_required` for users without an active Plus4U
//! session. A Fetch-domain interceptor strips `prompt=none` from the outgoing
//! auth request when the `client_id` matches this tenant's — but leaves the
//! IdM SPA's nested silent renewal (a different `client_id`) alone.
//!
//! This path is not unit-tested (same as the Go version) — it's exercised by
//! `--login-test` against a live Edookit instance.

use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{Context, anyhow, bail};
use arc_swap::ArcSwap;
use chromiumoxide::cdp::browser_protocol::fetch::{
    ContinueRequestParams, EnableParams as FetchEnableParams, EventRequestPaused, RequestPattern,
};
use chromiumoxide::cdp::browser_protocol::page::{
    EnableParams as PageEnableParams, EventJavascriptDialogOpening, HandleJavaScriptDialogParams,
};
use chromiumoxide::{Browser, BrowserConfig, Page};
use futures::StreamExt;
use tokio::task::JoinHandle;
use url::Url;

use super::LoginCookie;

/// Selector for the "Přihlásit přes Plus4U" button on Edookit's landing page.
/// A `<div class="plus4ULoginButton">` whose onclick triggers Plus4U's OIDC
/// client library to redirect to uuidentity.plus4u.net.
const PLUS4U_BUTTON: &str = ".plus4ULoginButton";
const PLUS4U_HOST: &str = "uuidentity.plus4u.net";

/// Controls how the headless browser performs OIDC login.
pub struct BrowserLoginConfig {
    pub base_url: String,
    pub username: String,
    pub password: String,
    pub headless: bool,
    pub timeout: Duration,
}

/// Aborts a spawned task when dropped — keeps the event-listener loops alive
/// for exactly the lifetime of the login flow.
struct AbortOnDrop(JoinHandle<()>);
impl Drop for AbortOnDrop {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// Drives the full OIDC code flow in a real chromium instance and returns the
/// cookies the target site set in the browser's cookie store. Only cookies
/// matching the base URL's host are returned — Plus4U session cookies are left
/// behind, since reqwest only needs the Edookit PHP session.
pub async fn login_via_browser(cfg: BrowserLoginConfig) -> anyhow::Result<Vec<LoginCookie>> {
    let base = Url::parse(&cfg.base_url).context("parse base url")?;
    let base_host = base
        .host_str()
        .ok_or_else(|| anyhow!("base url has no host"))?
        .to_string();

    // Keep chromium's default sandbox enabled — disabling it would weaken
    // browser isolation on end-user machines.
    let mut builder = BrowserConfig::builder()
        .arg("--disable-blink-features=AutomationControlled")
        .arg("--disable-gpu");
    if !cfg.headless {
        builder = builder.with_head();
    }
    let config = builder
        .build()
        .map_err(|e| anyhow!("browser config: {e}"))?;

    let (browser, mut handler) = Browser::launch(config).await.context("launch chromium")?;
    let handler_task = tokio::spawn(async move { while handler.next().await.is_some() {} });

    let outcome = tokio::time::timeout(cfg.timeout, run_login(&browser, &cfg, &base_host)).await;

    handler_task.abort();
    drop(browser); // closes the child chromium process

    match outcome {
        Ok(res) => res,
        Err(_) => Err(anyhow!("login timed out after {:?}", cfg.timeout)),
    }
}

async fn run_login(
    browser: &Browser,
    cfg: &BrowserLoginConfig,
    base_host: &str,
) -> anyhow::Result<Vec<LoginCookie>> {
    let page = browser
        .new_page("about:blank")
        .await
        .context("open blank page")?;

    // Page domain so the dialog handler receives javascriptDialogOpening; Fetch
    // domain (scoped to the OIDC auth URL) so we can strip prompt=none.
    page.execute(PageEnableParams::default())
        .await
        .context("enable Page domain")?;
    page.execute(FetchEnableParams {
        patterns: Some(vec![RequestPattern {
            url_pattern: Some(format!("https://{PLUS4U_HOST}/*/oidc/auth*")),
            resource_type: None,
            request_stage: None,
        }]),
        handle_auth_requests: None,
    })
    .await
    .context("enable Fetch domain")?;

    // The captured client_id is shared with the fetch interceptor task; it
    // stays empty until the extraction step runs, which is well before the auth
    // request fires (post button-click).
    let client_id = Arc::new(ArcSwap::from_pointee(String::new()));

    let _fetch_guard = spawn_fetch_interceptor(&page, client_id.clone()).await?;
    let _dialog_guard = spawn_dialog_dismisser(&page).await?;

    // 1. Navigate to the landing page.
    tracing::info!("[login] navigate to landing");
    page.goto(cfg.base_url.as_str())
        .await
        .context("navigate to landing")?;

    // 2. Wait for the Plus4U button.
    tracing::info!("[login] wait for Plus4U button");
    wait_visible(&page, PLUS4U_BUTTON, Duration::from_secs(30))
        .await
        .context("wait for Plus4U button")?;

    // 3. Wait for the OIDC client library + environment to populate. uu5loader
    //    can reassign UU5.Environment mid-init, so we check the actual field is
    //    non-empty rather than assuming "lib loaded" implies "config populated".
    tracing::info!("[login] wait for OIDC client library + environment");
    wait_js_true(
        &page,
        "typeof idmLoginClick === 'function' && typeof libraryImportPromise !== 'undefined' \
         && !!(window.UU5 && window.UU5.Environment && window.UU5.Environment.uu_app_oidc_providers_oidcg02_client_id)",
        Duration::from_secs(30),
    )
    .await
    .context("wait for OIDC client library + environment to populate")?;

    // 4. Extract the per-tenant OIDC client_id so the interceptor targets THIS
    //    school's auth request and leaves the IdM SPA's nested silent renewal
    //    (a different client_id) alone.
    let cid: String = page
        .evaluate("window.UU5.Environment.uu_app_oidc_providers_oidcg02_client_id")
        .await
        .context("evaluate client_id")?
        .into_value()
        .context("client_id into_value")?;
    if cid.is_empty() {
        bail!(
            "OIDC client_id empty (UU5.Environment may have been reset between readiness check and read)"
        );
    }
    tracing::info!("[login] OIDC client_id: {cid}");
    client_id.store(Arc::new(cid));

    // 5. Trigger the Plus4U OIDC flow. The lib emits its auth request with
    //    prompt=none for silent SSO — the interceptor strips that. The eval
    //    usually returns an "execution context destroyed" error because
    //    idmLoginClick() starts a navigation that tears down the page context —
    //    that's success, not failure, so we tolerate it; the redirect-wait
    //    below is the real signal.
    tracing::info!("[login] trigger Plus4U login");
    if let Err(e) = page.evaluate("idmLoginClick()").await {
        tracing::debug!(
            "[login] idmLoginClick eval returned (likely navigation tore down the context): {e}"
        );
    }

    // 6. Complete the flow resiliently. After the trigger, two paths are
    //    possible:
    //      (a) no active Plus4U session → the login form is shown → fill it;
    //      (b) an active Plus4U session → silent SSO bounces straight back to
    //          Edookit with NO form shown.
    //    A rigid "wait-for-Plus4U → fill → wait-for-Edookit" sequence hangs on
    //    (b): the fast bounce slips past the host poll. So instead we poll for
    //    the real success signal — the persistent Edookit auth cookie set by the
    //    OIDC callback — and fill the Plus4U form only if/when it appears.
    tracing::info!("[login] completing OIDC flow (filling Plus4U form if shown)");
    let deadline = Instant::now() + Duration::from_secs(75);
    let mut filled = false;
    loop {
        if authenticated(&page, base_host).await {
            break;
        }
        if !filled && fill_plus4u_form_if_present(&page, &cfg.username, &cfg.password).await? {
            filled = true;
            tracing::info!("[login] submitted Plus4U credentials");
        }
        if Instant::now() >= deadline {
            let url = page.url().await.ok().flatten().unwrap_or_default();
            // Surface the cookies we DID see for the host, so a cookie-name
            // mismatch in `authenticated()` is obvious without another round-trip.
            let names: Vec<String> = page
                .get_cookies()
                .await
                .unwrap_or_default()
                .into_iter()
                .filter(|c| host_matches_cookie(base_host, &c.domain))
                .map(|c| c.name)
                .collect();
            bail!(
                "login did not complete within 75s (no Edookit auth cookie; last URL: {url}; cookies seen for host: {names:?})"
            );
        }
        tokio::time::sleep(Duration::from_millis(300)).await;
    }
    tracing::info!("[login] authenticated session detected");

    // 10. Capture cookies for the target host.
    let raw = page.get_cookies().await.context("read cookies")?;
    let mut out = Vec::with_capacity(raw.len());
    for c in raw {
        if !host_matches_cookie(base_host, &c.domain) {
            continue;
        }
        out.push(LoginCookie::new(c.name, c.value));
    }
    if out.is_empty() {
        bail!("browser login: no cookies captured for target host");
    }
    Ok(out)
}

/// Spawns the Fetch interceptor: continues every paused request, stripping
/// `prompt=none` from the outer OIDC auth request whose `client_id` matches the
/// captured one. Returns a guard that aborts the task when dropped.
async fn spawn_fetch_interceptor(
    page: &Page,
    client_id: Arc<ArcSwap<String>>,
) -> anyhow::Result<AbortOnDrop> {
    let mut events = page.event_listener::<EventRequestPaused>().await?;
    let page = page.clone();
    let task = tokio::spawn(async move {
        while let Some(ev) = events.next().await {
            let cid = client_id.load();
            let mut params = ContinueRequestParams::new(ev.request_id.clone());
            let new_url = strip_prompt_none_for_client(&ev.request.url, &cid);
            if let Some(u) = new_url {
                tracing::info!(
                    "[fetch-intercept] stripped prompt=none from outer auth (client={cid})"
                );
                params.url = Some(u);
            }
            if let Err(e) = page.execute(params).await {
                tracing::warn!("[fetch-intercept] continue failed: {e}");
            }
        }
    });
    Ok(AbortOnDrop(task))
}

/// Spawns the dialog dismisser: Plus4U's init code surfaces non-fatal errors
/// via `alert()`, which headless chrome auto-dismisses but headful mode blocks
/// on. Accept any dialog so the flow behaves identically in both modes.
async fn spawn_dialog_dismisser(page: &Page) -> anyhow::Result<AbortOnDrop> {
    let mut events = page
        .event_listener::<EventJavascriptDialogOpening>()
        .await?;
    let page = page.clone();
    let task = tokio::spawn(async move {
        while let Some(ev) = events.next().await {
            tracing::info!(
                "[browser-dialog {:?}] {} (auto-dismissing)",
                ev.r#type,
                ev.message
            );
            let _ = page
                .execute(HandleJavaScriptDialogParams {
                    accept: true,
                    prompt_text: None,
                })
                .await;
        }
    });
    Ok(AbortOnDrop(task))
}

/// Removes `prompt=none` from a URL, but only when the request's `client_id`
/// matches the target. Returns `Some(new_url)` when modified, `None` when left
/// untouched (no match, or no `prompt=none`). The IdM SPA fires its own nested
/// silent renewal with a different `client_id`; that one must keep `prompt=none`
/// or its session-restore path breaks.
fn strip_prompt_none_for_client(raw: &str, client_id: &str) -> Option<String> {
    if client_id.is_empty() {
        return None;
    }
    let mut u = Url::parse(raw).ok()?;
    let prompt_is_none = u.query_pairs().any(|(k, v)| k == "prompt" && v == "none");
    let client_matches = u
        .query_pairs()
        .any(|(k, v)| k == "client_id" && v == client_id);
    if !prompt_is_none || !client_matches {
        return None;
    }
    let kept: Vec<(String, String)> = u
        .query_pairs()
        .filter(|(k, _)| k != "prompt")
        .map(|(k, v)| (k.into_owned(), v.into_owned()))
        .collect();
    u.query_pairs_mut().clear().extend_pairs(kept);
    Some(u.to_string())
}

/// Reports whether a cookie's Domain attribute applies to `host`. Chrome
/// reports domains with or without a leading dot; both are valid.
fn host_matches_cookie(host: &str, cookie_domain: &str) -> bool {
    let d = cookie_domain.strip_prefix('.').unwrap_or(cookie_domain);
    host == d || host.ends_with(&format!(".{d}"))
}

/// Polls a CSS selector until an element matches or the timeout elapses.
async fn wait_visible(page: &Page, selector: &str, timeout: Duration) -> anyhow::Result<()> {
    let deadline = Instant::now() + timeout;
    loop {
        if page.find_element(selector).await.is_ok() {
            return Ok(());
        }
        if Instant::now() >= deadline {
            bail!("timed out waiting for selector {selector}");
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

/// Polls a JavaScript boolean expression until it evaluates to true or times out.
async fn wait_js_true(page: &Page, expr: &str, timeout: Duration) -> anyhow::Result<()> {
    let deadline = Instant::now() + timeout;
    loop {
        if let Ok(result) = page.evaluate(expr).await
            && result.into_value::<bool>().unwrap_or(false)
        {
            return Ok(());
        }
        if Instant::now() >= deadline {
            bail!("timed out waiting for JS condition: {expr}");
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

/// True once the persistent Edookit auth cookie (set by the OIDC callback) is
/// present for the target host — the real "login complete" signal, regardless
/// of whether the Plus4U form was shown or silent SSO bounced straight back.
async fn authenticated(page: &Page, base_host: &str) -> bool {
    let Ok(cookies) = page.get_cookies().await else {
        return false;
    };
    cookies.iter().any(|c| {
        (c.name == "X-EdooAuthToken" || c.name == "X-Auth-Id")
            && host_matches_cookie(base_host, &c.domain)
    })
}

/// Fills + submits the Plus4U login form if its username field is currently
/// visible. Returns true if it submitted, false if the form wasn't there (e.g.
/// silent SSO already bounced us back). Submitting navigates away, so the
/// submit eval's "context destroyed" error is tolerated.
async fn fill_plus4u_form_if_present(
    page: &Page,
    username: &str,
    password: &str,
) -> anyhow::Result<bool> {
    let on_plus4u = page
        .url()
        .await
        .ok()
        .flatten()
        .and_then(|u| Url::parse(&u).ok())
        .and_then(|u| u.host_str().map(str::to_string))
        .is_some_and(|h| h == PLUS4U_HOST);
    if !on_plus4u {
        return Ok(false);
    }
    let Ok(user_el) = page.find_element(r#"input[autocomplete="username"]"#).await else {
        return Ok(false);
    };
    user_el.click().await?.type_str(username).await?;
    if let Ok(pass_el) = page
        .find_element(r#"input[autocomplete="current-password"]"#)
        .await
    {
        pass_el.click().await?.type_str(password).await?;
    }
    let _ = page
        .evaluate(
            r#"document.querySelector('input[autocomplete="current-password"]').form.submit()"#,
        )
        .await;
    Ok(true)
}

/// Launches chromium against `base_url`, waits for the page to render, and
/// returns the outer HTML of `<body>`. Selector-debugging aid.
pub async fn dump_landing_html(base_url: &str, headless: bool) -> anyhow::Result<String> {
    let mut builder = BrowserConfig::builder()
        .arg("--disable-blink-features=AutomationControlled")
        .arg("--disable-gpu");
    if !headless {
        builder = builder.with_head();
    }
    let config = builder
        .build()
        .map_err(|e| anyhow!("browser config: {e}"))?;
    let (browser, mut handler) = Browser::launch(config).await.context("launch chromium")?;
    let handler_task = tokio::spawn(async move { while handler.next().await.is_some() {} });

    let outcome = tokio::time::timeout(Duration::from_secs(30), async {
        let page = browser.new_page(base_url).await.context("navigate")?;
        // Give the SPA a moment to render after the document is ready.
        tokio::time::sleep(Duration::from_secs(3)).await;
        let html: String = page
            .evaluate("document.body.outerHTML")
            .await
            .context("read body outerHTML")?
            .into_value()
            .context("outerHTML into_value")?;
        anyhow::Ok(html)
    })
    .await;

    handler_task.abort();
    drop(browser);
    match outcome {
        Ok(res) => res,
        Err(_) => Err(anyhow!("dump landing html timed out")),
    }
}
