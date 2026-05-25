# edookit-mcp

Neoficiální MCP konektor pro Edookit — umožňuje AI asistentům jako Claude číst zprávy z žákovské knížky.

---

## 🇨🇿 Pro uživatele

### Co to je

`edookit-mcp` je malý program, který propojuje [Claude](https://claude.ai/) s vaším účtem v [Edookitu](https://edookit.com). Po napojení můžete Claudovi v běžné konverzaci klást otázky o své komunikaci ve škole:

- *„Mám nějaké nové zprávy?"*
- *„Ukaž mi zprávy od ředitele za poslední týden."*
- *„Co mi přišlo s přílohou?"*
- *„Najdi všechny zprávy, kde se píše o maturitách."*

Claude pak konektor zavolá, stáhne aktuální data přímo z Edookitu a odpoví českým souvislým textem. **Neoficiální projekt — nemá nic společného s Edookit s.r.o.**

### K čemu to neslouží

- Nepřepisuje zprávy ani neposílá za vás (zatím — k zápisu na Edookit jen zatím čte). 
- Nesleduje vás na pozadí — spouští se pouze, když ho Claude vyvolá.
- Neukládá žádná data do cloudu — vše zůstává na vašem počítači.

### Co je potřeba

1. **Mac s nainstalovaným Chromem** (libovolný prohlížeč založený na Chromium funguje také — Brave, Edge, atd.).
2. **Účet v Edookitu** přihlašovaný přes Plus4U.
3. **Claude Desktop nebo Claude Code** — kamkoli kam jde nakonfigurovat MCP server.
4. **Go 1.25** pro sestavení (jednorázové).

### Instalace

```bash
git clone git@github.com:dsaiko/edookit-mcp.git
cd edookit-mcp
make tools          # nainstaluje vývojářské nástroje (jednorázově)
make build          # zkompiluje binárku do bin/edookit-mcp
```

### Konfigurace

Vytvořte soubor `.env` podle šablony:

```bash
cp .env.example .env
$EDITOR .env
```

A vyplňte:

```env
EDOOKIT_URL=https://your-school-login.edookit.net
EDOOKIT_USER=vase.jmeno@example.cz
EDOOKIT_PASS=vase-heslo
```

URL adresa je specifická pro vaši školu — najdete ji v adresním řádku po přihlášení do Edookitu (např. `https://moje-skola-login.edookit.net`).

### První spuštění (ověření)

```bash
make smoke-login        # ověří, že přihlášení a stažení dat funguje
make test-messages      # vytiskne pár posledních zpráv ze schránky
```

Při prvním spuštění se na pár vteřin otevře okno Chromu, projde přes Plus4U a vyplní přihlašovací formulář. Cookies se uloží do `~/Library/Caches/edookit-mcp/cookies.json` a další spuštění už Chrome neotevírá — vystačí si s uloženou relací (až ~10 hodin). Pro vynucené nové přihlášení:

```bash
make clear-cookies
```

### Připojení k Claude Code

Otevřete soubor `~/.claude.json` a do sekce `mcpServers` přidejte:

```json
{
  "mcpServers": {
    "edookit": {
      "command": "/absolutní/cesta/k/edookit-mcp/bin/edookit-mcp",
      "env": {
        "EDOOKIT_URL": "https://your-school-login.edookit.net",
        "EDOOKIT_USER": "vase.jmeno@example.cz",
        "EDOOKIT_PASS": "vase-heslo"
      }
    }
  }
}
```

Restartujte Claude Code a v konverzaci by se měly objevit nástroje `list_inbox` a `list_sent`.

### Co umí (dostupné nástroje)

| Nástroj | Co dělá |
|---|---|
| `list_inbox` | Vypíše zprávy z **Přijaté** (volitelně jen Nepřečtené, S hvězdičkou, Archiv, Vše). Podporuje fulltext a filtrování podle data. |
| `list_sent` | Vypíše zprávy z **Vytvořené** (odeslané). Stejné filtry kromě "view". |

Každá zpráva obsahuje ID, datum, odesílatele (u příjmu) / stav (u odeslaných), předmět, prvních ~200 znaků textu a počet příloh.

### Bezpečnost a soukromí

- **Heslo** je uloženo v souboru `.env` (s běžnými právy 0600). Pokud používáte FileVault (zapnutý standardně na novějších Macích), je to dostačující ochrana proti odcizenému disku.
- **Cookies** jsou v `~/Library/Caches/edookit-mcp/cookies.json` (taky 0600). Vyloučeny ze zálohy Time Machine / iCloud — patří totiž do systémové cache. Více v sekci [otázek o šifrování](#proč-nejsou-cookies-šifrované) níže.
- **Žádné externí servery** — komunikace probíhá pouze mezi vaším počítačem, Edookitem a Plus4U. Žádný telemetrický kanál, žádné cloudové úložiště. Vstupní data od Claudea zpracovává model Anthropic dle [jeho privacy policy](https://www.anthropic.com/privacy).

#### Proč nejsou cookies šifrované?

XOR nebo podobné „obfuskace" by vám neposkytly žádnou skutečnou ochranu — útočník s přístupem k souborům má i přístup k binárce a tedy ke klíči. Skutečným bezpečnostním pásmem je FileVault (šifrování celého disku) a oprávnění souborů. Pokud chcete jít dál, dlouhodobé řešení je uložení hesla do macOS Keychain — to může být budoucí vylepšení.

### Časté problémy

- **Chrome se otevírá, ale zůstane na úvodní stránce.** Pravděpodobně se změnil HTML layout Edookitu. Otevřete issue v GitHubu se snímkem obrazovky.
- **„login failed: ...interaction_required"** — váš účet v Plus4U nemusí mít aktivní session. Přihlaste se ručně do uuidentity.plus4u.net, pak zkuste znovu.
- **„session expired"** opakovaně. Smažte cache (`make clear-cookies`) a zkuste znovu. Pokud to nepomáhá, změnilo se chování Edookitu — issue v GitHubu.

---

## 🇬🇧 Technical reference

### Architecture

```
                                    ┌────────────────────────┐
                                    │  Plus4U OIDC provider  │
                                    │ uuidentity.plus4u.net  │
                                    └──────────┬─────────────┘
                                               │ (auth code flow)
┌──────────┐    stdio MCP   ┌─────────────┐    │
│  Claude  │ ◄────────────► │ edookit-mcp │ ◄──┴── chromium (chromedp)
└──────────┘                └──────┬──────┘        only for login
                                   │
                                   │ net/http + cookie jar
                                   ▼
                          ┌────────────────────┐
                          │  Edookit backend   │
                          │  *.edookit.net     │
                          └────────────────────┘
```

`edookit-mcp` runs as a stdio MCP subprocess of Claude. On startup it loads cached session cookies from `~/Library/Caches/edookit-mcp/cookies.json` if present. On first tool call it warms the session up with `GET /` (which the Edookit backend uses to resurrect a PHP session from the persistent auth tokens), then issues authenticated calls to the SPA's internal JSON API.

If no cookies exist (or they're stale), chromium is launched in the background to drive the full Plus4U OIDC code flow — username/password submission, redirect chain, callback — and the resulting session cookies are saved.

### Why a real browser for login

Edookit federates to Plus4U OIDC, which is **rendered by a uu5loader-driven SPA** with reCAPTCHA. The OIDC token endpoint requires `client_secret_basic` auth — the client secret only lives in Edookit's PHP backend, so ROPC (`grant_type=password`) is closed off. The login UI itself uses dynamic JS components rather than a static form. Cheapest reliable answer: drive a real chromium instance once per ~10 hours via [chromedp](https://github.com/chromedp/chromedp), then hand the session cookie off to a normal `net/http` client for all reads.

A specific quirk worth knowing: the OIDC client library hardcodes `prompt=none` (silent SSO) on its outgoing auth request, which fails with `interaction_required` for users without an active Plus4U session. The fetch-domain interceptor in `loginViaBrowser` strips `prompt=none` from outgoing requests when the `client_id` matches Edookit's — but leaves it alone for the IdM SPA's nested silent renewal (different `client_id`).

### Cookie persistence and the warmup

Edookit's backend rotates `PHPSESSID` on every response, and `/handler/page/*` paths return `authenticated:false` for any session ID the server didn't just issue. The reconciliation: the **persistent** auth tokens are `X-EdooAuthToken` and `X-Auth-Id`, set by the OIDC callback handler and stable for the session lifetime (~12 h). A `GET /` request with those cookies makes the server mint a fresh `PHPSESSID` tied to a valid PHP session.

`ensureLoggedIn` therefore always does a warmup `GET /` before declaring the client "logged in" — whether the cookies came from cache or a fresh chromedp run. The cookie jar transparently handles subsequent rotations.

### Data flow for `list_inbox` / `list_sent`

The Edookit SPA is a thin wrapper over `/handler/page/X` (page descriptors) and `/handler/grid/X-data` (row data). For messages:

- `/handler/page/objects-for-me` → grid descriptor (column schema, filters, total count).
- `/handler/grid/objects-for-me-data?object_type_general=object_type_message&object_filter=inbox&page=N` → 100 rows per page. Each row is `[uid, uid, html]` where the HTML blob carries date, sender, subject, attachments count, and body preview.
- `/handler/grid/created-objects-data?object_type_general=object_type_message&page=N` → same shape, sent messages, but the leading `<span>` holds the publication status instead of the sender.

The package `internal/tools` parses each row's HTML with goquery, extracts structured fields, and returns `[]Message`. Pagination, optional fulltext (`?fulltext=`), and a client-side `since` date floor are all implemented in `fetchAndParse`.

### Project layout

| Path | Purpose |
|---|---|
| `main.go` | Flag parsing, MCP server bootstrap, tool registration |
| `internal/client/client.go` | Session-aware HTTP client (`GetJSON`, `GetDoc`, warmup, cookie cache) |
| `internal/client/login_chromedp.go` | OIDC login via chromedp (Plus4U landing page → fetch interception → form submission → callback) |
| `internal/client/cookie_store.go` | On-disk cookie persistence (`~/Library/Caches/edookit-mcp/cookies.json`) |
| `internal/tools/messages.go` | `ListInbox` / `ListSent`, HTML row parsing |
| `internal/tools/*_test.go` | Unit + integration tests (91.9% coverage) |

### Dependencies

- [`github.com/mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) — MCP server runtime
- [`github.com/chromedp/chromedp`](https://github.com/chromedp/chromedp) — Chrome DevTools Protocol driver for the OIDC login
- [`github.com/PuerkitoBio/goquery`](https://github.com/PuerkitoBio/goquery) — HTML parsing for row data extraction

### Development

```bash
make help                  # list all targets
make check                 # format + vet + lint-fix + tests
make build                 # build bin/edookit-mcp
make run                   # run the MCP server (waits for stdio framing)
make smoke-login           # one-shot OIDC login + dashboard probe
make test-messages         # one-shot list inbox + sent (smoke for the tools)
make dump-html             # dump the rendered landing page (selector debugging)
make clear-cookies         # delete the session cache; forces re-login
```

The project follows the Makefile conventions used by Oddin's Go services (shujinko, fujin, kira): pinned tool versions via `go tool` directives, `golangci-lint` v2 config, `gofumpt` + `goimports` formatters, and a `check` aggregate target.

### Testing

Unit tests cover the HTML parsers (date, sender, subject, attachments, body preview) against captured live row samples. Integration tests stand up an `httptest.Server` that mimics Edookit's grid endpoint and exercises pagination, `since` boundary stopping, `limit` truncation, fulltext propagation, view validation, and the sent-messages path. Run with:

```bash
go test -race -count=1 -cover ./internal/tools/...
```

Login + browser-driven flow are not unit-tested — they're exercised by `make smoke-login` against the live Edookit instance whenever you have a real account.

### Per-school configuration

Two values are currently hardcoded for [SSST](https://www.ssst.cz/) (the school this MCP was originally built for) and would need rework for use by another Edookit tenant:

1. The `EDOOKIT_URL` is per-school but already env-driven — just put your own URL in `.env`.
2. The Plus4U OIDC client_id (`plus4uClientID` in `login_chromedp.go`) is per-tenant. To support arbitrary schools it should be extracted at runtime from the landing-page JS literal `uu_app_oidc_providers_oidcg02_client_id`. Open an issue if you need this.

### License

[MIT](LICENSE) © 2026 Dušan Saiko
