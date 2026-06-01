# edookit-mcp-rs

A **Rust port** of [`edookit-mcp`](../edookit-mcp) (originally Go) — an unofficial MCP
connector for the Edookit Czech school information system. This repo exists to
**compare the two implementations** side by side: same behavior, same security
posture, two languages.

> ⚠️ **Work in progress.** This is a faithful re-implementation in flight. The
> table below tracks what's done. The Go project is the reference spec.

## Status

| Area | State | Tests |
|---|---|---|
| Scaffold, `build.rs` version injection, `--version` | ✅ done | — |
| `client` — config/URL validation, swappable cookie jar, retry/origin/auth-envelope, `get_json`/`get_doc`/`get_to`/`get_bytes`, cookie cache | ✅ done | 23 |
| `client::login` — chromiumoxide OIDC (fetch-intercept `prompt=none` strip, step sequence, cookie capture) | ✅ compiles¹ | — |
| `tools::untrusted` — prompt-injection envelopes | ✅ done | 2 |
| `tools::messages` — `list_inbox` / `list_sent` (row HTML, dates, pagination, `since`) | ✅ done | 8 |
| `tools::message` — `get_message` (workspace JSON, body→text, recipients) | ✅ done | 7 |
| `tools::courses` — `list_courses` (+ rosters) | ✅ done | 5 |
| `tools::attachments` — `download_attachments` (traversal guards, race-free commit) | ✅ done | 6 |
| `tools::view` — `view_attachment` (image downscale, PDF text + raster) | ⏳ todo | — |
| MCP server wiring (`main`, rmcp stdio, tool registration) | ⏳ todo | — |
| `oauth` — built-in OAuth 2.1 AS | ⏳ todo | — |
| Streamable HTTP transport + axum integration | ⏳ todo | — |
| Packaging (cargo-dist, deb/rpm, systemd, Homebrew, CI) | ⏳ todo | — |
| **Total** | | **51 passing** |

¹ The browser-login path has no automated tests (same as the Go original) — it's
verified by `--login-test` against a live Edookit instance.

## Build / test

```bash
cargo build              # debug build (TLS via rustls — no OpenSSL needed)
cargo test               # 51 tests, race-free, ~ms
cargo clippy             # lints
./target/debug/edookit-mcp --version
```

Rust 1.96+, edition 2024.

---

## Go vs Rust — an honest comparison

> *Preliminary, written from the actual port experience so far (client + login +
> 5/6 tools). Expanded as the remaining phases land. The author wrote both; this
> is engineering observation, not advocacy.*

A caveat first: the Go original is **already very mature and defensive** — it has
absorbed many review rounds and edge-case fixes. Porting it faithfully means the
Rust inherits that design. So this is mostly *"the same careful design, in two
languages"* rather than *"which language produces a better design"*.

### Where Rust helped (stability / bug-proofness)

- **The type system caught real mistakes at compile time** that Go would only
  surface (if at all) at runtime: an `i64`/`u64` byte-count mismatch, a
  non-exhaustive match on the download-classification states, and several
  "did you handle the missing field?" spots where `Option` forced a decision.
- **Errors are values you can't forget.** `Result` + `?` makes the
  re-login/retry control flow explicit; there's no `if err != nil` to omit, and
  no accidental use of a half-initialized value after an error.
- **The cookie-jar swap is provably race-free.** Go used an `atomic.Pointer`
  with careful comments; the Rust `ArcSwap<Mutex<…>>` expresses the same intent,
  and the borrow checker guarantees no torn read — the invariant is in the types,
  not the comments.
- **Sentinel errors are exhaustive.** `ClientError::AttachmentTooLarge` as an enum
  variant (matched with `matches!`) beats Go's `errors.Is(err, ErrX)` sentinel —
  the compiler knows the full set.
- **Tests are fast and deterministic.** `wiremock` + `#[tokio::test]` gave the
  same HTTP-fake coverage as Go's `httptest`, running in milliseconds; white-box
  `#[cfg(test)] mod` modules mirror Go's in-package tests one-for-one.

### Where Rust was more friction (difficulties)

- **Ecosystem archaeology.** Getting the cookie/`cookie_store` API right, and
  discovering `scraper` doesn't re-export `ego-tree` (so a transitive dep had to
  be version-pinned by hand) cost time Go's std-library-centric stack doesn't.
- **One place Go's design was genuinely easier.** Go's hand-rolled cookie jar let
  it snapshot the jar *per request attempt* to fence a set-cookie-during-reset
  race. reqwest's `cookie_provider` can't bind the pre-send and post-response
  cookie calls to one generation, so that narrow fence is a **documented
  simplification** in the Rust port (the important behavior — atomic
  invalidation clearing path-scoped cookies — is preserved).
- **Async lifecycle is more ceremony.** The chromiumoxide login has to juggle a
  browser handle, a spawned handler task, and abort-on-drop guards for the event
  listeners — where Go leaned on `defer cancel()` and goroutines with less
  boilerplate.
- **`!Send` will bite PDFium.** PDFium isn't thread-safe, so the Rust view tool
  needs a dedicated worker thread + channel; Go's `go-pdfium` pool abstracted
  that away. (And the WASM-no-cgo trick the Go build used has no turnkey Rust
  equivalent — hence the native-lib dependency, a deliberate divergence.)
- **More upfront design.** serde derives, explicit lifetimes on DOM node refs,
  and choosing concrete types cost more keystrokes than Go's structural typing —
  though they're also what bought the compile-time guarantees above.

### Roughly a wash

- **HTML scraping:** `scraper` (CSS + html5ever) vs goquery — comparable; raw
  tree-walking for the text renderers is similar effort either way.
- **JSON:** serde's typed derive is nicer than `encoding/json` for fixed shapes,
  but the loosely-typed `message-edit` response needed `serde_json::Value` — the
  direct analog of Go's `map[string]any`. Net neutral.
- **Lines of code:** comparable so far (the Rust is marginally longer due to
  explicit error types and derives).

### Performance

**Not yet measured** — benchmarking is a later phase. Early structural notes: the
Rust build defaults to rustls (no system OpenSSL), `jiff` bundles tzdata like
Go's `time/tzdata`, and the binary is a single artifact (modulo the PDFium native
lib). Real numbers (cold-start, per-request latency, memory, binary size) will go
here once both are measured on the same workload.
