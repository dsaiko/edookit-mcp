// TODO(phase4): drop these crate-level allowances once main wires every module
// + transport; during construction modules and their re-exports land before
// their call sites (the MCP server, OAuth AS, HTTP transport) exist.
#![allow(dead_code, unused_imports)]

mod client;
mod tools;

fn main() {
    // Placeholder bootstrap — replaced in Phase 4 with clap flag parsing,
    // env wiring, and the MCP transport selection. Kept minimal for now so the
    // dependency tree compiles and `--version` works during scaffolding.
    println!(
        "edookit-mcp {} (commit {}, built {})",
        env!("EDOOKIT_VERSION"),
        env!("EDOOKIT_COMMIT"),
        env!("EDOOKIT_BUILD_DATE"),
    );
}
