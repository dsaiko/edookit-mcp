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
use chromiumoxide::{Browser, BrowserConfig, Element, Page};
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
    let mut form = Plus4uFormState::default();
    loop {
        if authenticated(&page, base_host).await {
            break;
        }
        fill_plus4u_form_if_present(&page, &cfg.username, &cfg.password, &mut form).await?;
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

/// Per-step bookkeeping for the Plus4U gate. The poll loop calls
/// [`fill_plus4u_form_if_present`] every 300 ms, so this decides when a step is
/// still worth attempting.
///
/// The budget counts **submissions** — an Enter that actually went to the gate
/// — not DOM work. Filling a field is idempotent ([`type_into`] compares
/// against the live `value`) and submits nothing, so a re-render race may retry
/// it freely until the loop's 75 s deadline; what must stay bounded is the
/// number of times credentials are posted, because a few hundred failed
/// submissions would risk locking the account. A step that never lands ends in
/// that deadline, which reports the stuck URL.
#[derive(Debug, Default)]
struct Plus4uFormState {
    username: StepState,
    password: StepState,
    /// Set once a screen carrying the e-mail field **without** a password field
    /// has been seen — i.e. the gate is the two-step kind. See
    /// [`classify_step`] for why this, and not the submission history, is what
    /// tells the two form shapes apart.
    saw_email_only: bool,
}

#[derive(Debug, Default)]
struct StepState {
    /// The field was filled *and* Enter was accepted.
    submitted: bool,
    /// Enter presses actually issued for this step.
    submissions: u8,
}

/// Credential submissions per gate step before we stop touching it. Small on
/// purpose — see [`Plus4uFormState`].
const MAX_STEP_SUBMISSIONS: u8 = 3;

impl StepState {
    /// Whether this step should still be looked for on the page.
    fn pending(&self) -> bool {
        !self.submitted && self.submissions < MAX_STEP_SUBMISSIONS
    }

    fn record_submission(&mut self) {
        self.submissions = self.submissions.saturating_add(1);
    }
}

/// Drives whichever step of the Plus4U login gate is currently on screen.
///
/// The gate (uuIdentitymanagement g01 — "our new login gate", seen from 1.47.5)
/// is a **two-step SPA**: step 1 carries only the e-mail field (and renders no
/// `<form>` at all), step 2 adds the password field while keeping step 1's
/// (now blank) field in the DOM — hence the per-step bookkeeping rather than a
/// "both fields visible" assumption. Its submit buttons are
/// `<button type="button">` with generated class names, and step 2's stays
/// `disabled` until the field is non-empty — so each step is advanced with a
/// synthetic Enter keypress on the field itself rather than `form.submit()` or
/// a brittle button selector.
///
/// Called from the poll loop, so step two is picked up on a later iteration. A
/// step is marked done only once its field actually accepted the text: a
/// click/type that races the SPA's re-render leaves it un-done and is retried on
/// the next poll, while the Enter keypress navigates away, so *its* "context
/// destroyed" error is tolerated.
async fn fill_plus4u_form_if_present(
    page: &Page,
    username: &str,
    password: &str,
    state: &mut Plus4uFormState,
) -> anyhow::Result<()> {
    let on_plus4u = page
        .url()
        .await
        .ok()
        .flatten()
        .and_then(|u| Url::parse(&u).ok())
        .and_then(|u| u.host_str().map(str::to_string))
        .is_some_and(|h| h == PLUS4U_HOST);
    if !on_plus4u {
        return Ok(());
    }

    // What is on screen right now, skipping steps that are done or out of
    // attempts. The password is looked up by `type=password` because the gate
    // marks it `autocomplete="off"` (NOT `current-password`) and its
    // `name`/`id` are ambiguous/generated (step 2 also carries a hidden
    // `input[name=username]`).
    let pass_el = if state.password.pending() {
        page.find_element(r#"input[type="password"]"#).await.ok()
    } else {
        None
    };
    let user_el = if state.username.pending() {
        page.find_element(r#"input[autocomplete="username"]"#)
            .await
            .ok()
    } else {
        None
    };

    match classify_step(user_el, pass_el, state.saw_email_only) {
        // Legacy single-page form — the shape Plus4U used before the 2026 gate,
        // and whatever a future variant may show both fields on. Fill username
        // *then* password and submit once: submitting the password first would
        // post an empty username, and the failed attempt may clear the field.
        Step::Both(user, pass) => {
            // Fill both first: a DOM race here submits nothing, so it must not
            // consume the submission budget of either field.
            if !type_into(&user, username).await || !type_into(&pass, password).await {
                return Ok(());
            }
            // One Enter posts both fields, so it costs both budgets — but only
            // if the keypress was actually issued.
            match press_enter(&pass).await {
                Submit::NotFocused => return Ok(()),
                outcome => {
                    state.username.record_submission();
                    state.password.record_submission();
                    if outcome == Submit::Sent {
                        state.username.submitted = true;
                        state.password.submitted = true;
                        tracing::info!("[login] Plus4U single-page form submitted");
                    }
                }
            }
        }
        Step::Password(pass) => {
            // We are past step 1 whatever its keypress reported.
            state.username.submitted = true;
            if !type_into(&pass, password).await {
                return Ok(());
            }
            match press_enter(&pass).await {
                Submit::NotFocused => return Ok(()),
                outcome => {
                    state.password.record_submission();
                    if outcome == Submit::Sent {
                        state.password.submitted = true;
                        tracing::info!("[login] Plus4U step 2/2: password submitted");
                    }
                }
            }
        }
        Step::Email(user) => {
            // An e-mail-only screen identifies the two-step gate. Recorded on
            // sight, before any attempt, so a DOM race cannot lose it.
            state.saw_email_only = true;
            if !type_into(&user, username).await {
                return Ok(());
            }
            match press_enter(&user).await {
                Submit::NotFocused => return Ok(()),
                outcome => {
                    state.username.record_submission();
                    if outcome == Submit::Sent {
                        state.username.submitted = true;
                        tracing::info!("[login] Plus4U step 1/2: e-mail submitted");
                    }
                }
            }
        }
        Step::Idle => {}
    }

    Ok(())
}

/// Which gate step the fields currently on screen represent.
#[derive(Debug, PartialEq, Eq)]
enum Step<T> {
    /// Legacy single-page form: both fields, filled together and submitted once.
    Both(T, T),
    /// New gate, step 2 of 2 — password only. Any leftover e-mail input is
    /// carried by the page but deliberately not part of this variant.
    Password(T),
    /// New gate, step 1 of 2 — e-mail only.
    Email(T),
    /// Nothing actionable this round (silent SSO, or mid-render).
    Idle,
}

/// Decides which step is on screen. Generic over the element type so the truth
/// table can be unit-tested without a browser.
///
/// A username **and** a password input are in the DOM on both shapes — the
/// legacy single-page form has them together, and the new gate's step 2 keeps
/// step 1's (blank) e-mail field — so `(Some, Some)` alone is ambiguous.
///
/// `saw_email_only` resolves it, and it is an observation of the *page*: the
/// two-step gate always shows an e-mail-only screen on the way in, the
/// single-page form never does. Submission history cannot stand in for it —
/// keying off "we already posted a username" misreads the legacy form the
/// moment one of its attempts reports [`Submit::Uncertain`]: the next poll
/// would call the same page step 2, stop maintaining the username field, and
/// (if the failed attempt cleared it) post the password alone until the
/// deadline.
fn classify_step<T>(user: Option<T>, pass: Option<T>, saw_email_only: bool) -> Step<T> {
    match (user, pass) {
        (Some(user), Some(pass)) if !saw_email_only => Step::Both(user, pass),
        (_, Some(pass)) => Step::Password(pass),
        (Some(user), None) => Step::Email(user),
        (None, None) => Step::Idle,
    }
}

/// What became of an attempt to submit a field with Enter.
#[derive(Debug, PartialEq, Eq)]
enum Submit {
    /// Focus and keypress both accepted — the credentials went to the gate.
    Sent,
    /// Focus failed, so no keypress was issued: nothing left the client and the
    /// submission budget must not be charged.
    NotFocused,
    /// Focused, but the keypress errored. It may still have landed (a
    /// successful Enter navigates and the call can fail against the torn-down
    /// context), so this *is* charged — we cannot claim nothing was posted.
    Uncertain,
}

/// Focuses the field and submits it with Enter.
///
/// The explicit `focus()` is load-bearing: chromiumoxide's `press_key` is a
/// *page*-level keypress (it forwards to the tab, not the node — its own docs
/// reach the field via a preceding `click()`), so without it the Enter lands
/// wherever focus happens to be — after a re-render, or when [`type_into`]
/// short-circuited on an already-correct value and therefore never clicked —
/// and would still report `Ok`.
async fn press_enter(el: &Element) -> Submit {
    if el.focus().await.is_err() {
        return Submit::NotFocused;
    }
    if el.press_key("Enter").await.is_ok() {
        Submit::Sent
    } else {
        Submit::Uncertain
    }
}

/// Clicks `el` and types `text` into it. False if the SPA re-rendered under us,
/// in which case the caller leaves the step un-done and retries next poll.
///
/// **Idempotent against the live DOM**, which is what makes that retry safe: a
/// poll that filled the username and then failed on the password leaves both
/// step flags false, so the next round re-visits the username field. Comparing
/// against (and, if it differs, clearing) the field's actual `value` means the
/// retry cannot append a second copy of the e-mail — and unlike a "typed
/// already" flag, it still refills a field the SPA wiped in between.
async fn type_into(el: &Element, text: &str) -> bool {
    match el.string_property("value").await {
        // Already exactly what we want — nothing to do.
        Ok(Some(current)) if current == text => return true,
        // Some other content (a partial type, or a value the gate prefilled):
        // clear it first. Assigning `value` alone leaves a controlled input's
        // internal state stale, so fire the events its framework listens for.
        Ok(Some(current)) if !current.is_empty() => {
            let cleared = el
                .call_js_fn(
                    "function(){ this.value=''; \
                     this.dispatchEvent(new Event('input',{bubbles:true})); \
                     this.dispatchEvent(new Event('change',{bubbles:true})); }",
                    false,
                )
                .await;
            if cleared.is_err() {
                return false;
            }
        }
        // Empty, absent, or unreadable (element detached) — just type.
        Ok(_) => {}
        Err(_) => return false,
    }
    if el.click().await.is_err() || el.type_str(text).await.is_err() {
        return false;
    }
    // Confirm the field actually holds what we typed: a controlled input can
    // silently reject or rewrite the value, and submitting a half-typed
    // credential would just burn one of the step's attempts blind.
    matches!(el.string_property("value").await, Ok(Some(v)) if v == text)
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn step_classification_truth_table() {
        // Both fields, no e-mail-only screen ever seen: the single-page form.
        assert_eq!(
            classify_step(Some("u"), Some("p"), false),
            Step::Both("u", "p")
        );
        // Both fields *after* an e-mail-only screen: the new gate's step 2, so
        // the leftover e-mail input is ignored rather than re-posted.
        assert_eq!(
            classify_step(Some("u"), Some("p"), true),
            Step::Password("p")
        );
        // A password field alone is a password step either way.
        assert_eq!(classify_step(None, Some("p"), false), Step::Password("p"));
        // An e-mail field alone is step 1 of the two-step gate.
        assert_eq!(classify_step(Some("u"), None, false), Step::Email("u"));
        assert_eq!(classify_step::<&str>(None, None, false), Step::Idle);
    }

    /// The legacy single-page form must keep being treated as one across
    /// retries. An attempt that reports [`Submit::Uncertain`] bumps the
    /// submission counters, which must NOT re-label the page as the new gate's
    /// step 2 — that would abandon the username field, and a form that cleared
    /// it on the failed attempt would then only ever receive the password.
    #[test]
    fn legacy_form_survives_an_uncertain_attempt() {
        let mut state = Plus4uFormState::default();

        assert_eq!(
            classify_step(Some("u"), Some("p"), state.saw_email_only),
            Step::Both("u", "p")
        );

        // Enter came back Uncertain: both budgets charged, neither step marked
        // submitted — and no e-mail-only screen was ever observed.
        state.username.record_submission();
        state.password.record_submission();
        assert!(state.username.pending() && state.password.pending());

        assert_eq!(
            classify_step(Some("u"), Some("p"), state.saw_email_only),
            Step::Both("u", "p"),
            "submission history must not re-label the legacy form"
        );
    }

    /// The mirror case: once the two-step gate has shown its e-mail-only
    /// screen, a page with both inputs is step 2 from then on.
    #[test]
    fn two_step_gate_is_remembered_from_its_first_screen() {
        let mut state = Plus4uFormState::default();
        assert_eq!(
            classify_step(Some("u"), None, state.saw_email_only),
            Step::Email("u")
        );
        state.saw_email_only = true; // what the Email arm records on sight
        assert_eq!(
            classify_step(Some("u"), Some("p"), state.saw_email_only),
            Step::Password("p")
        );
    }

    #[test]
    fn submission_budget_gates_the_step() {
        let mut step = StepState::default();
        assert!(step.pending(), "a fresh step is attemptable");

        // Only issued keypresses count, and the budget is hard.
        for _ in 0..MAX_STEP_SUBMISSIONS {
            assert!(step.pending());
            step.record_submission();
        }
        assert!(
            !step.pending(),
            "credentials are never posted more than {MAX_STEP_SUBMISSIONS} times"
        );

        // A step that succeeded is left alone even with budget to spare.
        let mut done = StepState::default();
        done.record_submission();
        done.submitted = true;
        assert!(!done.pending());
    }
}
