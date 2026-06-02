# edookit-mcp-rs

Neoficiální MCP konektor pro Edookit — umožňuje AI asistentům (Claude, ChatGPT,
Cursor, VS Code Copilot a dalším MCP-kompatibilním klientům) číst zprávy z
žákovské knížky.

> **Rust port.** Toto je přepis [`edookit-mcp`](https://github.com/dsaiko/edookit-mcp) (původně v Go) do
> Rustu, vytvořený pro **srovnání obou implementací**. Chování, sada nástrojů i
> bezpečnostní model jsou stejné jako u Go verze. Upřímné srovnání Go vs Rust je
> na konci tohoto souboru. **Neoficiální projekt — nemá nic společného s Edookit
> s.r.o.**

---

## 🇨🇿 Pro uživatele

### Co to je

`edookit-mcp` propojuje vašeho AI asistenta s vaším účtem v
[Edookitu](https://edookit.com) přes [MCP](https://modelcontextprotocol.io/). Po
napojení se můžete ptát přirozeným jazykem:

- *„Mám nějaké nové zprávy?"*
- *„Ukaž mi zprávy od ředitele za poslední týden."*
- *„Co mi přišlo s přílohou?"*

Asistent zavolá konektor, stáhne aktuální data přímo z Edookitu a odpoví českým
souvislým textem.

### K čemu to neslouží

- Nepřepisuje zprávy ani neposílá za vás (zatím jen čte).
- Nesleduje vás na pozadí — spouští se jen, když ho asistent vyvolá.
- Neukládá data do cloudu — vše zůstává na vašem počítači. (Tělo zpráv ovšem
  prochází přes poskytovatele vašeho AI asistenta — viz [Bezpečnost](#bezpečnost-a-soukromí).)

### Co je potřeba

1. **macOS / Linux / Windows** s nainstalovaným Chromem (Chromium, Brave, Edge — cokoli na Chromium jádře).
2. **Účet v Edookitu** přihlašovaný přes Plus4U.
3. **AI klient s podporou MCP** (Claude Desktop/Code, ChatGPT, Cursor, VS Code Copilot, Zed, Continue.dev…).
4. **Rust 1.88+ (edition 2024)** pro sestavení ze zdrojáků.

### Instalace

**Homebrew (macOS / Linux) — nejjednodušší:**

```bash
brew install dsaiko/tap/edookit-mcp-rs
```

Nainstaluje se příkaz `edookit-mcp-rs` (i s přibalenou knihovnou PDFium). Tap
[`dsaiko/homebrew-tap`](https://github.com/dsaiko/homebrew-tap) hostí i Go verzi
jako `edookit-mcp` — obě mohou být nainstalované zároveň.

**Ze zdrojáků:**

```bash
git clone git@github.com:dsaiko/edookit-mcp-rs.git
cd edookit-mcp-rs
make build          # stáhne přibalený PDFium a sestaví bin do target/release/
```

`make build` je jazykově neutrální — nemusíte řešit `cargo`. Binárku najdete v
`target/release/edookit-mcp`. (Předkompilované binárky pro každý release jsou na
[GitHub Releases](https://github.com/dsaiko/edookit-mcp-rs/releases) — archiv
obsahuje binárku i přibalenou knihovnu PDFium.)

> **PDFium:** rasterizace PDF příloh používá nativní knihovnu PDFium, kterou
> `make build` stáhne do `third_party/pdfium/`. Při běhu z repozitáře se najde
> automaticky; přesunutou/nainstalovanou binárku nasměrujte přes
> `EDOOKIT_PDFIUM_LIB` nebo položte `libpdfium.*` vedle ní.

### Konfigurace

```bash
cp .env.example .env
chmod 600 .env       # ať heslo není čitelné pro ostatní uživatele systému
$EDITOR .env
```

```env
EDOOKIT_URL=https://your-school-login.edookit.net
EDOOKIT_USER=vase.jmeno@example.cz
EDOOKIT_PASS=vase-heslo
```

URL je specifická pro vaši školu (najdete ji v adresním řádku po přihlášení).

### První spuštění (ověření)

```bash
make smoke-login        # ověří přihlášení (Chrome se na pár vteřin otevře)
make test-messages      # vytiskne pár posledních zpráv ze schránky
```

Cookies se uloží do uživatelské cache (na macOS
`~/Library/Caches/edookit-mcp-rs/cookies.json`) a další spuštění už Chrome
neotevírá (~10 h). Vynucené nové přihlášení: `make clear-cookies`. Debug s
viditelným prohlížečem: `EDOOKIT_HEADLESS_LOGIN=false`.

### Připojení k AI asistentovi

`edookit-mcp` je standardní MCP server přes stdio. Společná JSON konfigurace
(Anthropic / Claude formát, který přejala většina ekosystému):

```json
{
  "mcpServers": {
    "edookit": {
      "command": "/absolutní/cesta/k/edookit-mcp-rs/target/release/edookit-mcp",
      "env": {
        "EDOOKIT_URL": "https://your-school-login.edookit.net",
        "EDOOKIT_USER": "vase.jmeno@example.cz",
        "EDOOKIT_PASS": "vase-heslo"
      }
    }
  }
}
```

Při instalaci přes Homebrew je `command` jen `"edookit-mcp-rs"` (je na PATH);
absolutní cesta k `target/release/edookit-mcp` platí pro build ze zdrojáků.

Liší se hlavně **kam ji vložit**: Claude Code `~/.claude.json`; Claude Desktop
`~/Library/Application Support/Claude/claude_desktop_config.json`; Cursor
`~/.cursor/mcp.json`; VS Code Copilot `<workspace>/.vscode/mcp.json` (jiný shape
— klíč `servers`). Po každé změně klienta restartujte.

> **Tip: heslo mimo config (Keychain).** Wrapper skript načte heslo z OS secret
> store až při startu — viz [`scripts/edookit-mcp-wrapper.sh.example`](scripts/edookit-mcp-wrapper.sh.example).
> V `command` ukážete na skript a `env` blok vynecháte.

### Vzdálené nasazení (Streamable HTTP)

Pro běžné lokální použití zůstává **stdio** výchozí. Pro vystavení jako
**vzdálený konektor přes HTTP** (typicky za TLS reverse-proxy, např. pro ChatGPT)
spusťte s `--http <addr>`. Autentizaci řeší **vestavěný OAuth 2.1 Authorization
Server** (Dynamic Client Registration, PKCE S256, HMAC-SHA256 JWT), takže externí
auth gateway není potřeba — stačí TLS terminátor:

```bash
EDOOKIT_PUBLIC_URL=https://edookit.mcp.example \
EDOOKIT_AUTH_PASSWORD=… EDOOKIT_JWT_SECRET="$(openssl rand -base64 48)" \
edookit-mcp --http 127.0.0.1:9000
```

HTTP transport se odmítne nastartovat bez `EDOOKIT_PUBLIC_URL`,
`EDOOKIT_AUTH_PASSWORD` a `EDOOKIT_JWT_SECRET` (≥ 32 B) — `/mcp` se tak nikdy
nevystaví bez auth. `SIGINT`/`SIGTERM` ukončí server čistě. Pro server install
jsou připravené DEB/RPM balíčky se systemd unitou (viz [Distribution](#distribution-and-packaging)).

### Co umí (dostupné nástroje)

| Nástroj | Co dělá |
|---|---|
| `edookit_list_inbox` | Vypíše **Přijaté** (volitelně Nepřečtené/S hvězdičkou/Archiv/Vše). Fulltext + filtr podle data. |
| `edookit_list_sent` | Vypíše **Vytvořené** (odeslané). Stejné filtry. |
| `edookit_get_message` | Plný text jedné zprávy podle ID — subject, status, autor, datum, body_text, body_html, přílohy, doručenky. **Vedlejší efekt:** stažení zprávy ji v Edookitu **označí jako přečtenou** (jako otevření v UI) — slouží i jako „označit jako přečtené". |
| `edookit_download_attachments` | Stáhne všechny přílohy do lokálního adresáře. Default `<temp>/edookit-mcp/m-<id>/`. |
| `edookit_view_attachment` | Zobrazí přílohu inline — obrázky, **PDF vyrenderuje na obrázky stránek** + extrahovaný text, text/CSV jako obsah. |
| `edookit_list_courses` | Kurzy přihlášeného učitele (Hodnocení → Známkování v tabulce), volitelně se žáky. |
| `edookit_server_info` | Build metadata běžícího serveru (`{version, commit, build_time}`). |

Nástroje nevoláte přímo — píšete asistentovi přirozeně a on rozhodne, kdy je
použít. Když máte připojený i Gmail/Slack MCP, pomáhá v promptu zmínit
**„v Edookitu" / „ze školy"**.

### Bezpečnost a soukromí

- **Heslo** v `.env` (nebo bezpečněji v OS secret store přes wrapper). Doporučená
  oprávnění `0600` (`chmod 600 .env`).
- **Cookies** v uživatelské cache s oprávněními `0600` (off-Windows).
- **Žádné externí servery** ze strany konektoru — komunikuje jen mezi vaším
  počítačem, Edookitem a Plus4U. **Ale** AI asistent posílá výstup nástrojů (těla
  zpráv, jména třetích stran) na servery svého poskytovatele. Pro citlivá data
  (jména dětí, zdravotní/studijní detaily) použijte **no-train placený tarif s
  DPA** nebo **lokální LLM** (Continue.dev/Goose + Ollama). Na free tarifech
  zapněte opt-out z trénování. *(Nejde o právní radu — pro školní/firemní kontext
  se zeptejte DPO/IT.)*

### Časté problémy

- **Chrome se otevře, ale zůstane na úvodní stránce** → změnil se HTML layout
  Edookitu; otevřete issue se snímkem.
- **„login failed … interaction_required"** → přihlaste se ručně do
  uuidentity.plus4u.net, pak zkuste znovu.
- **opakovaně „session expired"** → `make clear-cookies`.
- **PDF příloha se nezobrazí jako obrázek** → nenašla se knihovna PDFium;
  nastavte `EDOOKIT_PDFIUM_LIB` nebo spusťte `make build` (text se extrahuje i tak).
- **občasná „network error" / 502/503/504** → tyhle blipy konektor 2× opakuje s
  backoffem; když chybu vidíte i tak, selhaly všechny tři pokusy.

---

## 🇬🇧 Technical reference

### Architecture

```
                                    ┌────────────────────────┐
                                    │  Plus4U OIDC provider  │
                                    │ uuidentity.plus4u.net  │
                                    └──────────┬─────────────┘
                                               │ (auth code flow)
┌──────────────┐  stdio / HTTP ┌─────────────┐ │
│ AI assistant │ ◄──────────►  │ edookit-mcp │ ◄┴── chromium (chromiumoxide)
│ (Claude / …) │   (+ OAuth)   │   (rmcp)    │      only for login
└──────────────┘               └──────┬──────┘
                                       │ reqwest + swappable cookie jar
                                       ▼
                              ┌────────────────────┐
                              │  Edookit backend   │
                              │  *.edookit.net     │
                              └────────────────────┘
```

Runs as a stdio MCP subprocess (default) or a Streamable HTTP server (`--http`).
On the first tool call it warms the session with `GET /` (which resurrects a PHP
session from the persistent `X-EdooAuthToken` / `X-Auth-Id` cookies), then issues
authenticated calls to the SPA's internal JSON API. If cookies are missing/stale,
chromium is driven through the full Plus4U OIDC flow and the resulting cookies are
cached (~10 h).

### Why a real browser for login

Edookit federates to Plus4U OIDC — a uu5loader-driven SPA with reCAPTCHA. The
token endpoint needs `client_secret_basic` (secret lives in Edookit's PHP
backend), so ROPC is closed. Cheapest reliable answer: drive chromium once per
~10 h via [chromiumoxide](https://github.com/mattsse/chromiumoxide), then hand the
session cookie to reqwest. A `Fetch`-domain interceptor strips the lib's hardcoded
`prompt=none` from the outgoing auth request **only** when its `client_id` matches
the per-tenant one captured from the landing page (the IdM SPA's nested silent
renewal, a different `client_id`, is left alone).

### Cookie persistence + warmup, transient retries

Edookit rotates `PHPSESSID` on every response; the persistent tokens are
`X-EdooAuthToken` / `X-Auth-Id`. `ensure_logged_in` always does a warmup `GET /`
before declaring success. All reads funnel through one helper that retries net
errors and HTTP 408/502/503/504 with exponential backoff (default 500 ms → 1 s);
HTTP 500/501/505+ and all 4xx propagate immediately (deterministic, not masked).
The cookie jar is an `ArcSwap<Mutex<…>>` so session invalidation swaps the whole
jar atomically (clearing path-scoped cookies a name-based clear would miss).

### Data flow

- **Lists** (`list_inbox`/`list_sent`): `/handler/grid/objects-for-me-data` /
  `…/created-objects-data`, 100 rows/page; each row is `[uid, uid, html]` parsed
  with `scraper`. Returns `messages` + optional `parse_warnings` (rows the server
  returned that we couldn't parse). Rows-fetched-but-none-parsed → error, not a
  silent empty mailbox.
- **Message** (`get_message`): `/handler/page/message-edit?__index=N` — one
  workspace JSON carrying the form, the attachment list, and the acceptance
  (read-receipt) grid. Interpreted via `serde_json::Value` (the `data` field is an
  object for the form/fileviewer panels but a bare array for the grid).

### Project layout

| Path | Purpose |
|---|---|
| `src/main.rs` | flag parsing (clap), env wiring, transport selection, dev runners |
| `src/server.rs` | rmcp tool registration (the 7 tools) + MCP `ServerHandler` |
| `src/http.rs` | Streamable HTTP transport + axum integration + `validate_public_url` / `guard_bind_address` |
| `src/client/` | reqwest session client, swappable cookie jar, retry/origin, cookie cache, chromiumoxide login |
| `src/tools/` | one module per tool + HTML/date utils + untrusted-data envelope + PDFium worker + experimental MCP-UI inbox widget (`ui.rs`) |
| `src/oauth/` | built-in OAuth 2.1 AS (jwt HS256, server, middleware, ratelimit, login template) |
| `packaging/` | systemd unit + env template + Debian maintainer scripts |
| `.github/workflows/` | `ci.yml` (fmt/clippy/test/audit) + `release.yml` (cross-platform + deb/rpm) |

### Dependencies

[`rmcp`](https://docs.rs/rmcp) (MCP runtime), [`chromiumoxide`](https://docs.rs/chromiumoxide)
(CDP login), [`reqwest`](https://docs.rs/reqwest) (rustls TLS) +
[`cookie_store`](https://docs.rs/cookie_store), [`scraper`](https://docs.rs/scraper)
(html5ever), [`pdfium-render`](https://docs.rs/pdfium-render) + bundled PDFium,
[`pdf-extract`](https://docs.rs/pdf-extract), [`image`](https://docs.rs/image),
[`jiff`](https://docs.rs/jiff) (bundled tzdata), [`axum`](https://docs.rs/axum) +
`tower`, hand-rolled HS256 via `hmac`/`sha2`/`subtle`, [`tokio`](https://docs.rs/tokio).

### Development

```bash
make build        # fetch PDFium + build
make test         # 93 tests, race-free
make check        # fmt + clippy-fix + test (mutates)
make pre-push     # fmt-check + clippy -D + test + audit + build (the gate)
make tools        # install cargo-audit (once)
make smoke-login  # one-shot OIDC login against EDOOKIT_URL/USER/PASS
make smoke-message MSG=m-NNNNNN   # (dev) dump raw message-edit JSON
```

### Testing

**93 tests.** White-box `#[cfg(test)]` modules per file (mirroring Go's in-package tests):
HTML/date parsers against captured samples, an `httptest`-equivalent via
[`wiremock`](https://docs.rs/wiremock) for the client + download flows, an
injected clock for the OAuth AS, and a full DCR→authorize→token→refresh→replay
flow via `tower::oneshot`. The PDFium render path is exercised against a synthetic
PDF. The chromiumoxide login is not unit-tested (same as the Go original) — run
`make smoke-login` against a live account.

### Security notes (accepted residual risks)

The threat model assumes the **operator trusts whoever drives the MCP client** —
this is a personal connector to your own school account, not a multi-tenant
service. Two design choices carry residual risk that is accepted deliberately and
documented here rather than engineered away:

- **Attachment download path is not sandboxed.** `edookit_download_attachments`
  writes to the caller-supplied `destination_dir` (with `~` expansion), faithful
  to the Go tool — there is no base directory confining where files land.
  Mitigations in place: each attachment filename is reduced to a single path
  component with `..`/traversal, absolute paths, and Windows volume-roots and
  reserved names rejected (so a hostile *filename* from Edookit can't escape the
  chosen dir), downloads stream to a `0600` temp file and commit atomically
  (no-clobber by default), each file is capped at 512 MiB, and all tool arguments
  arrive wrapped in the untrusted-data envelope so the model treats them as data.
  Residual risk: a caller can still choose *any* writable directory as the
  destination. Confine it with OS permissions (the systemd unit runs as an
  unprivileged `edookit-mcp` user) if that matters for your deployment.
- **PDFium runs in-process (no WASM sandbox).** PDF rasterization links the
  native PDFium dylib into the process, unlike the Go build's WASM (wazero)
  sandbox. A memory-safety bug in PDFium parsing a malicious PDF would therefore
  execute in the server's address space rather than a sandbox. Mitigations: the
  library is pinned + SHA-256-verified at fetch, input is your own school
  attachments (not arbitrary internet PDFs), and rendering is bounded (page count
  + pixel dimensions) on a dedicated blocking thread under a mutex. The
  WASM-sandbox property is the one place Go's stack is genuinely safer; see the
  comparison section. Accepted for the native-render performance and simplicity.

### Experimental: MCP-UI inbox widget

Every tool otherwise returns only the two universally-supported MCP content
types — `text` (JSON wrapped in the untrusted envelope) and `image` (inline
attachment view). As an opt-in experiment, setting **`EDOOKIT_UI_RESOURCES=true`**
makes `edookit_list_inbox` *additionally* append an embedded
[MCP-UI](https://mcpui.com) resource — a `text/html` widget identified by the
`ui://edookit/inbox` URI ([`src/tools/ui.rs`](src/tools/ui.rs)). MCP-UI–capable
clients render it as a clickable inbox; clicking a row posts an MCP-UI `tool`
action asking the host to call `edookit_get_message` for that id (click → detail).

Design constraints, all deliberate:

- **Purely additive.** The untrusted-JSON text block is still emitted first and
  remains the source of truth; clients that don't understand `ui://` ignore the
  extra block. Off by default so the public HTTP endpoint's clients are
  unaffected unless an operator opts in.
- **No new capability / no resource handlers.** The HTML travels *inline* in the
  tool result's `content` array (the MCP-UI embedded-resource convention), so
  there's no `resources/list`+`read` round-trip and the server stays
  `tools`-only.
- **Untrusted-data discipline preserved.** The widget HTML-escapes every
  third-party field (sender/subject/preview) and renders only list *metadata*,
  passing back an opaque `id`. The message **body** is never injected into the
  widget DOM — the detail comes back through `edookit_get_message`'s normal
  untrusted-text path. (See the escaping/XSS unit tests in `ui.rs`.)
- **Known cost.** On clients that forward all content blocks to the model, the
  extra HTML also lands in the model's context. A production version would gate
  the block on a detected client UI capability rather than a static env var.

### Distribution and packaging

`release.yml` builds for darwin/linux/windows × amd64/arm64 on a `v*` tag
(tar.gz/zip per platform), and on Linux produces DEB + RPM (via `cargo-deb` /
`cargo-generate-rpm` — metadata in `Cargo.toml`) bundling the binary, the
matching PDFium library, the systemd unit, and the env conffile, declaring
`chromium` as a dependency. Both packages carry maintainer scriptlets that
create the `edookit-mcp` system user and reload systemd (the DEB via
`packaging/deb/*`, the RPM via `generate-rpm` scriptlets in `Cargo.toml`). It
also emits a `checksums.txt` and — when the `HOMEBREW_TAP_GITHUB_TOKEN` secret is
set — pushes a Homebrew formula to `dsaiko/homebrew-tap` (skipped otherwise,
exactly like GoReleaser's `--skip=homebrew`). The bundled PDFium is pinned to a
specific `bblanchon/pdfium-binaries` release and verified against a checked-in
SHA-256 before use (`scripts/fetch-pdfium.sh`), so a tampered binary fails the
build instead of being loaded. *(The packaging workflow is provided for
GoReleaser parity but hasn't been executed against a real tag yet — the first tag
is its shakedown.)*

### License

[MIT](LICENSE) © 2026 Dušan Saiko

---

## Go vs Rust — an honest comparison

*Written from the actual port (the whole app: client, chromedp login, 6 tools,
stdio + HTTP transports, OAuth AS). The author wrote both; this is engineering
observation, not advocacy.*

**Framing caveat:** the Go original is a mature, much-reviewed codebase. Porting
it faithfully means the Rust inherits its design and defensive edge-case
handling. So this is largely *"the same careful design, in two languages"* — the
interesting differences are in what each language made **easy, safe, or
annoying**, not in the architecture.

### Measured (Apple Silicon, release builds)

| Metric | Go | Rust | Notes |
|---|---|---|---|
| Binary size (stripped) | **33.8 MB**, single static file | 25.7 MB binary **+ 6.8 MB PDFium dylib** (≈ 32 MB, two files) | Comparable total; the Rust binary alone is smaller because Go embeds PDFium-as-WASM + the Go runtime, whereas Rust keeps PDFium as a separate native dylib (the chosen tradeoff) |
| Cold build | **5.9 s** | ~104 s | Rust's big async dep tree (chromiumoxide, axum, rmcp, reqwest, image) + monomorphization |
| Warm/incremental build | 0.15 s | a few s | |
| Startup (`--version`, mean of 30) | 6.6 ms | **3.8 ms** | Rust has no runtime/GC init to amortize |
| Resident memory (idle HTTP server) | ~25.1 MiB | **~13.5 MiB** | Measured on the production gateway (Rocky Linux 10.2, aarch64): Go 0.1.13 `VmRSS` 25,712 kB / 9 threads / 1.28 GB virtual, vs Rust 0.1.1 `VmRSS` 13,796 kB (14,672 kB after a request) / 3 threads / 163 MB virtual — Rust ~46 % leaner resident, and a fraction of the virtual reservation |
| Prod LOC (excl. tests) | ~6,600 | ~5,100 | Rust a touch tighter |
| Tests | somewhat larger by LOC | **93** | Rust now covers the parsers, client (retry/origin/fast-path/concurrency), the full OAuth flow, and grid/download integration; Go's suite is still a bit larger |

Memory was measured on the actual deployment (the `gateway-ssst` public MCP
endpoint) — the long-running Go service idled at ~25 MiB resident (its lifetime
peak, `VmHWM` == `VmRSS`), while the Rust build serves the same endpoint at
~13–14 MiB with a third of the threads. Throughput under load wasn't
micro-benchmarked: this is a single-user, I/O-bound tool (every call waits on
Edookit), so it wouldn't be a meaningful differentiator. The resident-memory
gap (no GC, rustls, a slimmer thread pool) is the one runtime axis where the
difference is real and measurable.

### Where Rust helped (stability / bug-proofness)

- **The compiler caught real mistakes** during the port that Go would surface
  only at runtime (if at all): an `i64`/`u64` byte-count mismatch, a
  non-exhaustive `match` on download-classification states, several
  "did you handle the missing field?" spots where `Option` forced the decision,
  and `#[non_exhaustive]` structs that refused silent construction.
- **Errors are values you can't forget.** `Result` + `?` makes the
  re-login/retry and OAuth grant flows explicit — no `if err != nil` to omit, no
  use-after-error.
- **The hairiest logic ported with structural guarantees.** The OAuth
  refresh-rotation + replay-detection state machine runs under one
  `parking_lot::Mutex`; the type system makes "held a lock across `.await`"
  impossible by construction, and exhaustive enums model the grant outcomes.
- **Concurrency is checked, not hoped.** The swappable cookie jar
  (`ArcSwap<Mutex<…>>`) and the shared `Arc<Client>` are `Send + Sync` by proof;
  `cargo test` needs no `-race` flag because the races can't compile.

### Where Rust was more friction (Go won)

- **Compile times.** 104 s cold is felt on every dependency change; Go's ~6 s is
  a materially nicer inner loop.
- **Ecosystem archaeology.** Aligning `cookie_store`/`ego-tree` versions and
  learning the exact APIs of rmcp, axum 0.8, chromiumoxide, and pdfium-render
  each cost real time. Go's std-library-centric surface needed almost none.
- **axum 0.8 sharp edges.** `Option<ConnectInfo>` isn't a valid extractor (needs
  a custom one); `ServerInfo`/`Implementation` are `#[non_exhaustive]`.
- **chromiumoxide vs chromedp.** Driving the login surfaced a real behavioural
  gap: `page.evaluate("idmLoginClick()")` *errors* because the click starts a
  navigation that tears down the JS context — chromedp's `Evaluate` returns
  before that happens, so you tolerate the eval error instead. Chasing that also
  exposed a latent flaw the Go original *shares*: its rigid "wait-for-Plus4U →
  fill form → wait-for-Edookit" sequence **hangs when Plus4U has an active
  session and silent-SSO's straight back** (the form never shows, and the fast
  bounce slips past the host poll). The Rust port instead waits on the real
  success signal — the `X-EdooAuthToken` cookie the OIDC callback sets — and
  fills the form only if it appears, so both the form and silent-SSO paths work.
  A case where porting *improved* on the reference.
- **One place Go's design was simply better.** Go's hand-rolled cookie jar
  snapshots the jar *per request attempt* to fence a set-cookie-during-reset
  race. reqwest's `cookie_provider` can't bind the pre-send and post-response
  cookie calls to one generation, so that narrow fence is a **documented
  simplification** here (the important behavior — atomic invalidation — is kept).
- **PDFium.** Go's `go-pdfium` ships PDFium as WASM (wazero) → a no-cgo single
  binary for free, *and* runs the C++ parser inside a memory-safe sandbox. Rust
  has no turnkey equivalent, so the port links a native dylib (fetched by
  `make build`, pinned + checksum-verified, shipped alongside in packaging) that
  runs in-process. That costs the second binary *and* the sandbox: a PDFium
  memory-safety bug is in the process here, not contained — the one place Go's
  stack is genuinely safer (see *Security notes* above for the mitigations and
  why it's accepted).

### Roughly a wash

- **HTML scraping** (`scraper` vs goquery), **JSON** (serde `Value` vs
  `map[string]any`), and **MCP plumbing** (rmcp `#[tool]` macros vs mcp-go
  `AddTool`) — different ergonomics, similar effort.

### Bottom line

For *this* workload the two are closer than the language wars suggest, because the
design was fixed in advance. Rust's dividend is **compile-time correctness
guarantees** (several latent bugs caught for free) and a slightly faster/leaner
runtime; the price is **build times and ecosystem friction**. Go's dividend is a
**dramatically tighter build loop** and a **simpler single-artifact story** (the
PDFium-as-WASM trick especially); the price is that a class of bugs the Rust
compiler rejected would only show up in tests or production. Neither is "better"
here — they optimize for different things, and this port made the trade-offs
concrete rather than theoretical.
