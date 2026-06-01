// Build-time metadata injection — the Rust equivalent of the Go build's
// `-ldflags -X main.version=…`. Emits three compile-time env vars consumed by
// `env!()` in the binary:
//
//   EDOOKIT_VERSION     release version (CI sets EDOOKIT_VERSION; else Cargo pkg version)
//   EDOOKIT_COMMIT      short git commit (or "none" in a tree with no commits)
//   EDOOKIT_BUILD_DATE  build timestamp (CI sets EDOOKIT_BUILD_DATE; else "unknown")
//
// The "none"/"unknown" placeholders mirror the Go defaults: they mark a local
// dev build, as opposed to a released binary stamped by the release pipeline.
use std::process::Command;

fn main() {
    let commit = Command::new("git")
        .args(["rev-parse", "--short=12", "HEAD"])
        .output()
        .ok()
        .filter(|o| o.status.success())
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| "none".to_string());

    let version = env_nonempty("EDOOKIT_VERSION").unwrap_or_else(|| {
        std::env::var("CARGO_PKG_VERSION").unwrap_or_else(|_| "dev".to_string())
    });

    let date = env_nonempty("EDOOKIT_BUILD_DATE").unwrap_or_else(|| "unknown".to_string());

    println!("cargo:rustc-env=EDOOKIT_VERSION={version}");
    println!("cargo:rustc-env=EDOOKIT_COMMIT={commit}");
    println!("cargo:rustc-env=EDOOKIT_BUILD_DATE={date}");
    // Recompute when the checked-out commit or the override vars change.
    println!("cargo:rerun-if-changed=.git/HEAD");
    println!("cargo:rerun-if-env-changed=EDOOKIT_VERSION");
    println!("cargo:rerun-if-env-changed=EDOOKIT_BUILD_DATE");
}

fn env_nonempty(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|s| !s.is_empty())
}
