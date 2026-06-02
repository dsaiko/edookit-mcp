//! MCP Apps UI for the inbox listing (SEP-1865, extension
//! `io.modelcontextprotocol/ui`).
//!
//! This implements the **MCP Apps** model (the standardized successor to the
//! community MCP-UI), *not* the older inline-embedded-resource approach:
//!
//!   * The UI is a **predeclared, static** HTML template served via
//!     `resources/read` at [`INBOX_UI_URI`] with mimeType [`UI_MIME`]. It
//!     contains no per-call data.
//!   * `edookit_list_inbox` links to it by carrying `_meta.ui.resourceUri`
//!     ([`inbox_tool_meta`]) on its tool definition, and delivers the inbox rows
//!     as the tool result's `structuredContent`.
//!   * The host renders the template in a sandboxed iframe; the template and the
//!     host speak the **MCP JSON-RPC base protocol over `postMessage`**
//!     ([`PROTOCOL_JS`]): the view sends `ui/initialize`, the host pushes the
//!     data via `ui/notifications/tool-result`, and a row click issues a
//!     `tools/call` for `edookit_get_message` (click → detail).
//!
//! All of this is gated by `EDOOKIT_UI_RESOURCES` (see [`crate::server`]); the
//! plain text/JSON output is unaffected and stays the model-facing source of
//! truth.
//!
//! ## Security
//!
//! Edookit-derived fields are third-party-controlled (see [`super::untrusted`]).
//! The template renders rows **client-side using DOM APIs (`textContent` /
//! `createElement`)** — never `innerHTML` — so a hostile subject/sender cannot
//! become markup; XSS-safety holds by construction rather than by escaping. The
//! template is static and predeclared, so the host can review it before
//! rendering, and a row click only ever passes back an opaque message `id`.

use std::collections::BTreeMap;

use rmcp::model::{
    AnnotateAble, ExtensionCapabilities, Meta, RawResource, Resource, ResourceContents,
};

use super::messages::ListResult;

/// MCP Apps resource URI for the inbox template.
pub const INBOX_UI_URI: &str = "ui://edookit/inbox";

/// MCP Apps HTML profile mimeType (SEP-1865 MVP).
pub const UI_MIME: &str = "text/html;profile=mcp-app";

/// The MCP Apps extension identifier negotiated at `initialize`.
pub const UI_EXTENSION: &str = "io.modelcontextprotocol/ui";

const STYLE: &str = r#"
*{box-sizing:border-box}
body{margin:0;font:14px/1.4 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;
  color:#1a1a1a;background:#fff}
h1{font-size:15px;font-weight:600;margin:0;padding:12px 16px;border-bottom:1px solid #e5e5e5;
  color:#444;position:sticky;top:0;background:#fff}
ul{list-style:none;margin:0;padding:0}
li{padding:10px 16px;border-bottom:1px solid #f0f0f0;cursor:pointer;outline:none}
li:hover,li:focus{background:#f5f8ff}
li:focus{box-shadow:inset 2px 0 0 #2563eb}
.top{display:flex;justify-content:space-between;gap:8px;margin-bottom:2px}
.sender{font-weight:600;color:#111;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.date{color:#888;font-size:12px;white-space:nowrap;flex-shrink:0}
.subject{color:#222;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.preview{color:#888;font-size:13px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.clip{color:#2563eb;font-size:12px;margin-left:6px}
.empty{padding:24px 16px;color:#888;text-align:center}
"#;

/// The MCP Apps iframe ↔ host bridge. Dependency-free: speaks the MCP JSON-RPC
/// base protocol over `postMessage`, renders rows from the `tool-result`
/// notification's `structuredContent`, and issues a `tools/call` on click. All
/// third-party text reaches the DOM only via `textContent`.
const PROTOCOL_JS: &str = r#"
(function(){
  var PROTOCOL='2025-06-18';
  var initId=null, nextId=1, ready=false;
  function post(m){ try{ (window.parent||window).postMessage(m,'*'); }catch(e){} }
  function req(method,params){ var id=nextId++; post({jsonrpc:'2.0',id:id,method:method,params:params||{}}); return id; }
  function note(method,params){ post({jsonrpc:'2.0',method:method,params:params||{}}); }

  function openMessage(id){ if(id) req('tools/call',{name:'edookit_get_message',arguments:{id:String(id)}}); }

  function render(data){
    data = data || {};
    var msgs = Array.isArray(data.messages) ? data.messages : [];
    var root = document.getElementById('root');
    while(root.firstChild) root.removeChild(root.firstChild);

    var h = document.createElement('h1');
    h.textContent = 'Přijaté zprávy (' + msgs.length + ')';
    root.appendChild(h);

    if(!msgs.length){
      var e = document.createElement('div'); e.className='empty';
      e.textContent='Žádné zprávy.'; root.appendChild(e); return;
    }
    var ul = document.createElement('ul');
    msgs.forEach(function(m){
      var li = document.createElement('li');
      li.tabIndex = 0; li.setAttribute('role','button');

      var top = document.createElement('div'); top.className='top';
      var s = document.createElement('span'); s.className='sender';
      s.textContent = m.sender || m.status || '';
      var d = document.createElement('span'); d.className='date';
      d.textContent = m.date || '';
      top.appendChild(s); top.appendChild(d); li.appendChild(top);

      var subj = document.createElement('div'); subj.className='subject';
      subj.textContent = m.subject || '(bez předmětu)';
      if(m.attachments > 0){
        var c = document.createElement('span'); c.className='clip';
        c.textContent = ' 📎 ' + m.attachments; subj.appendChild(c);
      }
      li.appendChild(subj);

      if(m.body_preview){
        var p = document.createElement('div'); p.className='preview';
        p.textContent = m.body_preview; li.appendChild(p);
      }
      li.addEventListener('click', function(){ openMessage(m.id); });
      li.addEventListener('keydown', function(ev){
        if(ev.key==='Enter'||ev.key===' '){ ev.preventDefault(); openMessage(m.id); }
      });
      ul.appendChild(li);
    });
    root.appendChild(ul);
  }

  window.addEventListener('message', function(ev){
    var msg = ev.data;
    if(!msg || msg.jsonrpc !== '2.0') return;
    if(msg.id != null && msg.id === initId && msg.result){
      if(!ready){ ready = true; note('ui/notifications/initialized',{}); }
      return;
    }
    if(msg.method === 'ui/notifications/tool-result'){
      render(msg.params && msg.params.structuredContent);
    }
  });

  initId = req('ui/initialize', {
    capabilities:{}, clientInfo:{name:'edookit-inbox', version:'1'},
    protocolVersion:PROTOCOL, appCapabilities:{availableDisplayModes:['inline']}
  });
})();
"#;

/// Assembles the static template document. Pure constant — no per-call data.
fn inbox_template_html() -> String {
    format!(
        r#"<!doctype html>
<html lang="cs"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>{style}</style></head>
<body><div id="root"></div><script>{script}</script></body></html>"#,
        style = STYLE,
        script = PROTOCOL_JS,
    )
}

/// Resource descriptor for `resources/list`.
pub fn inbox_resource_descriptor() -> Resource {
    let mut raw = RawResource::new(INBOX_UI_URI, "Edookit – přijaté zprávy");
    raw.mime_type = Some(UI_MIME.to_string());
    raw.description =
        Some("Interactive inbox list (MCP Apps UI); click a row to open the message.".to_string());
    raw.no_annotation()
}

/// Resource contents for `resources/read` of [`INBOX_UI_URI`].
pub fn inbox_template_contents() -> ResourceContents {
    ResourceContents::TextResourceContents {
        uri: INBOX_UI_URI.to_string(),
        mime_type: Some(UI_MIME.to_string()),
        text: inbox_template_html(),
        meta: None,
    }
}

/// `_meta` linking `edookit_list_inbox` to its UI template — the nested
/// `_meta.ui.resourceUri` form (the flat `ui/resourceUri` is deprecated).
pub fn inbox_tool_meta() -> Meta {
    let obj = serde_json::json!({
        "ui": { "resourceUri": INBOX_UI_URI, "visibility": ["model", "app"] }
    });
    Meta(obj.as_object().expect("object literal").clone())
}

/// The server-side extension capability advertised at `initialize`.
pub fn ui_extensions() -> ExtensionCapabilities {
    let settings = serde_json::json!({ "mimeTypes": [UI_MIME] });
    let mut map: ExtensionCapabilities = BTreeMap::new();
    map.insert(
        UI_EXTENSION.to_string(),
        settings.as_object().expect("object literal").clone(),
    );
    map
}

/// The inbox data delivered to the view as the tool result's
/// `structuredContent`. The view renders rows from `.messages`.
pub fn inbox_structured_content(result: &ListResult) -> serde_json::Value {
    serde_json::to_value(result).unwrap_or_else(|_| serde_json::json!({ "messages": [] }))
}

/// Wraps the template with a tiny dev mock host (handshake + sample
/// `tool-result`) so `--preview-ui` renders a populated list in a plain
/// browser, with no MCP host. The mock is **never** part of the served
/// resource — only this preview output.
pub fn render_preview_html() -> String {
    // Sample data, including a hostile-looking subject/sender, to demonstrate
    // that client-side textContent rendering neutralizes third-party markup.
    let sample = serde_json::json!({
        "messages": [
            {"id":"m-290491","date":"2026-05-21T12:31:00+02:00","sender":"Nováková Eva (učitel 4SC)",
             "subject":"Pozvánka na třídní schůzky","body_preview":"Dobrý den, zveme Vás na třídní schůzky ve čtvrtek 28. 5. od 17:00…","attachments":2},
            {"id":"m-290488","date":"2026-05-20T08:05:00+02:00","sender":"Ředitelství školy",
             "subject":"Uzavření školy — státní svátek","body_preview":"V pondělí bude škola uzavřena.","attachments":0},
            {"id":"m-290470","date":"2026-05-19T15:40:00+02:00","sender":"<script>alert('xss')</script>",
             "subject":"Subject \"with\" <b>markup</b>","body_preview":"Obsah od třetí strany je vykreslen jako text.","attachments":1}
        ]
    });
    let mock = format!(
        r#"<script>
// DEV-ONLY mock host (not part of the served ui:// resource).
(function(){{
  var DATA = {sample};
  window.addEventListener('message', function(ev){{
    var m = ev.data; if(!m || m.jsonrpc !== '2.0') return;
    if(m.method === 'ui/initialize'){{
      window.postMessage({{jsonrpc:'2.0',id:m.id,result:{{protocolVersion:'2025-06-18',hostInfo:{{name:'preview',version:'1'}},hostCapabilities:{{}}}}}},'*');
    }} else if(m.method === 'ui/notifications/initialized'){{
      window.postMessage({{jsonrpc:'2.0',method:'ui/notifications/tool-result',params:{{structuredContent:DATA}}}},'*');
    }} else if(m.method === 'tools/call'){{
      console.log('[preview] tools/call', JSON.stringify(m.params));
    }}
  }});
}})();
</script>"#,
        sample = sample,
    );
    inbox_template_html().replace("</body>", &format!("{mock}</body>"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tools::messages::Message;

    #[test]
    fn resource_descriptor_uri_and_mime() {
        let r = inbox_resource_descriptor();
        assert_eq!(r.raw.uri, INBOX_UI_URI);
        assert_eq!(r.raw.mime_type.as_deref(), Some(UI_MIME));
    }

    #[test]
    fn template_contents_carry_mcp_app_profile() {
        match inbox_template_contents() {
            ResourceContents::TextResourceContents {
                uri,
                mime_type,
                text,
                ..
            } => {
                assert_eq!(uri, INBOX_UI_URI);
                assert_eq!(mime_type.as_deref(), Some("text/html;profile=mcp-app"));
                assert!(text.contains("<!doctype html>"));
            }
            _ => panic!("expected text resource"),
        }
    }

    #[test]
    fn template_speaks_mcp_apps_jsonrpc_protocol() {
        let html = inbox_template_html();
        // Lifecycle + the click → detail call must all be present.
        assert!(html.contains("ui/initialize"));
        assert!(html.contains("ui/notifications/initialized"));
        assert!(html.contains("ui/notifications/tool-result"));
        assert!(html.contains("tools/call"));
        assert!(html.contains("edookit_get_message"));
        // structuredContent is the data channel.
        assert!(html.contains("structuredContent"));
    }

    #[test]
    fn template_is_xss_safe_by_construction() {
        // It must render via textContent, never innerHTML — that's the whole
        // security premise now that data is injected client-side at runtime.
        let html = inbox_template_html();
        assert!(html.contains("textContent"));
        assert!(
            !html.contains("innerHTML"),
            "template must not use innerHTML with runtime data"
        );
    }

    #[test]
    fn tool_meta_links_resource_uri_nested() {
        let meta = inbox_tool_meta();
        let v = serde_json::to_value(&meta).unwrap();
        assert_eq!(v["ui"]["resourceUri"], INBOX_UI_URI);
        assert_eq!(v["ui"]["visibility"][0], "model");
    }

    #[test]
    fn extensions_advertise_ui_with_mime() {
        let ext = ui_extensions();
        let entry = ext.get(UI_EXTENSION).expect("ui extension present");
        let v = serde_json::to_value(entry).unwrap();
        assert_eq!(v["mimeTypes"][0], "text/html;profile=mcp-app");
    }

    #[test]
    fn structured_content_serializes_messages() {
        let result = ListResult {
            messages: vec![Message {
                id: "m-1".into(),
                subject: "Ahoj".into(),
                sender: "Učitel".into(),
                date: "2026-05-21T12:31:00+02:00".into(),
                ..Default::default()
            }],
            ..Default::default()
        };
        let v = inbox_structured_content(&result);
        assert_eq!(v["messages"][0]["id"], "m-1");
        assert_eq!(v["messages"][0]["subject"], "Ahoj");
    }
}
