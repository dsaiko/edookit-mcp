//! MCP server: registers the seven Edookit tools via rmcp's macro router and
//! maps tool outputs into MCP content (wrapping Edookit-derived data in the
//! untrusted envelope). Port of the tool-registration half of Go's `main.go`.

use std::sync::Arc;

use rmcp::handler::server::router::tool::ToolRouter;
use rmcp::handler::server::wrapper::Parameters;
use rmcp::model::{CallToolResult, Content, Implementation, ServerCapabilities, ServerInfo};
use rmcp::schemars::{self, JsonSchema};
use rmcp::{ErrorData, ServerHandler, tool, tool_handler, tool_router};
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
    tool_router: ToolRouter<EdookitServer>,
}

impl EdookitServer {
    pub fn new(client: Arc<Client>, info: BuildInfo) -> Self {
        Self {
            client,
            info: Arc::new(info),
            tool_router: Self::tool_router(),
        }
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
    max_size_mb: Option<f64>,
    #[schemars(
        description = "For PDFs: how many pages to render to images. Default 5, hard max 20. Extracted text always covers the whole document regardless."
    )]
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
    include_students: Option<bool>,
}

#[tool_router]
impl EdookitServer {
    #[tool(
        description = "List received messages from the **Edookit school information system** (Komunikace → Přijaté). Edookit is a Czech educational platform used by schools to communicate with parents and students. Use this tool when the user asks about school messages — anything from teachers, the school office, the head teacher (třídní učitel), the principal (ředitel), or about school topics like grades, attendance, parent-teacher meetings, trips, exams. This is NOT a general email inbox — for Gmail / Outlook / Slack DMs use those dedicated tools instead. Returns a JSON object with two keys: `messages` is an array of message objects (id, date, sender, subject, body_preview ~200 chars, attachments count) in newest-first order; `parse_warnings` (optional) lists any rows the server returned that couldn't be parsed — usually means Edookit's row HTML changed. An empty messages array with no warnings means the mailbox itself is empty; an error is returned if every fetched row failed to parse."
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
        Ok(json_result(
            tools::messages::list_inbox(&self.client, opts).await,
        ))
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
            return Ok(CallToolResult::error(vec![Content::text(
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
            return Ok(CallToolResult::error(vec![Content::text(
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
            return Ok(CallToolResult::error(vec![Content::text(
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
                content.push(Content::text(tools::untrusted_attachment_banner()));
                for block in res.blocks {
                    match block {
                        ViewBlock::Text(t) => content.push(Content::text(t)),
                        ViewBlock::Image { b64, mime } => content.push(Content::image(b64, mime)),
                    }
                }
                content.push(Content::text(tools::untrusted_attachment_close()));
                Ok(CallToolResult::success(content))
            }
            Err(e) => Ok(CallToolResult::error(vec![Content::text(e.to_string())])),
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
            Ok(j) => Ok(CallToolResult::success(vec![Content::text(j)])),
            Err(e) => Ok(CallToolResult::error(vec![Content::text(format!(
                "marshal: {e}"
            ))])),
        }
    }
}

#[tool_handler]
impl ServerHandler for EdookitServer {
    fn get_info(&self) -> ServerInfo {
        // ServerInfo / Implementation are #[non_exhaustive] — build from Default
        // and set the public fields rather than struct-literal them.
        let mut implementation = Implementation::default();
        implementation.name = "edookit-mcp".to_string();
        implementation.version = self.info.version.clone();

        let mut info = ServerInfo::default();
        info.capabilities = ServerCapabilities::builder().enable_tools().build();
        info.server_info = implementation;
        info
    }
}

/// Marshals a tool result to JSON, wraps it in the untrusted-data envelope, and
/// returns it as a text content block. A serialize failure or the tool's own
/// error becomes a tool-level error result (matching Go's `NewToolResultError`).
fn json_result<T: Serialize>(result: anyhow::Result<T>) -> CallToolResult {
    match result {
        Ok(value) => match serde_json::to_string(&value) {
            Ok(json) => {
                CallToolResult::success(vec![Content::text(tools::wrap_as_untrusted_json(&json))])
            }
            Err(e) => CallToolResult::error(vec![Content::text(format!("marshal: {e}"))]),
        },
        Err(e) => CallToolResult::error(vec![Content::text(e.to_string())]),
    }
}
