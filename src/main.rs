mod client;
mod http;
mod server;
mod tools;

use std::process::ExitCode;
use std::sync::Arc;

use anyhow::{anyhow, bail};
use clap::Parser;
use rmcp::ServiceExt;
use rmcp::transport::stdio;

use crate::client::{Client, Config};
use crate::server::{BuildInfo, EdookitServer};

const VERSION: &str = env!("EDOOKIT_VERSION");
const COMMIT: &str = env!("EDOOKIT_COMMIT");
const BUILD_DATE: &str = env!("EDOOKIT_BUILD_DATE");

/// Unofficial MCP connector for the Edookit school information system.
#[derive(Parser, Debug)]
#[command(name = "edookit-mcp", version = VERSION, about)]
struct Cli {
    /// Perform the OIDC login once and exit (smoke test).
    #[arg(long)]
    login_test: bool,
    /// Navigate to EDOOKIT_URL, dump body HTML, exit (selector debugging).
    #[arg(long)]
    dump_html: bool,
    /// Delete the cached session cookies and exit.
    #[arg(long)]
    clear_cookies: bool,
    /// List a few inbox + sent messages and exit (smoke test for the tools).
    #[arg(long)]
    test_messages: bool,
    /// (dev) Fetch the full body of the given message ID and print FullMessage JSON.
    #[arg(long, value_name = "ID")]
    get_message: Option<String>,
    /// (dev) Dump raw JSON of full-message endpoints for one ID (reverse-engineering aid).
    #[arg(long, value_name = "ID")]
    dump_message: Option<String>,
    /// Run as a remote MCP server over Streamable HTTP on this address (endpoint /mcp).
    /// When unset, runs as a local stdio MCP server. Falls back to EDOOKIT_HTTP_ADDR.
    #[arg(long, value_name = "ADDR")]
    http: Option<String>,
    /// (dev) Render the experimental MCP Apps inbox template (with a built-in
    /// mock host feeding sample data) to stdout and exit — no login. Pipe to a
    /// file and open in a browser: `edookit-mcp --preview-ui > /tmp/inbox.html`.
    #[arg(long)]
    preview_ui: bool,
}

#[tokio::main]
async fn main() -> ExitCode {
    init_tracing();
    let cli = Cli::parse();
    match run(cli).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("error: {e:#}");
            ExitCode::FAILURE
        }
    }
}

async fn run(cli: Cli) -> anyhow::Result<()> {
    // One-shot runners that don't need the full Edookit client.
    if cli.preview_ui {
        print!("{}", tools::ui::render_preview_html());
        return Ok(());
    }
    if cli.clear_cookies {
        return run_clear_cookies();
    }
    if cli.dump_html {
        let html = client::login::dump_landing_html(
            &getenv_required("EDOOKIT_URL")?,
            getenv_bool("EDOOKIT_HEADLESS_LOGIN", true)?,
        )
        .await?;
        print!("{html}");
        return Ok(());
    }

    let cli_client = build_client_from_env()?;

    if cli.login_test {
        return run_login_test(&cli_client).await;
    }
    if cli.test_messages {
        return run_test_messages(&cli_client).await;
    }
    if let Some(id) = &cli.get_message {
        return run_get_message(&cli_client, id).await;
    }
    if let Some(id) = &cli.dump_message {
        return run_dump_message(&cli_client, id).await;
    }

    let http_addr = cli.http.clone().or_else(|| {
        std::env::var("EDOOKIT_HTTP_ADDR")
            .ok()
            .filter(|s| !s.is_empty())
    });

    // Experimental: expose the MCP Apps inbox UI (on by default; set
    // EDOOKIT_UI_RESOURCES=false to suppress it).
    let ui_resources = getenv_bool("EDOOKIT_UI_RESOURCES", true)?;

    let server = EdookitServer::new(
        Arc::new(cli_client),
        BuildInfo {
            version: VERSION.to_string(),
            commit: COMMIT.to_string(),
            build_time: BUILD_DATE.to_string(),
        },
        ui_resources,
    );

    if let Some(addr) = http_addr {
        return http::run_http(server, addr).await;
    }
    serve_stdio(server).await
}

/// Serves the MCP server over stdio until the client disconnects.
async fn serve_stdio(server: EdookitServer) -> anyhow::Result<()> {
    let service = server
        .serve(stdio())
        .await
        .map_err(|e| anyhow!("serve stdio: {e}"))?;
    service
        .waiting()
        .await
        .map_err(|e| anyhow!("stdio transport: {e}"))?;
    Ok(())
}

// --- env wiring ---

fn build_client_from_env() -> anyhow::Result<Client> {
    let mut cfg = Config::new(
        getenv_required("EDOOKIT_URL")?,
        getenv_required("EDOOKIT_USER")?,
        getenv_required("EDOOKIT_PASS")?,
    );
    cfg.allow_insecure_http = getenv_bool("EDOOKIT_ALLOW_INSECURE_HTTP", false)?;
    cfg.headless_login = getenv_bool("EDOOKIT_HEADLESS_LOGIN", true)?;
    cfg.cookie_cache_path = cookie_cache_path()?;
    cfg.timezone = Some(load_timezone()?);
    cfg.download_hosts = download_hosts();
    Client::new(cfg)
}

/// Resolves EDOOKIT_DOWNLOAD_HOSTS — the hosts an attachment download may be
/// redirected to besides the base origin (Edookit serves uploaded files from
/// `dataN.edookit.net`). Comma- or whitespace-separated; exact hostnames or
/// `*.suffix` wildcards. Unset → the built-in default; set but empty → strict
/// same-origin (no redirect off the tenant host is tolerated).
fn download_hosts() -> Vec<String> {
    let Ok(raw) = std::env::var("EDOOKIT_DOWNLOAD_HOSTS") else {
        return client::DEFAULT_DOWNLOAD_HOSTS
            .iter()
            .map(|s| (*s).to_string())
            .collect();
    };
    raw.split([',', ' ', '\t'])
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
        .collect()
}

/// EDOOKIT_NO_COOKIE_CACHE=true disables caching; EDOOKIT_COOKIE_CACHE overrides
/// the path; otherwise the default per-user cache location.
fn cookie_cache_path() -> anyhow::Result<Option<std::path::PathBuf>> {
    if getenv_bool("EDOOKIT_NO_COOKIE_CACHE", false)? {
        return Ok(None);
    }
    if let Ok(v) = std::env::var("EDOOKIT_COOKIE_CACHE")
        && !v.is_empty()
    {
        return Ok(Some(std::path::PathBuf::from(v)));
    }
    match client::default_cookie_cache_path() {
        Ok(p) => Ok(Some(p)),
        Err(e) => {
            tracing::warn!(
                "cannot determine default cookie cache path ({e}); persistence disabled"
            );
            Ok(None)
        }
    }
}

/// Resolves EDOOKIT_TIMEZONE (default Europe/Prague) into a jiff TimeZone.
fn load_timezone() -> anyhow::Result<jiff::tz::TimeZone> {
    let name = std::env::var("EDOOKIT_TIMEZONE")
        .ok()
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| "Europe/Prague".to_string());
    jiff::tz::TimeZone::get(&name).map_err(|e| anyhow!("invalid EDOOKIT_TIMEZONE: {e}"))
}

pub(crate) fn getenv_required(key: &str) -> anyhow::Result<String> {
    match std::env::var(key) {
        Ok(v) if !v.is_empty() => Ok(v),
        _ => bail!("required env var {key} is not set"),
    }
}

pub(crate) fn getenv_bool(key: &str, default: bool) -> anyhow::Result<bool> {
    match std::env::var(key) {
        Ok(v) if !v.is_empty() => {
            parse_env_bool(&v).ok_or_else(|| anyhow!("env var {key} is not a valid bool: {v}"))
        }
        _ => Ok(default),
    }
}

/// Accepts the same forms as Go's strconv.ParseBool (the subset we document).
fn parse_env_bool(v: &str) -> Option<bool> {
    match v.to_ascii_lowercase().as_str() {
        "1" | "t" | "true" => Some(true),
        "0" | "f" | "false" => Some(false),
        _ => None,
    }
}

// --- dev one-shot runners ---

fn run_clear_cookies() -> anyhow::Result<()> {
    match cookie_cache_path()? {
        None => {
            eprintln!("cookie cache is disabled — nothing to clear");
            Ok(())
        }
        Some(path) => match std::fs::remove_file(&path) {
            Ok(()) => {
                eprintln!("removed {}", path.display());
                Ok(())
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                eprintln!("no cached cookies at {}", path.display());
                Ok(())
            }
            Err(e) => Err(anyhow!("remove {}: {e}", path.display())),
        },
    }
}

async fn run_login_test(cli: &Client) -> anyhow::Result<()> {
    eprintln!("ensuring login session (chromium launches only if cache is cold)...");
    cli.ensure_logged_in().await?;
    eprintln!(
        "session ready — {} cookie(s) available for target host",
        cli.session_cookies().len()
    );

    if cli.uses_handler_api().await? {
        let probe: serde_json::Value = cli.get_json("/handler/page/dashboard").await?;
        if probe.get("authenticated").and_then(|v| v.as_bool()) != Some(true) {
            bail!(
                "/handler/page/dashboard returned authenticated=false despite successful login — warmup is broken"
            );
        }
        eprintln!("authenticated session verified via /handler/page/dashboard");
    } else {
        let html = cli
            .get_text("/overview/updates")
            .await
            .map_err(|e| anyhow!("overview session probe failed: {e}"))?;
        if !html.contains("inboxMessage") {
            bail!(
                "/overview/updates loaded but looks unlike an inbox page — UI may have changed"
            );
        }
        eprintln!("authenticated session verified via /overview/updates (overview UI)");
    }
    Ok(())
}

async fn run_test_messages(cli: &Client) -> anyhow::Result<()> {
    use tools::messages::{InboxOptions, SentOptions, list_inbox, list_sent};

    eprintln!("=== INBOX (3 most recent) ===");
    let inbox = list_inbox(
        cli,
        InboxOptions {
            limit: 3,
            ..Default::default()
        },
    )
    .await?;
    for m in &inbox.messages {
        eprintln!(
            "  [{}] {} | {} | {:?} | attachments={}",
            m.id, m.date, m.sender, m.subject, m.attachments
        );
    }
    for w in &inbox.parse_warnings {
        eprintln!("  [parse-warning] {w}");
    }

    eprintln!("=== SENT (3 most recent) ===");
    let sent = list_sent(
        cli,
        SentOptions {
            limit: 3,
            ..Default::default()
        },
    )
    .await?;
    for m in &sent.messages {
        eprintln!(
            "  [{}] {} | {} | {:?} | attachments={}",
            m.id, m.date, m.status, m.subject, m.attachments
        );
    }

    eprintln!("=== INBOX UNREAD ===");
    let unread = list_inbox(
        cli,
        InboxOptions {
            view: "unread".to_string(),
            limit: 5,
            ..Default::default()
        },
    )
    .await?;
    eprintln!("{} unread message(s)", unread.messages.len());
    for m in &unread.messages {
        eprintln!("  [{}] {} | {} | {:?}", m.id, m.date, m.sender, m.subject);
    }
    Ok(())
}

async fn run_get_message(cli: &Client, id: &str) -> anyhow::Result<()> {
    let msg = tools::message::get_message(cli, id).await?;
    println!("{}", serde_json::to_string_pretty(&msg)?);
    Ok(())
}

/// Probes the plausible full-message endpoint patterns and prints whichever
/// return JSON — used to reverse-engineer the API shape.
async fn run_dump_message(cli: &Client, id_arg: &str) -> anyhow::Result<()> {
    let id = id_arg.trim().trim_start_matches("m-");
    if id.is_empty() {
        bail!("--dump-message: empty ID (use m-NNNNNN or NNNNNN)");
    }
    let paths = [
        format!("/handler/page/message-edit?__index={id}"),
        format!("/handler/page/message-view?__index={id}"),
        format!("/handler/page/message?__index={id}"),
        format!("/handler/window/message-edit?__index={id}"),
        format!("/handler/page/mail-edit?__index={id}"),
        format!("/handler/page/object-view?__index={id}"),
    ];
    let mut successes = 0;
    for p in &paths {
        eprintln!("[probe] GET {p} ...");
        match cli.get_json::<serde_json::Value>(p).await {
            Ok(resp) => {
                eprintln!("  -> OK");
                println!("\n===== response for {p} =====");
                println!("{}", serde_json::to_string_pretty(&resp)?);
                successes += 1;
            }
            Err(e) => eprintln!("  -> ERR: {e}"),
        }
    }
    if successes == 0 {
        bail!(
            "all {} candidate endpoints failed — Edookit URL scheme may have moved",
            paths.len()
        );
    }
    eprintln!(
        "done — {}/{} endpoints returned JSON",
        successes,
        paths.len()
    );
    Ok(())
}

fn init_tracing() {
    use tracing_subscriber::{EnvFilter, fmt};
    // chromiumoxide logs a WARN for every CDP event it can't deserialize from a
    // newer Chrome ("WS Invalid message…") — benign noise; quiet it by default.
    let filter = EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| EnvFilter::new("info,chromiumoxide=error,tungstenite=error"));
    // Logs MUST go to stderr — stdout is the stdio MCP protocol channel.
    fmt()
        .with_writer(std::io::stderr)
        .with_env_filter(filter)
        .with_target(false)
        .init();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_env_bool_forms() {
        for t in ["1", "t", "true", "TRUE", "True"] {
            assert_eq!(parse_env_bool(t), Some(true), "{t}");
        }
        for f in ["0", "f", "false", "FALSE"] {
            assert_eq!(parse_env_bool(f), Some(false), "{f}");
        }
        assert_eq!(parse_env_bool("yes"), None);
        assert_eq!(parse_env_bool("1.5"), None);
    }

    #[test]
    fn build_info_round_trips() {
        let info = BuildInfo {
            version: "1.2.3".into(),
            commit: "abc123".into(),
            build_time: "2026-06-01".into(),
        };
        let j = serde_json::to_value(&info).unwrap();
        assert_eq!(j["version"], "1.2.3");
        assert_eq!(j["commit"], "abc123");
        assert_eq!(j["build_time"], "2026-06-01");
    }
}
