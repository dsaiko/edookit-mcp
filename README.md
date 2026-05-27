# edookit-mcp

Neoficiální MCP konektor pro Edookit — umožňuje AI asistentům (Claude, ChatGPT, Cursor, VS Code Copilot a dalším MCP-kompatibilním klientům) číst zprávy z žákovské knížky.

---

## 🇨🇿 Pro uživatele

### Co to je

`edookit-mcp` je malý program, který propojuje vašeho AI asistenta — [Claude](https://claude.ai/), [ChatGPT](https://chatgpt.com/), [Cursor](https://cursor.com/), VS Code s Copilotem nebo jiného klienta s podporou [MCP](https://modelcontextprotocol.io/) — s vaším účtem v [Edookitu](https://edookit.com). Po napojení můžete asistentovi v běžné konverzaci klást otázky o své komunikaci ve škole:

- *„Mám nějaké nové zprávy?"*
- *„Ukaž mi zprávy od ředitele za poslední týden."*
- *„Co mi přišlo s přílohou?"*
- *„Najdi všechny zprávy, kde se píše o maturitách."*

Asistent pak konektor zavolá, stáhne aktuální data přímo z Edookitu a odpoví českým souvislým textem. **Neoficiální projekt — nemá nic společného s Edookit s.r.o.**

### K čemu to neslouží

- Nepřepisuje zprávy ani neposílá za vás (zatím — k zápisu na Edookit jen zatím čte). 
- Nesleduje vás na pozadí — spouští se pouze, když ho asistent vyvolá.
- Neukládá žádná data do cloudu — vše zůstává na vašem počítači.

### Co je potřeba

1. **macOS, Linux nebo Windows** s nainstalovaným Chromem (případně Chromium, Brave, Edge — cokoli na Chromium jádře).
2. **Účet v Edookitu** přihlašovaný přes Plus4U.
3. **AI klient s podporou MCP** — Claude Desktop, Claude Code, ChatGPT desktop, Cursor, VS Code s GitHub Copilotem (agent mode), Zed, Continue.dev a další. Konkrétní postup pro nejpoužívanější klienty najdete níže v sekci [Připojení k AI asistentovi](#připojení-k-ai-asistentovi).
4. **Go 1.26+** pro sestavení (jednorázové).

> **Poznámka pro Windows uživatele:** `make` targety v tomto repozitáři používají bash, takže fungují přes [WSL](https://learn.microsoft.com/cs-cz/windows/wsl/install) nebo [Git Bash](https://gitforwindows.org/). Bez nich lze projekt sestavit přímo: `go build -o edookit-mcp.exe .` Funkčně je vše ostatní platformově neutrální.

### Instalace

Tři možnosti — stačí si vybrat jednu.

#### 1. Stáhnout hotovou binárku z GitHub Releases *(doporučeno)*

Pro každý nový release se automaticky kompilují binárky pro všechny platformy. Stačí jít na [stránku releases](https://github.com/dsaiko/edookit-mcp/releases/latest) a stáhnout archiv pro váš operační systém:

| Platforma | Soubor |
|---|---|
| macOS (Apple Silicon) | `edookit-mcp_<verze>_Darwin_arm64.tar.gz` |
| macOS (Intel) | `edookit-mcp_<verze>_Darwin_x86_64.tar.gz` |
| Linux (x86_64) | `edookit-mcp_<verze>_Linux_x86_64.tar.gz` |
| Linux (ARM64) | `edookit-mcp_<verze>_Linux_arm64.tar.gz` |
| Windows (x86_64) | `edookit-mcp_<verze>_Windows_x86_64.zip` |

Rozbalte archiv, binárku přesuňte někam do `$PATH` (např. `/usr/local/bin/` na Mac/Linux) nebo si zapamatujte plnou cestu k ní.

> **macOS Gatekeeper:** při prvním spuštění Apple zablokuje binárku, protože není podepsaná Apple Developer účtem. Obejdete to buď: pravým tlačítkem → Otevřít → Otevřít, nebo z terminálu: `xattr -d com.apple.quarantine /cesta/k/edookit-mcp`. Jednorázově.
>
> **Windows SmartScreen** může zobrazit varování. Klikněte „More info" → „Run anyway".

#### 2. Přes Homebrew *(macOS / Linux)*

```bash
brew install dsaiko/tap/edookit-mcp
```

Homebrew si o aktualizace řekne sám při `brew upgrade`. Bez problémů s Gatekeeperem (Homebrew sám si stáhnuté binárky bypass-uje).

#### 3. Sestavit ze zdrojáků

Pokud máte Go 1.26+ nainstalované a chcete poslední vývojovou verzi:

```bash
go install github.com/dsaiko/edookit-mcp@latest
```

Nebo klasicky:

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
chmod 600 .env       # ať heslo není čitelné pro ostatní uživatele systému
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

Při prvním spuštění se na pár vteřin otevře okno Chromu, projde přes Plus4U a vyplní přihlašovací formulář. Cookies se uloží do uživatelské cache (na macOS `~/Library/Caches/edookit-mcp/cookies.json`, na Linuxu `~/.cache/edookit-mcp/cookies.json`, na Windows `%LocalAppData%\edookit-mcp\cookies.json`) a další spuštění už Chrome neotevírá — vystačí si s uloženou relací (až ~10 hodin). Pro vynucené nové přihlášení:

```bash
make clear-cookies
```

### Připojení k AI asistentovi

`edookit-mcp` je standardní MCP server — běží jako stdio subproces vašeho asistenta a mluví [Model Context Protocol](https://modelcontextprotocol.io/). Funguje s libovolným klientem, který MCP podporuje.

Společné pro většinu klientů je tahle JSON konfigurace (Anthropic / Claude formát, který přejala většina ekosystému):

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

Liší se hlavně **kam ji vložit**. Po každé změně klienta restartujte.

> **Tip: heslo mimo config JSON (Keychain).** Pokud nechcete mít `EDOOKIT_PASS` v plaintextu v config souboru ani v `.env`, použijte **wrapper skript**, který heslo načte z OS secret store až při spuštění — viz [Varianta: heslo v Keychainu](#varianta-heslo-v-keychainu). V `command` pak ukážete na ten skript a `env` blok úplně vynecháte.

#### Claude Code

Konfigurační soubor: `~/.claude.json`. Vložte výše uvedený `mcpServers` blok (pokud sekce neexistuje, vytvořte ji; pokud existuje, přidejte do ní jen klíč `"edookit"`).

#### Claude Desktop

Konfigurační soubor:

- **macOS**: `~/Library/Application Support/Claude/claude_desktop_config.json`
- **Windows**: `%APPDATA%\Claude\claude_desktop_config.json`
- **Linux**: oficiální Claude Desktop pro Linux zatím neexistuje — použijte Claude Code v terminálu, případně Claude Desktop přes WSL na Windows.

Struktura souboru je stejná jako u Claude Code (`{"mcpServers": {...}}`).

#### Cursor

- **Per-project** (sdílí se přes git, takže to vidí jen kdo má repo): `<projekt>/.cursor/mcp.json`
- **Globálně**: `~/.cursor/mcp.json`, případně přes Settings → Features → MCP → *New MCP Server* v UI

Stejná `{"mcpServers": {...}}` struktura. Po přidání zkontrolujte v Cursoru Settings → Features → MCP, že je server v zeleném stavu.

#### ChatGPT (desktop)

OpenAI postupně přidává MCP support do ChatGPT desktop aplikace přes Settings → **Connectors** (nebo *Apps*, podle verze). Připojuje se buď přes GUI (zadáte cestu k spustitelnému souboru + env vars) nebo přes externí konfigurační soubor. Mechanismus se průběžně mění — pro aktuální postup doporučuji [OpenAI MCP docs](https://platform.openai.com/docs/mcp) a [ChatGPT app changelog](https://help.openai.com/).

Pokud jde o vstupní hodnoty, vždy budete potřebovat:
- **Command**: absolutní cesta k binárce `edookit-mcp`
- **Env vars**: `EDOOKIT_URL`, `EDOOKIT_USER`, `EDOOKIT_PASS`

#### VS Code (GitHub Copilot, agent mode)

GitHub Copilot v agent módu podporuje MCP. Konfigurace:

- **Per-workspace**: `<workspace>/.vscode/mcp.json`
- **Globálně**: User Settings, klíč pro MCP servery

VS Code používá nepatrně jiný JSON shape než Anthropic formát (klíče `servers` místo `mcpServers`, mírně odlišné pole pro env). Aktuální syntax + příklady viz [VS Code MCP dokumentace](https://code.visualstudio.com/docs/copilot/chat/mcp-servers). Hodnoty pro `command` a env vars jsou stejné jako v Anthropic formátu výše.

#### Jiný MCP klient

Pokud váš klient (Zed, Continue.dev, Windsurf, Goose, …) podporuje MCP, podívejte se do jeho dokumentace na cestu konfiguračního souboru a očekávanou JSON strukturu. Náš binary mluví standardní MCP přes stdio, takže by mělo stačit zadat:

- **Command**: absolutní cesta k `edookit-mcp` binárce
- **Args**: žádné
- **Env vars**: `EDOOKIT_URL`, `EDOOKIT_USER`, `EDOOKIT_PASS` (a volitelně `EDOOKIT_TIMEZONE`, `EDOOKIT_COOKIE_CACHE`, `EDOOKIT_NO_COOKIE_CACHE`, `EDOOKIT_HEADLESS_LOGIN`, `EDOOKIT_ALLOW_INSECURE_HTTP` — viz [.env.example](.env.example))

Po napojení by se v konverzaci měly objevit nástroje `edookit_list_inbox`, `edookit_list_sent`, `edookit_get_message`, `edookit_download_attachments` a `edookit_view_attachment`.

### Co umí (dostupné nástroje)

K dispozici je pět nástrojů:

| Nástroj | Co dělá |
|---|---|
| `edookit_list_inbox` | Vypíše zprávy z **Přijaté** (volitelně jen Nepřečtené, S hvězdičkou, Archiv, Vše). Podporuje fulltext a filtrování podle data. |
| `edookit_list_sent` | Vypíše zprávy z **Vytvořené** (odeslané). Stejné filtry kromě "view". |
| `edookit_get_message` | Stáhne **plný text** jedné konkrétní zprávy podle ID — to, co `list_*` vrací jen jako ~200znakový preview. Funguje pro přijaté i odeslané. Vrací subject, status, autora, datum, body_text (plain text), body_html (originál) a metadata všech příloh. |
| `edookit_download_attachments` | **Stáhne všechny přílohy** jedné zprávy do lokálního adresáře. Funguje pro přijaté i odeslané. Defaultně ukládá do `<os-temp>/edookit-mcp/<id>/` (přenositelné napříč OS); explicitní cestu lze předat parametrem. |
| `edookit_view_attachment` | **Zobrazí jednu přílohu inline** přímo v konverzaci (bez ukládání na disk). Obrázky vrací jako obrázkový blok (velké zmenší na 1568 px), PDF jako extrahovaný text, textové/CSV soubory jako jejich obsah. U PDF a ostatních binárních typů (Office apod.) navíc přiloží **surový soubor jako MCP resource** — capable klient ho může zobrazit/nabídnout, ale podpora se liší podle klienta; pro spolehlivou lokální kopii použijte `edookit_download_attachments`. ID přílohy zjistíte přes `edookit_get_message`. |

Nástroje **nevoláte přímo** — píšete Claudovi normálním jazykem a on sám rozhodne, kdy a s jakými parametry je použít. Níže jsou příklady promptů a co se pod nimi typicky děje.

> **Jak Claude pozná, že má použít Edookit?** Rozhoduje se podle popisu nástroje, podle toho jaké další MCP máte k Claudovi připojené, a podle kontextu konverzace. Když máte připojený jen edookit-mcp, je to jednoznačné a stačí psát přirozeně. Když máte i Gmail / Outlook / Slack MCP, pomáhá v promptu **zmínit „v Edookitu" nebo „ze školy"** — Claude pak nepřesměruje dotaz omylem do mailu. Slova jako „paní učitelka", „třídní", „ředitel", „pololetí" obvykle stačí sama o sobě, ale **explicitní zmínka je nejjistější**.

#### Příklady, jak se zeptat

**Přehled nepřečtených:**
> *"Mám něco nového v Edookitu?"*
> *"Kolik mám nepřečtených zpráv ze školy?"*

→ Claude zavolá `edookit_list_inbox` s `view=unread` a shrne, kolik zpráv máte, od koho a o čem.

**Hledání podle odesílatele nebo tématu:**
> *"Co mi v poslední době psala paní učitelka Nováková?"*
> *"Najdi v Edookitu všechny zprávy, kde se píše o maturitách."*
> *"Které zprávy od ředitele mám z posledního měsíce?"*

→ Claude použije `edookit_list_inbox` s `fulltext="..."` (server-side hledání napříč odesílateli, předměty i těly).

**Filtrování podle data:**
> *"Co mi přišlo ze školy za poslední týden?"*
> *"Ukaž mi všechny zprávy z Edookitu od 1. května."*
> *"Co bylo v Edookitu za poslední tři dny?"*

→ Claude předá `since="7d"`, `since="2026-05-01"` apod.

**Plný text jedné zprávy:**
> *"Otevři mi tu zprávu od ředitele a přečti, co píše."*
> *"Co přesně píše paní učitelka Nováková v té zprávě o exkurzi?"*

→ Po vyhledání ID přes `edookit_list_inbox` zavolá Claude `edookit_get_message` s konkrétním ID a dostane plné tělo (`body_text` čistý text, `body_html` originál). Funguje stejně pro přijaté i odeslané.

**Stažení příloh:**
> *"Stáhni přílohy té zprávy ze školy."*
> *"Ulož si to PDF od paní učitelky někam na disk."*
> *"Stáhni přílohy do ~/Documents/skola/."*

→ Claude zavolá `edookit_download_attachments` — defaultně uloží soubory do `<os-temp>/edookit-mcp/<message-id>/` (na macOS/Linuxu typicky `/tmp/...`, na Windows `%TMP%\...`). Pokud chcete jinam, zmiňte cílový adresář v promptu; Claude předá hodnotu jako parametr `destination_dir`. Návratová hodnota obsahuje konkrétní cesty k souborům.

**Zobrazení přílohy přímo v konverzaci:**
> *"Ukaž mi tu fotku z té zprávy."*
> *"Co je v tom PDF od třídní?"*
> *"Přečti mi obsah té přílohy."*

→ Claude zavolá `edookit_view_attachment` a přílohu vrátí **inline** (bez ukládání na disk): obrázek se zobrazí přímo, z PDF přečte text, textový/CSV soubor vypíše. U naskenovaných/obrázkových PDF, Office dokumentů a jiných binárních typů Claude doporučí `edookit_download_attachments`.

**Zprávy s přílohou (jen seznam, bez stahování):**
> *"Která nepřečtená zpráva ze školy má přílohu?"*
> *"Najdi mi v Edookitu PDF, co mi nedávno přišlo."*

→ Claude vytáhne nepřečtené (`view=unread`) a vyfiltruje ty, které mají `attachments > 0`. Samotné soubory nestahuje, dokud si o to neřeknete — viz "Stažení příloh" výše.

**Odeslané zprávy:**
> *"Co jsem v poslední době někomu v Edookitu posílal?"*
> *"Poslal jsem už paní Novákové ten dotaz na exkurzi?"*

→ `edookit_list_sent` (případně s `fulltext="Nováková"` nebo `since="2w"`).

**Souhrn za období:**
> *"Udělej mi přehled komunikace s třídní za poslední pololetí."*

→ Claude může zavolat oba nástroje, případně několikrát s různými filtry, a sestaví souvislé shrnutí.

#### Parametry

Pokud chcete Claudovi pomoci přesně, můžete parametry zmínit explicitně ("za posledních 30 dní", "jen nepřečtené", "max 20 zpráv"). Akceptované hodnoty:

**`edookit_list_inbox` / `edookit_list_sent`:**

| Parametr | Hodnoty | Default |
|---|---|---|
| `view` (jen `edookit_list_inbox`) | `inbox` (Přijaté), `unread` (Nepřečtené), `starred` (S hvězdičkou), `archived` (Archiv), `all` (Vše) | `inbox` |
| `fulltext` | libovolný text — hledá se na straně serveru napříč odesílatelem, předmětem i tělem | — |
| `since` | relativní (`7d`, `1w`, `2m`, `1y`) nebo absolutní (`YYYY-MM-DD`, popř. RFC 3339) | bez omezení |
| `limit` | 1–200 (interně se stránkuje po 100) | 50 |

**`edookit_get_message`:**

| Parametr | Hodnoty | Default |
|---|---|---|
| `id` (povinné) | `m-NNNNNN` nebo bare `NNNNNN` — ID zprávy z `list_inbox`/`list_sent` | — |

**`edookit_download_attachments`:**

| Parametr | Hodnoty | Default |
|---|---|---|
| `id` (povinné) | jako u `get_message` | — |
| `destination_dir` | absolutní cesta, cesta začínající `~/` (rozvine se do home dir), nebo holé `~` (home dir samotný); relativní cesty server odmítne s chybou — cwd MCP serveru není stabilní | `<os-temp>/edookit-mcp/m-<číslo>/` — `m-` prefix je vždy přítomen, i když jste do `id` parametru předali bare číslo (na macOS/Linuxu typicky `/tmp/...`, na Windows `%TMP%\...`) |
| `overwrite` | `true` / `false` — má se existující soubor přepsat? | `false` (existující soubor se přeskočí) |

#### Co dostanete zpět

**Z `edookit_list_inbox` / `edookit_list_sent`** — pole `messages`, kde každá položka má:

- **`id`** + **`number`** — vnitřní identifikátor (`m-290491` / `290491`)
- **`date`** — datum a čas (RFC 3339, podle časové zóny školy — default Europe/Prague, lze přebít přes `EDOOKIT_TIMEZONE`)
- **`sender`** (v Přijatých) nebo **`status`** (v Odeslaných, např. „Publikováno")
- **`subject`** — předmět zprávy v originálním jazyce (typicky česky)
- **`body_preview`** — prvních ~200 znaků textu zprávy
- **`attachments`** — **počet** příloh (jen číslo; pro samotné soubory použijte `edookit_download_attachments`)

Vedle `messages` může přijít i `parse_warnings` — to jsou řádky, které Edookit vrátil v neočekávaném formátu (typicky když změnili layout). Když Claude něco takového dostane, většinou na to upozorní. Pokud parser selže úplně na všech řádcích, místo prázdného seznamu se vrátí chyba — jinak by Claude nemohl odlišit „schránka je prázdná" od „parser je rozbitý".

**Z `edookit_get_message`** — jeden objekt:

- **`id`** + **`number`** — stejné jako výše
- **`subject`** — předmět
- **`status`** — slovo z UI (typicky „Publikováno" / „Nepublikováno")
- **`author`** — odesílatel u přijatých, publikující uživatel u odeslaných (typicky vy sami)
- **`date`** — RFC 3339, pokud je v hlavičce zprávy parseable; jinak chybí
- **`body_text`** — plain text těla (entity dekódované, tagy odstraněné, paragraf/`<br>` převedené na konce řádků)
- **`body_html`** — originální HTML, jak ho Edookit posílá (užitečné, když chcete zachovat odkazy a formátování)
- **`deleted`** — `true` pokud autor zprávu v Edookitu smazal. V takovém případě je `subject` / `body_text` / `body_html` prázdné (Edookit je server-side stripne), ale `status`, `author` a `date` zůstávají. Volitelné pole — `false` / chybí pro normální zprávy
- **`attachments`** — pole `{id, name, url, date}` — `url` je plně kvalifikovaná, ale download přes browser by vyžadoval přihlášenou session; pro spolehlivé stažení použijte `edookit_download_attachments`
- **`recipients`** — doručenky (sekce „Doručenky" v Edookitu). Pole `{name, read_at, parents, parents_read_at}`:
  - `name` — jméno příjemce (např. „Fajkus Eliáš")
  - `read_at` — ISO datum „2026-05-21" kdy si příjemce zprávu poprvé otevřel; prázdný řetězec = ještě nečetl
  - `parents` — seznam rodičů příjemce (např. `["Fajkus Martin", "Fajkusová Soňa"]`); prázdné pro učitele/zaměstnance
  - `parents_read_at` — kdy si rodiče zprávu přečetli, **vždy zarovnáno s `parents`** (Edookit někdy posílá jednu hodnotu pro všechny stejné, parser ji rozkopíruje). ISO datum nebo prázdný řetězec
  - Pro **odeslané** zprávy je tohle nejužitečnější — vidíte, kdo si zprávu přečetl a kdo ne. Pro **přijaté** zprávy je tam typicky jen jeden záznam (vy sami).

**Z `edookit_download_attachments`** — jeden objekt:

- **`message_id`** — ID zprávy, ke které se to stáhlo
- **`directory`** — absolutní cesta k výslednému adresáři
- **`files`** — pole `{name, path, bytes, skipped?, error?}`. `path` je absolutní cesta k uloženému souboru; `bytes` je velikost. `skipped: true` znamená, že soubor už v adresáři existoval a nepřepisuje se (předejte `overwrite=true` pro vynucený přepis). `error: "..."` na jedné položce znamená, že **ta jedna příloha** selhala; ostatní pokračovaly — Claude obvykle shrne, co se stáhlo a co ne.

### Bezpečnost a soukromí

- **Heslo** je uloženo v souboru `.env` (nebo bezpečněji v OS secret store — viz [Varianta: heslo v Keychainu](#varianta-heslo-v-keychainu)). Na **macOS / Linuxu** doporučená oprávnění jsou `0600` — to nastaví krok `chmod 600 .env` v sekci Konfigurace výše (běžné umask `022` by jinak vyrobilo `0644`, tj. soubor čitelný pro ostatní uživatele systému). Pokud používáte FileVault (zapnutý standardně na novějších Macích), je to dostačující ochrana proti odcizenému disku. Na **Windows** POSIX bity nefungují stejně — soubor je chráněn primárně přes ACL vašeho uživatelského profilu (`%USERPROFILE%`); pro citlivý disk používejte BitLocker.
- **Cookies** jsou v uživatelské cache (cesta výše). Na **macOS / Linuxu** se ukládají s oprávněními `0600` a na macOS jsou vyloučeny ze zálohy Time Machine / iCloud (patří do systémové cache). Na **Windows** Go neumí POSIX bity vynutit (`os.Chmod` je tam v podstatě no-op kromě read-only flagu) — ochranou je standardní ACL na `%LocalAppData%\edookit-mcp\`, který je čitelný jen pro vlastníka profilu. Více v sekci [otázek o šifrování](#proč-nejsou-cookies-šifrované) níže.
- **Žádné externí servery** ze strany konektoru — `edookit-mcp` komunikuje pouze mezi vaším počítačem, Edookitem a Plus4U. Žádný telemetrický kanál, žádné cloudové úložiště. Tělo zpráv (a vaše prompty) ovšem prochází přes poskytovatele vašeho AI asistenta — [Anthropic](https://www.anthropic.com/privacy) pro Claude, [OpenAI](https://openai.com/policies/privacy-policy/) pro ChatGPT, atd. — zpracovává je podle jejich vlastních pravidel. Pokud vám to nevyhovuje, použijte lokálně běžící model přes klienta typu Continue.dev / Goose s lokálním LLM.

#### Proč nejsou cookies šifrované?

XOR nebo podobné „obfuskace" by vám neposkytly žádnou skutečnou ochranu — útočník s přístupem k souborům má i přístup k binárce a tedy ke klíči. Skutečným bezpečnostním pásmem je FileVault (šifrování celého disku) a oprávnění souborů. Pokud chcete jít dál, heslo lze místo `.env` načítat z OS secret store (macOS Keychain / Linux libsecret) přes wrapper skript — viz následující sekce.

#### Varianta: heslo v Keychainu

Pokud nechcete mít heslo v plaintextu (ani v `.env`, ani v `env` bloku config JSON), použijte **wrapper skript**, který ho načte z OS secret store až při startu serveru — heslo se tak neobjeví v žádném souboru ani v process listingu (argv). Šablona je v repu: [`scripts/edookit-mcp-wrapper.sh.example`](scripts/edookit-mcp-wrapper.sh.example).

```bash
cp scripts/edookit-mcp-wrapper.sh.example ~/.local/bin/edookit-mcp-wrapper.sh
chmod +x ~/.local/bin/edookit-mcp-wrapper.sh
$EDITOR ~/.local/bin/edookit-mcp-wrapper.sh   # vyplňte URL, USER a cestu k binárce
```

Heslo uložte do secret store:

```bash
# macOS (Keychain)
security add-generic-password -a "$USER" -s edookit-mcp -w 'VAŠE_HESLO'

# Linux (libsecret / GNOME Keyring — balíček libsecret-tools)
secret-tool store --label="edookit-mcp" service edookit-mcp account "$USER"
```

Skript podporuje obě platformy — v šabloně odkomentujte příslušný řádek (`security` pro macOS, `secret-tool` pro Linux). Ve Windows tahle varianta nefunguje (skript je bash + `security`/`secret-tool`); tam zůstaňte u `.env` chráněného přes ACL profilu.

V MCP klientovi pak nastavte `command` na ten skript a **`env` blok úplně vynechte** (heslo i ostatní proměnné dodá wrapper):

```json
{
  "mcpServers": {
    "edookit": {
      "command": "/Users/vase-jmeno/.local/bin/edookit-mcp-wrapper.sh"
    }
  }
}
```

#### Co posíláte do cloudu AI poskytovatele (a co s tím dál)

> ⚠ **Tohle není právní rada.** Jsem inženýr, ne právník. Níže popisuji, **co se s daty technicky děje**, a **jaké možnosti** máte, pokud chcete riziko snížit. Pro konkrétní rozhodnutí (zvlášť ve školním nebo firemním kontextu) se zeptejte vašeho DPO / IT / právního.

`edookit-mcp` sám o sobě posílá data jen mezi vaším počítačem, Edookitem a Plus4U — žádný cloud, žádná telemetrie. Ale **AI asistent**, kterému tooly zpřístupníte, vidí výstup těchto toolů jako součást konverzace a **odesílá ho na servery svého poskytovatele**. To znamená:

- **`edookit_list_inbox` / `edookit_list_sent`** — Anthropic / OpenAI / GitHub (Copilot) atd. dostanou předměty, jména odesílatelů, prvních ~200 znaků těla a metadata zpráv.
- **`edookit_get_message`** — celé tělo zprávy + jména rodičů + jména recipientů + indikátory přečtení.
- **`edookit_download_attachments`** — samotné soubory se ukládají **lokálně na váš disk**, ale jejich **jména** (z JSON odpovědi) cloud vidí. Obsah souborů sám o sobě cloud nevidí (binárky neprojdou skrze MCP odpovědi — jen cesty), pokud je do konverzace explicitně neuploadnete.

V Edookit zprávách jsou **osobní údaje třetích stran** — jména jiných dětí, jejich rodičů, učitelů, někdy zdravotní nebo studijní detaily. Z pohledu GDPR jste vy v roli příjemce/zpracovatele těchto údajů (na základě svého vztahu ke škole) a předáváte je AI poskytovateli. Většina veřejných AI tarifů (free / Plus / Pro) má v podmínkách klauzule typu „inputs may be used to improve our models" pokud explicitně neopt-outujete. Pro citlivá data je to obvykle **nedostatečné**.

**Dva schůdné způsoby, jak to udělat slušně:**

##### 1. AI tarif s garancí soukromí (no-train, business / enterprise)

Většina poskytovatelů nabízí placené tarify, kde:
- inputs ani outputs nejsou použity pro trénování modelu (explicitně v ToS),
- existuje **Data Processing Agreement (DPA)** podle GDPR,
- často retence dat ≤ 30 dnů, s možností opt-out na nulu.

Konkrétní příklady (stav k roku 2026, **vždy si ověřte aktuální podmínky u poskytovatele**):
- **Anthropic** — Claude **Team** / **Enterprise** / **API přes Console** (pay-as-you-go) — no-train default, DPA na vyžádání, [Trust Center](https://trust.anthropic.com/).
- **OpenAI** — ChatGPT **Team** / **Enterprise** / **API** — no-train default pro Team+ a API, [Enterprise privacy](https://openai.com/enterprise-privacy/).
- **Google** — Gemini přes **Vertex AI** (placené, ne free Gemini app) — no-train, GDPR DPA.
- **Microsoft** — Copilot **for Business** / **Enterprise** — no-train, EU Data Boundary.

S takovým tarifem je riziko **srovnatelné** s tím, jak když posíláte e-mail přes Gmail Workspace nebo dokumenty přes Microsoft 365 — formálně podloženo DPA, technicky no-train, byť stále jde o cloud.

##### 2. Lokální LLM (data nikdy neopustí váš počítač)

Pokud chcete plnou kontrolu, MCP klienty lze provozovat s **lokálně běžícím modelem**. `edookit-mcp` mluví standardní MCP přes stdio, takže funguje s libovolným klientem, který tu kombinaci umí:

- **[Continue.dev](https://continue.dev/)** + **[Ollama](https://ollama.com/)** / **[LM Studio](https://lmstudio.ai/)** — VS Code / JetBrains extension, MCP support, žádný cloud LLM.
- **[Goose](https://block.github.io/goose/)** (od Block) — desktop agent s MCP, podpora lokálních modelů přes Ollama.
- **[Cursor](https://cursor.com/)** s lokálním Ollama endpointem (přes OpenAI-kompatibilní API).

Modely typu **Llama 3.3 70B**, **Qwen 2.5 32B** nebo **Gemma 2 27B** jsou na M-series Macu / herním PC s 32+ GB RAM/VRAM použitelné a zvládnou typické dotazy nad Edookit zprávami (česky včetně). Kvalita je o stupeň níž než Claude Opus / GPT-4 ale pro „kolik mám nepřečtených, od koho, o čem" plně stačí.

##### Free tarify — co se s nimi reálně děje

Pokud používáte **free Claude / ChatGPT / Gemini**:
- Vaše konverzace **mohou být** použity k trénování modelu (opt-out často skryté v Settings).
- Konverzace jsou ukládány na neurčito (možno smazat).
- GDPR DPA typicky není dostupné na free tier.

Pro **nízce citlivá data** (vaše vlastní rozvrhy, vaše nepřečtené zprávy bez detailů třetích stran) je to obvykle akceptovatelné riziko. Pro **citlivá data** (zprávy obsahující jména a info o dětech v třídě, zdravotní info, hodnocení) by **měl být na free tarifu opt-out** z trénování zapnutý, a i pak je rozumnější přesunout se na placený no-train tarif nebo lokální model.

##### Praktická doporučení

- **Pokud používáte projekt jako rodič** pro své vlastní účely (čtete si zprávy ze školy svého dítěte) → buď no-train placený tarif, nebo lokální model, nebo opt-out + smazání citlivých konverzací.
- **Pokud používáte projekt jako učitel** s přístupem k osobním datům studentů a jejich rodičů → **jen no-train tarif s DPA, nebo jen lokální model.** Free tarify bez opt-outu nejsou vhodné.
- **Pokud má škola GDPR / IT policy** — zeptejte se, jestli AI tooly s přístupem ke školní agendě má povolené. Některé školy mají Microsoft 365 / Google Workspace s explicitními DPAs; další zatím nemají rozhodnuto. Není v žádném případě bezpečné předpokládat „jistě to je v pohodě".

### Časté problémy

- **Chrome se otevírá, ale zůstane na úvodní stránce.** Pravděpodobně se změnil HTML layout Edookitu. Otevřete issue v GitHubu se snímkem obrazovky.
- **„login failed: ...interaction_required"** — váš účet v Plus4U nemusí mít aktivní session. Přihlaste se ručně do uuidentity.plus4u.net, pak zkuste znovu.
- **„session expired"** opakovaně. Smažte cache (`make clear-cookies`) a zkuste znovu. Pokud to nepomáhá, změnilo se chování Edookitu — issue v GitHubu.
- **Občasná „network error" nebo HTTP 502/503/504.** Tyhle blipy běžně chodí i z webového rozhraní Edookitu — MCP server je automaticky 2× opakuje s exponenciálním backoffem (~500 ms a 1 s), takže asistent o nich většinou neví. Pokud chybu vidíte i tak, znamená to, že selhaly všechny tři pokusy během ~1,5 s — buď je výpadek delší (počkejte chvíli a zkuste znovu), nebo se vrací deterministická chyba (HTTP 500, 4xx), která se z principu neopakuje.

---

## 🇬🇧 Technical reference

### Architecture

```
                                    ┌────────────────────────┐
                                    │  Plus4U OIDC provider  │
                                    │ uuidentity.plus4u.net  │
                                    └──────────┬─────────────┘
                                               │ (auth code flow)
┌──────────────┐  stdio MCP   ┌─────────────┐    │
│ AI assistant │ ◄──────────► │ edookit-mcp │ ◄──┴── chromium (chromedp)
│ (Claude /    │              │             │       only for login
│  ChatGPT /   │              │             │
│  Cursor / …) │              │             │
└──────────────┘              └──────┬──────┘
                                     │ net/http + cookie jar
                                     ▼
                            ┌────────────────────┐
                            │  Edookit backend   │
                            │  *.edookit.net     │
                            └────────────────────┘
```

`edookit-mcp` runs as a stdio MCP subprocess of whatever MCP-capable client the user has connected (Claude Code / Claude Desktop / ChatGPT / Cursor / VS Code agent mode / etc.). On startup it loads cached session cookies from `~/Library/Caches/edookit-mcp/cookies.json` if present. On first tool call it warms the session up with `GET /` (which the Edookit backend uses to resurrect a PHP session from the persistent auth tokens), then issues authenticated calls to the SPA's internal JSON API.

If no cookies exist (or they're stale), chromium is launched in the background to drive the full Plus4U OIDC code flow — username/password submission, redirect chain, callback — and the resulting session cookies are saved.

### Why a real browser for login

Edookit federates to Plus4U OIDC, which is **rendered by a uu5loader-driven SPA** with reCAPTCHA. The OIDC token endpoint requires `client_secret_basic` auth — the client secret only lives in Edookit's PHP backend, so ROPC (`grant_type=password`) is closed off. The login UI itself uses dynamic JS components rather than a static form. Cheapest reliable answer: drive a real chromium instance once per ~10 hours via [chromedp](https://github.com/chromedp/chromedp), then hand the session cookie off to a normal `net/http` client for all reads.

A specific quirk worth knowing: the OIDC client library hardcodes `prompt=none` (silent SSO) on its outgoing auth request, which fails with `interaction_required` for users without an active Plus4U session. The fetch-domain interceptor in `loginViaBrowser` strips `prompt=none` from outgoing requests when the `client_id` matches Edookit's — but leaves it alone for the IdM SPA's nested silent renewal (different `client_id`).

### Cookie persistence and the warmup

Edookit's backend rotates `PHPSESSID` on every response, and `/handler/page/*` paths return `authenticated:false` for any session ID the server didn't just issue. The reconciliation: the **persistent** auth tokens are `X-EdooAuthToken` and `X-Auth-Id`, set by the OIDC callback handler and stable for the session lifetime (~12 h). A `GET /` request with those cookies makes the server mint a fresh `PHPSESSID` tied to a valid PHP session.

`ensureLoggedIn` therefore always does a warmup `GET /` before declaring the client "logged in" — whether the cookies came from cache or a fresh chromedp run. The cookie jar transparently handles subsequent rotations.

### Transient-failure retries

Edookit's web frontend regularly surfaces HTTP 502/503/504 from its upstream proxy. The `Client.do` helper (the single chokepoint all three of `warmupSession`, `getJSON`, and `getDoc` route through) retries those plus net-level errors with exponential backoff so transient infrastructure noise doesn't bubble up to the MCP caller.

- **Retried:** net errors other than `context.Canceled` / `context.DeadlineExceeded`, and HTTP `408 / 502 / 503 / 504`.
- **Not retried:** HTTP `500 / 501 / 505+` (more likely deterministic application bugs than transient hiccups — retrying would mask them); all `4xx`; anything during context cancellation or deadline (that's caller intent).
- **Schedule:** `Config.MaxAttempts` (default 3 = 1 initial + 2 retries), `Config.RetryBaseDelay` (default 500 ms). Backoff doubles each retry — with the defaults the schedule is 500 ms then 1 s, so ~1.5 s worst case before propagating the failure. Set `MaxAttempts: 1` to disable retries entirely.
- **Safety:** the implementation assumes every request is idempotent. Currently every Edookit call we make is `GET`, so this holds. If a non-`GET` verb is ever added, the retry path needs gating on `req.Method`.
- **Race fence:** the cookie-jar snapshot from `c.do` is taken per-attempt, so a concurrent `invalidateSession` between retries lands the next attempt on the fresh jar (preserving the swap-race fix without re-introducing the stale-Set-Cookie problem).

### Data flow for `edookit_list_inbox` / `edookit_list_sent`

The Edookit SPA is a thin wrapper over `/handler/page/X` (page descriptors) and `/handler/grid/X-data` (row data). For messages:

- `/handler/page/objects-for-me` → grid descriptor (column schema, filters, total count).
- `/handler/grid/objects-for-me-data?object_type_general=object_type_message&object_filter=inbox&page=N` → 100 rows per page. Each row is `[uid, uid, html]` where the HTML blob carries date, sender, subject, attachments count, and body preview.
- `/handler/grid/created-objects-data?object_type_general=object_type_message&page=N` → same shape, sent messages, but the leading `<span>` holds the publication status instead of the sender.

The package `internal/tools` parses each row's HTML with goquery, extracts structured fields, and returns `ListResult` — a JSON object with `messages` (the parsed rows) and optional `parse_warnings` (one entry per row the server returned that we couldn't parse). When the server returned rows but every one failed to parse, `fetchAndParse` returns an error rather than a silent empty `messages` array. Pagination, optional fulltext (`?fulltext=`), and a client-side `since` date floor are all implemented in `fetchAndParse`.

### Data flow for `edookit_get_message` / `edookit_download_attachments`

Edookit serves the full message body and attachment list from a single shared endpoint:

- `/handler/page/message-edit?__index=N` → JSON whose `components.workspace[]` array contains both the message form (`DOMTarget="__lc_Form_Message"`) and the attachment list (`DOMTarget="__lc_Fileviewer_Slave_datatemplate_message"`). The endpoint is the same for received and sent messages — only the embedded `object_status` HTML differs slightly (received messages carry an extra `Od DD.MM.YYYY HH:MM` inline date).

The form panel exposes:

- `__form_panel_main[*]` is an array of labeled sub-panels. The parser locates required fields by `items[].name`:
  - `name` → `subject` (subject line, plain text)
  - `object_status` → HTML carrying `status` word ("Publikováno" / "Nepublikováno"), `author` (bold-span text), and optionally a parseable `date` (`Od …` line on received messages)
  - `description__editor` → `readValue` is the rendered body HTML; `val` carries the same content double-encoded for form-submit. We use `readValue`.

The fileviewer panel exposes:

- `data[]` array of `{id, name, link, date, trashed}`. Trashed entries are filtered out. `link` is a fully-qualified `https://<host>/handler/download/file<uuid>` URL that the authenticated session can GET directly.

`GetMessage` (in `internal/tools/message.go`) does the JSON fetch + parse and returns a `FullMessage`. `DownloadAttachments` (in `internal/tools/attachments.go`) calls `GetMessage` then streams each attachment URL via `client.GetTo` into `<dest>/<filename>`. Path-traversal is neutralized by taking `filepath.Base` of the server-supplied name; existing files are skipped unless the caller passes `overwrite=true`; partial-write failures clean up the partial file. Per-attachment errors don't abort the loop — each attachment's outcome is captured in the result entry so a single broken file doesn't lose the rest.

### Project layout

| Path | Purpose |
|---|---|
| `main.go` | Flag parsing, MCP server bootstrap, tool registration |
| `internal/client/client.go` | Session-aware HTTP client (`GetJSON`, `GetDoc`, `GetTo` for binary downloads, warmup, cookie cache, retry on transient failures) |
| `internal/client/login_chromedp.go` | OIDC login via chromedp (Plus4U landing page → fetch interception → form submission → callback) |
| `internal/client/cookie_store.go` | On-disk cookie persistence (`~/Library/Caches/edookit-mcp/cookies.json`) |
| `internal/tools/messages.go` | `ListInbox` / `ListSent` — paginated list of inbox / sent rows with HTML parsing |
| `internal/tools/message.go` | `GetMessage` — full body + attachment metadata for one message (shared inbox/sent endpoint) |
| `internal/tools/attachments.go` | `DownloadAttachments` — streams each attachment URL into a destination dir |
| `internal/tools/*_test.go` | Unit + integration tests (~89% coverage on `internal/tools`; ~46% on `internal/client`, excluding the chromedp login path) |
| `.goreleaser.yaml` | Cross-platform build matrix + Homebrew formula config |
| `.github/workflows/ci.yml` | Runs lint / vet / `go test -race` / govulncheck on every push to `main`/`develop` and on every PR |
| `.github/workflows/release.yml` | Runs the same checks as a gate, then GoReleaser, on `v*` tag push |

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
make smoke-message MSG=m-N # (dev) dump raw /handler/page/message-edit JSON for ID N; used to reverse-engineer endpoint shapes
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

`edookit-mcp` is per-tenant by construction:

1. `EDOOKIT_URL` is env-driven — put your own school's Edookit URL in `.env`.
2. The Plus4U OIDC `client_id` is extracted at runtime from the landing page's embedded `UU5.Environment.uu_app_oidc_providers_oidcg02_client_id` literal during login. The fetch interceptor that strips the lib's hardcoded `prompt=none` only fires for requests carrying this captured ID, so any other school's tenant works out of the box without code changes. (If the extraction step fails — i.e. the landing page no longer exposes that field — login aborts with a clear error rather than silently misbehaving.)

### Distribution and releases

Releases are cut by pushing an annotated git tag with a `v` prefix:

```bash
git tag -a v0.2.0 -m "Release v0.2.0"
git push origin v0.2.0
```

The [`release` workflow](.github/workflows/release.yml) runs in two jobs:

**Job 1 — `check` (gate):** identical to the `ci` workflow — `go vet`, `golangci-lint run`, `go test -race`, `govulncheck`. If any step fails the release is aborted; no public artifact is ever produced from a tag that doesn't pass the same checks developers run locally.

**Job 2 — `goreleaser`** (runs only after `check` succeeds, via `needs: check`): invokes [GoReleaser](https://goreleaser.com/) (config in [`.goreleaser.yaml`](.goreleaser.yaml)), which:

1. Cross-compiles for `darwin/{amd64,arm64}`, `linux/{amd64,arm64}`, `windows/amd64`.
2. Packages each binary into a `.tar.gz` (Unix) or `.zip` (Windows) along with `README.md`, `LICENSE`, and `.env.example`.
3. Generates `checksums.txt` (SHA-256 for every archive).
4. Creates a GitHub Release with auto-generated changelog (commits since previous tag, filtered to exclude `docs:`, `test:`, `ci:`, `chore:`).
5. *(Optional)* Pushes a Homebrew formula to `dsaiko/homebrew-tap` if the `HOMEBREW_TAP_GITHUB_TOKEN` repo secret is set; otherwise the brew step is silently skipped.

#### Enabling the Homebrew tap (one-time setup)

The tap repo `dsaiko/homebrew-tap` is created and empty. For GoReleaser to push the formula into it on every release, a personal access token with write access to the tap repo is needed.

1. Generate a fine-grained PAT at https://github.com/settings/tokens?type=beta:
   - **Resource owner**: dsaiko
   - **Repository access**: Only select repositories → `homebrew-tap`
   - **Repository permissions**: Contents → Read and write, Metadata → Read-only
   - **Expiration**: 90 days or longer
2. Copy the generated token (`github_pat_...`).
3. Add it as a repository secret in this repo: Settings → Secrets and variables → Actions → New repository secret. Name: `HOMEBREW_TAP_GITHUB_TOKEN`. Value: the token.
4. Re-tag (or trigger the workflow manually); the next release will push `Formula/edookit-mcp.rb` to the tap.

Once the formula is in the tap, users install via `brew install dsaiko/tap/edookit-mcp` and `brew upgrade edookit-mcp` for updates.

#### Code signing

Binaries are not currently signed. macOS users hit Gatekeeper on first run; Windows users hit SmartScreen. The README's user section documents the one-time bypass. Real notarization requires an Apple Developer account ($99/year) and is not justified for a hobby tool.

### License

[MIT](LICENSE) © 2026 Dušan Saiko
