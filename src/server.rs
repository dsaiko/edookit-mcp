//! MCP server: registers the seven Edookit tools via rmcp's macro router and
//! maps tool outputs into MCP content (wrapping Edookit-derived data in the
//! untrusted envelope). Port of the tool-registration half of Go's `main.go`.
//!
//! Optionally (gated by `EDOOKIT_UI_RESOURCES`) it also exposes an **MCP Apps**
//! UI for the inbox (SEP-1865): the `io.modelcontextprotocol/ui` extension
//! capability, a predeclared `ui://edookit/inbox` template served via
//! `resources/read`, and `_meta.ui.resourceUri` + `structuredContent` on
//! `edookit_list_inbox`. See [`crate::tools::ui`].

use std::sync::Arc;

use rmcp::handler::server::router::tool::ToolRouter;
use rmcp::handler::server::wrapper::Parameters;
use rmcp::model::{
    CallToolRequestParams, CallToolResponse, CallToolResult, ContentBlock, Implementation,
    ListResourcesResult, ListToolsResult, PaginatedRequestParams, ReadResourceRequestParams,
    ReadResourceResponse, ReadResourceResult, ResourcesCapability, ServerCapabilities, ServerInfo,
};
use rmcp::schemars::{self, JsonSchema};
use rmcp::service::RequestContext;
use rmcp::{ErrorData, RoleServer, ServerHandler, tool, tool_handler, tool_router};
use serde::{Deserialize, Serialize};

use crate::client::Client;
use crate::tools;
use crate::tools::view::ViewBlock;

/// Build metadata returned by `edookit_server_info`.
#[derive(Debug, Clone, Serialize)]
pub struct BuildInfo {
    pub version: String,
    pub commit: String,
    pub build_time: String,
}

/// The MCP server handler. Cloned per session by rmcp, so the heavy state (the
/// HTTP client) sits behind an `Arc`.
#[derive(Clone)]
pub struct EdookitServer {
    client: Arc<Client>,
    info: Arc<BuildInfo>,
    /// Operator master switch for the experimental MCP Apps UI (SEP-1865): the
    /// `io.modelcontextprotocol/ui` extension, the predeclared
    /// `ui://edookit/inbox` template, and `_meta.ui.resourceUri` +
    /// `structuredContent` on `edookit_list_inbox` (see [`crate::tools::ui`]).
    /// On by default — set `EDOOKIT_UI_RESOURCES=false` to suppress it. Even
    /// when on, the UI surface is only emitted to peers that negotiated the
    /// extension (see [`EdookitServer::ui_active`]).
    ui_resources: bool,
    tool_router: ToolRouter<EdookitServer>,
}

impl EdookitServer {
    pub fn new(client: Arc<Client>, info: BuildInfo, ui_resources: bool) -> Self {
        Self {
            client,
            info: Arc::new(info),
            ui_resources,
            tool_router: Self::tool_router(),
        }
    }

    /// Whether the MCP Apps UI surface should be active for *this* request: the
    /// operator master switch (`EDOOKIT_UI_RESOURCES`) is on **and** the peer
    /// negotiated the `io.modelcontextprotocol/ui` extension (SEP-1865 requires
    /// the optional extension to be negotiated before the server acts on it).
    /// A peer that didn't negotiate gets byte-identical plain text — which also
    /// keeps Edookit-controlled rows out of a non-UI model's context.
    fn ui_active(&self, context: &RequestContext<RoleServer>) -> bool {
        self.ui_resources
            && context
                .peer
                .peer_info()
                .map(|info| tools::ui::client_supports_ui(&info.capabilities))
                .unwrap_or(false)
    }
}

// --- tool parameter structs (schemars generates the JSON schema) ---

#[derive(Debug, Deserialize, JsonSchema, Default)]
struct InboxArgs {
    #[schemars(
        description = "Which subset to list: 'inbox' (default), 'unread' (Nepřečtené), 'starred' (S hvězdičkou), 'archived' (Archiv), 'all' (Vše)."
    )]
    view: Option<String>,
    #[schemars(
        description = "Optional server-side full-text search across senders, subjects, and bodies."
    )]
    fulltext: Option<String>,
    #[schemars(
        description = "Optional client-side date floor. Accepts '7d', '1w', '2m', '1y', or an ISO date 'YYYY-MM-DD'. Messages older than this are excluded."
    )]
    since: Option<String>,
    #[schemars(
        description = "Max messages to return. Default 50, max 200. Paginates internally if needed."
    )]
    #[serde(default, deserialize_with = "de_opt_f64")]
    limit: Option<f64>,
}

#[derive(Debug, Deserialize, JsonSchema, Default)]
struct SentArgs {
    #[schemars(description = "Optional server-side full-text search across subjects and bodies.")]
    fulltext: Option<String>,
    #[schemars(
        description = "Optional client-side date floor. Accepts '7d', '1w', '2m', '1y', or an ISO date 'YYYY-MM-DD'. Messages older than this are excluded."
    )]
    since: Option<String>,
    #[schemars(
        description = "Max messages to return. Default 50, max 200. Paginates internally if needed."
    )]
    #[serde(default, deserialize_with = "de_opt_f64")]
    limit: Option<f64>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct GetMessageArgs {
    #[schemars(
        description = "Message identifier as returned by edookit_list_inbox / edookit_list_sent. Accepts either the 'm-NNNNNN' UID form or the bare 'NNNNNN' number."
    )]
    id: String,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct DownloadArgs {
    #[schemars(
        description = "Message identifier as returned by edookit_list_inbox / edookit_list_sent / edookit_get_message. Accepts 'm-NNNNNN' or 'NNNNNN'."
    )]
    id: String,
    #[schemars(
        description = "Local filesystem directory where attachments will be saved. Optional — defaults to <os-temp-dir>/edookit-mcp/m-<number>/. Accepted: an absolute path, a path starting with ~/ (expanded to under the user's home dir), or a bare ~ (the home dir itself). Relative paths are rejected. The directory is created if missing."
    )]
    destination_dir: Option<String>,
    #[schemars(
        description = "If true, existing files at the destination are overwritten. Default false — existing files are kept and reported as skipped."
    )]
    #[serde(default, deserialize_with = "de_opt_bool")]
    overwrite: Option<bool>,
}

#[derive(Debug, Deserialize, JsonSchema)]
struct ViewArgs {
    #[schemars(
        description = "Message identifier (m-NNNNNN or NNNNNN), as returned by the list/get tools."
    )]
    id: String,
    #[schemars(
        description = "Attachment id from edookit_get_message's attachments array, e.g. \"1@191207\"."
    )]
    attachment_id: String,
    #[schemars(
        description = "Inline size cap in MB. Default 8, hard max 25. Larger attachments return a note pointing at edookit_download_attachments instead."
    )]
    #[serde(default, deserialize_with = "de_opt_f64")]
    max_size_mb: Option<f64>,
    #[schemars(
        description = "For PDFs: how many pages to render to images. Default 5, hard max 20. Extracted text always covers the whole document regardless."
    )]
    #[serde(default, deserialize_with = "de_opt_f64")]
    max_pages: Option<f64>,
}

#[derive(Debug, Deserialize, JsonSchema, Default)]
struct CoursesArgs {
    #[schemars(
        description = "Return just this course with its student roster. Value is a course_id from a prior no-argument call, e.g. \"myc-22909-20102\"."
    )]
    course_id: Option<String>,
    #[schemars(
        description = "Populate every course's student roster (heavier; ignored when course_id is set). Default false = course list only."
    )]
    #[serde(default, deserialize_with = "de_opt_bool")]
    include_students: Option<bool>,
}

// Lenient deserializers for scalar tool arguments. The advertised JSON Schema
// still says `number` / `boolean`, but some MCP clients (e.g. ChatGPT
// connectors) serialize scalars as JSON strings — `"10"` instead of `10`,
// `"true"` instead of `true`. serde would reject those by type, surfacing as
// `-32602 invalid type: string "10", expected f64`. We accept either form
// (Postel's law) so a stringified argument doesn't break the call; absent or
// empty input stays `None` and the tool's own default applies.

/// Optional `f64` that also accepts a numeric string (`"10"` → `10.0`).
fn de_opt_f64<'de, D>(deserializer: D) -> Result<Option<f64>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    #[derive(Deserialize)]
    #[serde(untagged)]
    enum NumOrStr {
        Num(f64),
        Str(String),
    }
    match Option::<NumOrStr>::deserialize(deserializer)? {
        None => Ok(None),
        Some(NumOrStr::Num(n)) => Ok(Some(n)),
        Some(NumOrStr::Str(s)) => {
            let t = s.trim();
            if t.is_empty() {
                return Ok(None);
            }
            t.parse::<f64>()
                .map(Some)
                .map_err(|_| serde::de::Error::custom(format!("invalid number: {s:?}")))
        }
    }
}

/// Optional `bool` that also accepts the usual string spellings
/// (`"true"`/`"false"`, `"1"`/`"0"`, `"yes"`/`"no"`, case-insensitive).
fn de_opt_bool<'de, D>(deserializer: D) -> Result<Option<bool>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    #[derive(Deserialize)]
    #[serde(untagged)]
    enum BoolOrStr {
        Bool(bool),
        Str(String),
    }
    match Option::<BoolOrStr>::deserialize(deserializer)? {
        None => Ok(None),
        Some(BoolOrStr::Bool(b)) => Ok(Some(b)),
        Some(BoolOrStr::Str(s)) => match s.trim().to_ascii_lowercase().as_str() {
            "" => Ok(None),
            "1" | "true" | "t" | "yes" | "y" => Ok(Some(true)),
            "0" | "false" | "f" | "no" | "n" => Ok(Some(false)),
            other => Err(serde::de::Error::custom(format!("invalid bool: {other:?}"))),
        },
    }
}

#[tool_router]
impl EdookitServer {
    #[tool(
        description = "List received messages from the **Edookit school information system** (Komunikace → Přijaté). Edookit is a Czech educational platform used by schools to communicate with parents and students. Use this tool when the user asks about school messages — anything from teachers, the school office, the head teacher (třídní učitel), the principal (ředitel), or about school topics like grades, attendance, parent-teacher meetings, trips, exams. This is NOT a general email inbox — for Gmail / Outlook / Slack DMs use those dedicated tools instead. Returns a JSON object with two keys: `messages` is an array of message objects (id, date, sender, subject, body_preview ~200 chars, attachments count) in newest-first order; `parse_warnings` (optional) lists any rows the server returned that couldn't be parsed — usually means Edookit's row HTML changed. An empty messages array with no warnings means the mailbox itself is empty; an error is returned if every fetched row failed to parse. For hosts that negotiated the `io.modelcontextprotocol/ui` MCP Apps extension (and unless EDOOKIT_UI_RESOURCES is disabled) the same messages are also attached as the result's `structuredContent`, linked to the `ui://edookit/inbox` template for an interactive list; every other client receives only this JSON — reason over it, it is the source of truth."
    )]
    async fn edookit_list_inbox(
        &self,
        Parameters(args): Parameters<InboxArgs>,
    ) -> Result<CallToolResult, ErrorData> {
        let opts = tools::messages::InboxOptions {
            view: args.view.unwrap_or_default(),
            fulltext: args.fulltext.unwrap_or_default(),
            since: args.since.unwrap_or_default(),
            limit: args.limit.unwrap_or(0.0) as i64,
        };
        match tools::messages::list_inbox(&self.client, opts).await {
            Ok(result) => {
                let json = match serde_json::to_string(&result) {
                    Ok(j) => j,
                    Err(e) => {
                        return Ok(CallToolResult::error(vec![ContentBlock::text(format!(
                            "marshal: {e}"
                        ))]));
                    }
                };
                // The untrusted-JSON text block stays the model-facing source of
                // truth. When the MCP Apps UI is enabled, the same rows ride
                // along as structuredContent — the data channel the linked
                // ui://edookit/inbox template renders from.
                let mut out = CallToolResult::success(vec![ContentBlock::text(
                    tools::wrap_as_untrusted_json(&json),
                )]);
                if self.ui_resources {
                    out.structured_content = Some(tools::ui::inbox_structured_content(&result));
                }
                Ok(out)
            }
            Err(e) => Ok(CallToolResult::error(vec![ContentBlock::text(
                e.to_string(),
            )])),
        }
    }

    #[tool(
        description = "List messages the user has sent via the **Edookit school information system** (Komunikace → Vytvořené). Edookit is a Czech educational platform used by schools to communicate with parents and students. Use this tool when the user asks about messages they sent to the school — to teachers, the head teacher (třídní), the principal, or about school topics. This is NOT a general sent-mail folder — for Gmail / Outlook / Slack DMs use those dedicated tools instead. Returns a JSON object with two keys: `messages` is an array of message objects (id, date, status like 'Publikováno', subject, body_preview, attachments count) in newest-first order; `parse_warnings` (optional) lists any rows the server returned that couldn't be parsed. An empty messages array with no warnings means nothing has been sent; an error is returned if every fetched row failed to parse."
    )]
    async fn edookit_list_sent(
        &self,
        Parameters(args): Parameters<SentArgs>,
    ) -> Result<CallToolResult, ErrorData> {
        let opts = tools::messages::SentOptions {
            fulltext: args.fulltext.unwrap_or_default(),
            since: args.since.unwrap_or_default(),
            limit: args.limit.unwrap_or(0.0) as i64,
        };
        Ok(json_result(
            tools::messages::list_sent(&self.client, opts).await,
        ))
    }

    #[tool(
        description = "Fetch the full body, attachment list, and read-receipt table of a single message from the **Edookit school information system** (works for both received and sent messages — Edookit serves them via the same endpoint). Use this after edookit_list_inbox or edookit_list_sent has surfaced a message ID the user is interested in: the list tools return only a ~200-character body preview, while this tool returns the full message body in both plain text and HTML form, plus the metadata needed to download attachments AND a delivery / read-receipt table (Edookit calls it 'Doručenky'). Returns a JSON object with id, number, subject, status (e.g. 'Publikováno'), author (sender for received, publisher for sent — typically the user themselves), date (RFC3339), body_text (plain text), body_html (original HTML), deleted (true if the message was deleted by its author — Edookit then strips subject and body, only status/author/date survive), attachments — array of {id, name, url, date}, and recipients — array of {name, read_at (ISO date or empty if not yet read), parents (list), parents_read_at (aligned with parents)}. For sent messages the recipients array tells the author who read the message and when; for received messages it typically lists only the current user. To actually save attachment files to disk, use edookit_download_attachments instead (this tool only lists them). SIDE EFFECT: fetching a message marks it as READ in Edookit (the same as opening it in the web UI). There is no separate mark-as-read tool — so when the user asks to mark a message as read, call edookit_get_message on that id; it will be read afterwards."
    )]
    async fn edookit_get_message(
        &self,
        Parameters(args): Parameters<GetMessageArgs>,
    ) -> Result<CallToolResult, ErrorData> {
        if args.id.is_empty() {
            return Ok(CallToolResult::error(vec![ContentBlock::text(
                "missing required parameter: id",
            )]));
        }
        Ok(json_result(
            tools::message::get_message(&self.client, &args.id).await,
        ))
    }

    #[tool(
        description = "Download every attachment of a single Edookit message to a local directory and return the saved file paths. Works for both received and sent messages. Files are written with the original filenames Edookit reports, into the directory given by `destination_dir`. The directory is created if it doesn't exist (mode 0700). If the same filename already exists in the directory it is left alone (download is skipped) unless `overwrite=true` is also passed. Returns a JSON object with message_id, directory, and a files array of {name, path, bytes, skipped?, error?} — a per-attachment outcome. A populated `error` on one entry means just that file failed; the others continue. Use this when the user asks to download, save, or open attachments — for example after edookit_get_message surfaces an attachment list."
    )]
    async fn edookit_download_attachments(
        &self,
        Parameters(args): Parameters<DownloadArgs>,
    ) -> Result<CallToolResult, ErrorData> {
        if args.id.is_empty() {
            return Ok(CallToolResult::error(vec![ContentBlock::text(
                "missing required parameter: id",
            )]));
        }
        let opts = tools::attachments::DownloadOptions {
            dest_dir: args.destination_dir.unwrap_or_default(),
            overwrite: args.overwrite.unwrap_or(false),
        };
        Ok(json_result(
            tools::attachments::download_attachments(&self.client, &args.id, opts).await,
        ))
    }

    #[tool(
        description = "View a single attachment of an **Edookit** message *inline* in the conversation — no file is written to disk. Use this (instead of edookit_download_attachments) when the user wants to SEE or READ an attachment's content directly: a photo/scan, a PDF (including scanned/image-only ones), or a text/CSV file. Returns MCP content blocks the client renders directly: images come back as image content (downscaled if very large); PDFs are rendered to PNG page images (the first `max_pages` pages) plus the whole document's extracted text, so even scanned/image-only PDFs are shown; text-like files come back as their decoded content. Office documents (doc/xls/ppt) and other binary types can't be shown inline — for those (or to keep a local copy) use edookit_download_attachments. Find attachment ids via edookit_get_message (each attachment has an `id` like `1@191207`)."
    )]
    async fn edookit_view_attachment(
        &self,
        Parameters(args): Parameters<ViewArgs>,
    ) -> Result<CallToolResult, ErrorData> {
        if args.id.is_empty() || args.attachment_id.is_empty() {
            return Ok(CallToolResult::error(vec![ContentBlock::text(
                "missing required parameter: both id and attachment_id are required",
            )]));
        }
        let opts = tools::view::ViewOptions {
            max_size_mb: args.max_size_mb.unwrap_or(0.0) as i64,
            max_pages: args.max_pages.unwrap_or(0.0) as i64,
        };
        match tools::view::view_attachment(&self.client, &args.id, &args.attachment_id, opts).await
        {
            Ok(res) => {
                // Bookend the attachment blocks with the untrusted-data banner so
                // the "treat as data" instruction has a hard boundary.
                let mut content = Vec::with_capacity(res.blocks.len() + 2);
                content.push(ContentBlock::text(tools::untrusted_attachment_banner()));
                for block in res.blocks {
                    match block {
                        ViewBlock::Text(t) => content.push(ContentBlock::text(t)),
                        ViewBlock::Image { b64, mime } => {
                            content.push(ContentBlock::image(b64, mime))
                        }
                    }
                }
                content.push(ContentBlock::text(tools::untrusted_attachment_close()));
                Ok(CallToolResult::success(content))
            }
            Err(e) => Ok(CallToolResult::error(vec![ContentBlock::text(
                e.to_string(),
            )])),
        }
    }

    #[tool(
        description = "List the signed-in teacher's **Edookit** courses — what the user calls *moje třídy / moje kurzy / moje skupiny* (the courses shown in Hodnocení → Známkování v tabulce). A course is a subject taught to a class or group, e.g. \"AUT - 4SA\" (whole class) with its split half-groups \"AUT 1 - 4SA\" / \"AUT 2 - 4SA\" (split_group=true). Use this for questions like \"which classes/courses do I teach\", \"list my groups\", or as the way to find a course_id before listing its pupils. Returns a JSON array of {course_id, name, split_group, students?, error?}. By default (no arguments) it returns just the course list — one cheap request. Pass `course_id` to get one course **with its student roster** (žáci: {study_id, name, class}); a half-group's roster is the subset of the class in that half. Pass `include_students=true` to populate every course's roster at once (heavier — one request per course; pupils of a class repeat under its half-groups). In that mode a course whose roster failed to load carries a non-empty `error` field (and no `students`), so an empty class is distinguishable from a failed fetch — don't treat a missing roster as 'no pupils' when `error` is set."
    )]
    async fn edookit_list_courses(
        &self,
        Parameters(args): Parameters<CoursesArgs>,
    ) -> Result<CallToolResult, ErrorData> {
        let opts = tools::courses::CoursesOptions {
            course_id: args.course_id.unwrap_or_default(),
            include_students: args.include_students.unwrap_or(false),
        };
        Ok(json_result(
            tools::courses::list_courses(&self.client, opts).await,
        ))
    }

    #[tool(
        description = "Return this edookit-mcp server's build metadata as JSON: {version, commit, build_time}. Use ONLY when the user explicitly asks which version is running or whether the server/connector is up to date — it is not part of any normal message or attachment workflow. Takes no arguments. Placeholder values (\"dev\"/\"none\"/\"unknown\") mean a local dev build, not a released binary."
    )]
    async fn edookit_server_info(&self) -> Result<CallToolResult, ErrorData> {
        match serde_json::to_string(self.info.as_ref()) {
            // server_info is the one tool NOT wrapped in the untrusted envelope —
            // it's our own trusted build metadata, not Edookit-derived content.
            Ok(j) => Ok(CallToolResult::success(vec![ContentBlock::text(j)])),
            Err(e) => Ok(CallToolResult::error(vec![ContentBlock::text(format!(
                "marshal: {e}"
            ))])),
        }
    }
}

// Dispatch through the pre-built `tool_router` field (constructed once in
// `new`) rather than the macro's default of rebuilding it on every call.
#[tool_handler(router = self.tool_router)]
impl ServerHandler for EdookitServer {
    fn get_info(&self) -> ServerInfo {
        // ServerInfo / Implementation are #[non_exhaustive] — build from Default
        // and set the public fields rather than struct-literal them.
        let mut implementation = Implementation::default();
        implementation.name = "edookit-mcp".to_string();
        implementation.version = self.info.version.clone();

        // The capabilities builder uses const-generic typestate, so it can't be
        // conditionally chained; build the tools-only base, then add the MCP
        // Apps surface (resources + the ui extension) by mutating public fields.
        let mut capabilities = ServerCapabilities::builder().enable_tools().build();
        if self.ui_resources {
            capabilities.resources = Some(ResourcesCapability::default());
            capabilities.extensions = Some(tools::ui::ui_extensions());
        }

        let mut info = ServerInfo::default();
        info.capabilities = capabilities;
        info.server_info = implementation;
        info
    }

    // --- MCP Apps surface (gated by `ui_active`: env switch + negotiation) --
    //
    // Per SEP-1865 the UI extension is optional and must be negotiated, so every
    // method below checks `ui_active(context)` — the operator switch *and* the
    // peer's declared `io.modelcontextprotocol/ui` capability — before emitting
    // any UI surface. We define these ourselves so `#[tool_handler]` skips
    // generating them (it only generates methods not already present): the
    // macro's `call_tool`/`list_tools` can't gate on the peer or attach
    // `_meta.ui.resourceUri`.

    async fn call_tool(
        &self,
        request: CallToolRequestParams,
        context: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, ErrorData> {
        // `structuredContent` is the Apps data channel; strip it for peers that
        // didn't negotiate the UI extension so Edookit rows never reach a
        // non-UI model outside the untrusted-data envelope. (Only
        // `edookit_list_inbox` sets it, and only when the env switch is on.)
        let ui_active = self.ui_active(&context);
        let tcc = rmcp::handler::server::tool::ToolCallContext::new(self, request, context);
        let mut response = self.tool_router.call(tcc).await?;
        // Our tools only ever complete (no elicitation/tasks), but the enum is
        // #[non_exhaustive] — pass anything else through untouched.
        if !ui_active && let CallToolResponse::Complete(result) = &mut response {
            result.structured_content = None;
        }
        Ok(response)
    }

    async fn list_tools(
        &self,
        _request: Option<PaginatedRequestParams>,
        context: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, ErrorData> {
        let mut tools = self.tool_router.list_all();
        if self.ui_active(&context)
            && let Some(t) = tools.iter_mut().find(|t| t.name == "edookit_list_inbox")
        {
            t.meta = Some(tools::ui::inbox_tool_meta());
        }
        Ok(ListToolsResult::with_all_items(tools))
    }

    async fn list_resources(
        &self,
        _request: Option<PaginatedRequestParams>,
        context: RequestContext<RoleServer>,
    ) -> Result<ListResourcesResult, ErrorData> {
        let mut result = ListResourcesResult::default();
        if self.ui_active(&context) {
            result.resources = vec![tools::ui::inbox_resource_descriptor()];
        }
        Ok(result)
    }

    async fn read_resource(
        &self,
        request: ReadResourceRequestParams,
        context: RequestContext<RoleServer>,
    ) -> Result<ReadResourceResponse, ErrorData> {
        if self.ui_active(&context) && request.uri == tools::ui::INBOX_UI_URI {
            return Ok(ReadResourceResult::new(vec![tools::ui::inbox_template_contents()]).into());
        }
        Err(ErrorData::resource_not_found(
            format!("unknown resource: {}", request.uri),
            None,
        ))
    }
}

/// Marshals a tool result to JSON, wraps it in the untrusted-data envelope, and
/// returns it as a text content block. A serialize failure or the tool's own
/// error becomes a tool-level error result (matching Go's `NewToolResultError`).
fn json_result<T: Serialize>(result: anyhow::Result<T>) -> CallToolResult {
    match result {
        Ok(value) => match serde_json::to_string(&value) {
            Ok(json) => CallToolResult::success(vec![ContentBlock::text(
                tools::wrap_as_untrusted_json(&json),
            )]),
            Err(e) => CallToolResult::error(vec![ContentBlock::text(format!("marshal: {e}"))]),
        },
        Err(e) => CallToolResult::error(vec![ContentBlock::text(e.to_string())]),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::client::{Client, Config, LoginCookie, LoginFn};
    use std::time::Duration;
    use wiremock::matchers::{method, path as mpath};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    // --- lenient scalar argument deserialization (number/bool-as-string) ---

    #[test]
    fn limit_accepts_number_string_and_absent() {
        // The exact shape ChatGPT-style clients send: a stringified number.
        let a: InboxArgs = serde_json::from_value(serde_json::json!({"limit": "10"})).unwrap();
        assert_eq!(a.limit, Some(10.0));
        // Plain JSON number still works.
        let a: InboxArgs = serde_json::from_value(serde_json::json!({"limit": 25})).unwrap();
        assert_eq!(a.limit, Some(25.0));
        // Absent / null / empty-string all collapse to None (tool default applies).
        let a: InboxArgs = serde_json::from_value(serde_json::json!({})).unwrap();
        assert_eq!(a.limit, None);
        let a: InboxArgs = serde_json::from_value(serde_json::json!({"limit": null})).unwrap();
        assert_eq!(a.limit, None);
        let a: InboxArgs = serde_json::from_value(serde_json::json!({"limit": ""})).unwrap();
        assert_eq!(a.limit, None);
    }

    #[test]
    fn limit_rejects_non_numeric_string() {
        let err =
            serde_json::from_value::<InboxArgs>(serde_json::json!({"limit": "lots"})).unwrap_err();
        assert!(err.to_string().contains("invalid number"), "got: {err}");
    }

    #[test]
    fn view_size_and_pages_accept_strings() {
        let a: ViewArgs = serde_json::from_value(serde_json::json!({
            "id": "m-1", "attachment_id": "1@2", "max_size_mb": "12", "max_pages": "3"
        }))
        .unwrap();
        assert_eq!(a.max_size_mb, Some(12.0));
        assert_eq!(a.max_pages, Some(3.0));
    }

    #[test]
    fn overwrite_accepts_bool_and_string_forms() {
        let a: DownloadArgs =
            serde_json::from_value(serde_json::json!({"id": "m-1", "overwrite": "true"})).unwrap();
        assert_eq!(a.overwrite, Some(true));
        let a: DownloadArgs =
            serde_json::from_value(serde_json::json!({"id": "m-1", "overwrite": false})).unwrap();
        assert_eq!(a.overwrite, Some(false));
        let a: DownloadArgs =
            serde_json::from_value(serde_json::json!({"id": "m-1", "overwrite": "0"})).unwrap();
        assert_eq!(a.overwrite, Some(false));
        let a: DownloadArgs = serde_json::from_value(serde_json::json!({"id": "m-1"})).unwrap();
        assert_eq!(a.overwrite, None);
    }

    /// Builds a Client whose login is stubbed and whose requests hit `uri`.
    fn build_client(uri: &str) -> Client {
        let login_fn: LoginFn =
            Arc::new(|| Box::pin(async { Ok(vec![LoginCookie::new("X-EdooAuthToken", "tok")]) }));
        let mut cfg = Config::new(uri, "u", "p");
        cfg.retry_base_delay = Duration::from_millis(1);
        cfg.timezone = Some(jiff::tz::TimeZone::get("Europe/Prague").unwrap());
        cfg.login_fn = Some(login_fn);
        Client::new(cfg).unwrap()
    }

    /// Mounts the warmup probe + a one-row inbox grid.
    async fn mount_inbox(server: &MockServer) {
        Mock::given(method("GET"))
            .and(mpath("/"))
            .respond_with(ResponseTemplate::new(200))
            .mount(server)
            .await;
        let row = r#"<small><b>21.05.2026 12:31</b> <span>Učitel 4SC</span></small><div><a href="x"><b>Pozvánka</b></a></div>"#;
        let grid = serde_json::json!({
            "components": { "workspace": [ { "data": [["m-290491", "m-290491", row]] } ] }
        });
        Mock::given(method("GET"))
            .and(mpath("/handler/grid/objects-for-me-data"))
            .respond_with(ResponseTemplate::new(200).set_body_json(grid))
            .mount(server)
            .await;
    }

    fn server_with(client: Client, ui_resources: bool) -> EdookitServer {
        EdookitServer::new(
            Arc::new(client),
            BuildInfo {
                version: "test".into(),
                commit: "test".into(),
                build_time: "test".into(),
            },
            ui_resources,
        )
    }

    #[tokio::test]
    async fn inbox_attaches_structured_content_when_enabled() {
        let mock = MockServer::start().await;
        mount_inbox(&mock).await;
        let srv = server_with(build_client(&mock.uri()), true);

        let res = srv
            .edookit_list_inbox(Parameters(InboxArgs::default()))
            .await
            .unwrap();

        // MCP Apps: the model-facing text block is unchanged; the rows ride
        // along as structuredContent (the UI template's data channel).
        assert_eq!(res.content.len(), 1, "single untrusted-JSON text block");
        assert!(res.content[0].as_text().is_some());
        let sc = res.structured_content.expect("structuredContent present");
        assert_eq!(sc["messages"][0]["id"], "m-290491");
    }

    #[tokio::test]
    async fn inbox_has_no_structured_content_when_disabled() {
        let mock = MockServer::start().await;
        mount_inbox(&mock).await;
        let srv = server_with(build_client(&mock.uri()), false);

        let res = srv
            .edookit_list_inbox(Parameters(InboxArgs::default()))
            .await
            .unwrap();

        assert_eq!(res.content.len(), 1);
        assert!(
            res.structured_content.is_none(),
            "no structuredContent when UI disabled"
        );
    }

    #[test]
    fn get_info_advertises_apps_extension_only_when_enabled() {
        let on = server_with(build_client("https://localhost"), true).get_info();
        let caps = serde_json::to_value(&on.capabilities).unwrap();
        assert_eq!(
            caps["extensions"]["io.modelcontextprotocol/ui"]["mimeTypes"][0],
            "text/html;profile=mcp-app"
        );
        assert!(
            caps["resources"].is_object(),
            "resources capability present"
        );

        let off = server_with(build_client("https://localhost"), false).get_info();
        let caps = serde_json::to_value(&off.capabilities).unwrap();
        assert!(caps["extensions"].is_null(), "no extension when disabled");
        assert!(caps["resources"].is_null(), "no resources when disabled");
    }

    // The `list_resources` / `read_resource` trait methods are thin gated
    // dispatchers over the pure helpers in `tools::ui` (covered by that module's
    // tests); constructing a `RequestContext<RoleServer>` to drive them directly
    // isn't worth the ceremony here.
}
