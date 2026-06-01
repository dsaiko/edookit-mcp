//! MCP tool implementations: message lists, full messages, attachments, inline
//! view, courses — plus the untrusted-data envelope. Port of Go's
//! `internal/tools`.

pub mod attachments;
pub mod courses;
mod date;
mod htmlutil;
pub mod message;
pub mod messages;
mod pdfrender;
pub mod untrusted;
pub mod view;

pub use untrusted::{
    untrusted_attachment_banner, untrusted_attachment_close, wrap_as_untrusted_json,
};
