//! Test fixtures for Agen.

use std::path::PathBuf;
use std::sync::Mutex;

static BUILD_LOCK: Mutex<()> = Mutex::new(());

/// Path to a workspace binary, building it first (tests in one crate cannot
/// otherwise run another crate's binaries).
pub fn bin_path(package: &str, bin: &str) -> PathBuf {
    let _guard = BUILD_LOCK.lock().unwrap_or_else(|e| e.into_inner());
    let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../..");
    let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".into());
    let status = std::process::Command::new(cargo)
        .current_dir(&root)
        .args(["build", "-q", "-p", package, "--bin", bin])
        .status()
        .expect("run cargo build");
    assert!(status.success(), "building {bin} failed");
    let target = std::env::var("CARGO_TARGET_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|_| root.join("target"));
    target
        .join("debug")
        .join(format!("{bin}{}", std::env::consts::EXE_SUFFIX))
}
