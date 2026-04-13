// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

use std::fs;
use std::io::Write;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, ExitCode};
use std::time::{Duration, Instant};

use anyhow::{bail, ensure, Context, Result};
use clap::{Parser, Subcommand};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

const SOCKET_NAME: &str = "grpc_exec.sock";
const PID_NAME: &str = "launcher.pid";
const RUNTIME_DIR_NAME: &str = ".agent-shell-tools";
const SOCKET_TIMEOUT: Duration = Duration::from_secs(10);
const SOCKET_POLL_INTERVAL: Duration = Duration::from_millis(50);

/// Workspace sandbox launcher.
///
/// Starts a sandboxed grpc_execd for a workspace, exposing a gRPC socket
/// that agents can use to run commands inside the sandbox.
#[derive(Parser)]
#[command(name = "wsb")]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Start a sandboxed grpc_execd for the given workspace.
    Start(StartArgs),
}

#[derive(clap::Args)]
struct StartArgs {
    /// Path to the workspace directory.
    workspace: PathBuf,

    /// Path to the sandbox binary (default: auto-discover).
    #[arg(long)]
    sandbox_bin: Option<PathBuf>,

    /// Path to the grpc_execd binary (default: auto-discover).
    #[arg(long)]
    grpc_execd_bin: Option<PathBuf>,
}

/// Metadata written to the state directory for workspace ID recovery.
#[derive(Serialize, Deserialize)]
struct WorkspaceMeta {
    path: String,
}

/// Resolved paths for a workspace session.
#[derive(Debug)]
struct Layout {
    /// Canonical absolute path to the workspace.
    workspace: PathBuf,
    /// State directory: ~/.local/share/agent-shell-tools/<id>/
    data_dir: PathBuf,
    /// Persistent sandbox home: data_dir/home/
    home_dir: PathBuf,
    /// Runtime directory inside the workspace: <workspace>/.agent-shell-tools/
    runtime_dir: PathBuf,
    /// Unix socket path: runtime_dir/grpc_exec.sock
    socket_path: PathBuf,
    /// PID file: runtime_dir/launcher.pid
    pid_path: PathBuf,
    /// Sandbox log: data_dir/sandbox.log
    log_path: PathBuf,
}

/// Resolve a potentially-relative path against the real working directory.
/// When launched via `bazel run`, the process cwd is inside the runfiles
/// tree.  Bazel sets BUILD_WORKING_DIRECTORY to the original shell cwd.
fn resolve_path(p: &Path) -> PathBuf {
    if p.is_absolute() {
        return p.to_path_buf();
    }
    if let Ok(dir) = std::env::var("BUILD_WORKING_DIRECTORY") {
        return PathBuf::from(dir).join(p);
    }
    p.to_path_buf()
}

/// Resolve a binary path.  Uses the same BUILD_WORKING_DIRECTORY
/// handling as `resolve_path` so relative paths from `--sandbox-bin`
/// etc. work under `bazel run`.
fn resolve_binary_path(p: &Path) -> PathBuf {
    resolve_path(p)
}

/// XDG data home, defaulting to $HOME/.local/share.
fn data_home() -> Result<PathBuf> {
    if let Ok(xdg) = std::env::var("XDG_DATA_HOME") {
        return Ok(PathBuf::from(xdg));
    }
    let home = std::env::var("HOME")
        .context("neither XDG_DATA_HOME nor HOME is set")?;
    Ok(PathBuf::from(home).join(".local/share"))
}

/// First 16 hex chars of SHA-256 of the canonical workspace path.
fn workspace_id(canonical: &Path) -> Result<String> {
    let path_str = canonical
        .to_str()
        .with_context(|| format!("workspace path is not valid UTF-8: {}", canonical.display()))?;
    let mut hasher = Sha256::new();
    hasher.update(path_str.as_bytes());
    let hash = hasher.finalize();
    Ok(hash[..8].iter().map(|b| format!("{b:02x}")).collect())
}

/// Build the Layout for a workspace and create all necessary directories.
fn setup_layout(workspace: &Path) -> Result<Layout> {
    let id = workspace_id(workspace)?;
    let data_dir = data_home()?.join("agent-shell-tools").join(&id);
    let home_dir = data_dir.join("home");
    let runtime_dir = workspace.join(RUNTIME_DIR_NAME);
    let socket_path = runtime_dir.join(SOCKET_NAME);
    let pid_path = runtime_dir.join(PID_NAME);
    let log_path = data_dir.join("sandbox.log");

    // Check socket path length (Unix limit is typically 107 bytes).
    let socket_str = socket_path
        .to_str()
        .context("non-UTF-8 socket path")?;
    ensure!(
        socket_str.len() <= 107,
        "socket path is {} bytes, exceeds Unix limit of 107: {socket_str}",
        socket_str.len(),
    );

    fs::create_dir_all(&home_dir)
        .with_context(|| format!("creating state directory '{}'", home_dir.display()))?;
    fs::create_dir_all(&runtime_dir)
        .with_context(|| format!("creating runtime directory '{}'", runtime_dir.display()))?;

    // Write workspace metadata for ID recovery.
    let meta = WorkspaceMeta {
        path: workspace.to_string_lossy().into_owned(),
    };
    let meta_path = data_dir.join("workspace.toml");
    fs::write(&meta_path, toml::to_string_pretty(&meta).unwrap())
        .with_context(|| format!("writing '{}'", meta_path.display()))?;

    Ok(Layout { workspace: workspace.to_path_buf(), data_dir, home_dir, runtime_dir, socket_path, pid_path, log_path })
}

/// Check for a running instance and clean up stale state.
///
/// Uses kill(pid, 0) to probe process liveness.  This cannot distinguish
/// PID reuse (a different process now owns that PID), but that is an
/// inherent limitation of PID files.  We treat EPERM as "alive" since
/// the process exists but is owned by another user.
fn check_stale(runtime_dir: &Path) -> Result<()> {
    let pid_path = runtime_dir.join(PID_NAME);
    if let Ok(contents) = fs::read_to_string(&pid_path) {
        if let Ok(pid) = contents.trim().parse::<i32>() {
            let ret = unsafe { libc::kill(pid, 0) };
            if ret == 0 {
                bail!("another instance (pid {pid}) is already running for this workspace");
            }
            // EPERM means the process exists but we can't signal it.
            let errno = std::io::Error::last_os_error().raw_os_error().unwrap_or(0);
            if errno == libc::EPERM {
                bail!("another instance (pid {pid}) is running for this workspace (owned by another user)");
            }
        }
        let _ = fs::remove_file(&pid_path);
    }
    let socket_path = runtime_dir.join(SOCKET_NAME);
    let _ = fs::remove_file(&socket_path);
    Ok(())
}

/// Walk a Bazel runfiles tree looking for a file named `name`.
fn find_in_runfiles(runfiles_dir: &Path, name: &str) -> Option<PathBuf> {
    fn walk(dir: &Path, name: &str) -> Option<PathBuf> {
        let entries = fs::read_dir(dir).ok()?;
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_file() && entry.file_name() == name {
                return Some(path);
            }
            if path.is_dir() {
                if let Some(found) = walk(&path, name) {
                    return Some(found);
                }
            }
        }
        None
    }
    walk(runfiles_dir, name)
}

/// Find a binary by name.  Search order:
/// 1. Sibling of the current executable (dist tarball layout)
/// 2. Bazel runfiles tree (<exe>.runfiles/, recursive search for name)
/// 3. $PATH
fn find_binary(name: &str) -> Result<PathBuf> {
    if let Ok(self_path) = std::env::current_exe() {
        // Check sibling (dist tarball layout: all binaries in one dir).
        if let Some(dir) = self_path.parent() {
            let candidate = dir.join(name);
            if candidate.is_file() {
                return Ok(candidate);
            }
        }
        // Check Bazel runfiles tree: <exe>.runfiles/_main/**/name.
        let runfiles_dir = PathBuf::from(format!("{}.runfiles", self_path.display()));
        if runfiles_dir.is_dir() {
            if let Some(found) = find_in_runfiles(&runfiles_dir, name) {
                return Ok(found);
            }
        }
    }
    // Fall back to $PATH.
    if let Some(path_var) = std::env::var_os("PATH") {
        for dir in std::env::split_paths(&path_var) {
            let candidate = dir.join(name);
            if candidate.is_file() {
                return Ok(candidate);
            }
        }
    }
    bail!("'{name}' not found alongside wsb or in PATH")
}

/// Poll for the socket file to appear, checking that the child hasn't
/// exited prematurely.
fn wait_for_socket(socket_path: &Path, child: &mut Child) -> Result<()> {
    let deadline = Instant::now() + SOCKET_TIMEOUT;
    loop {
        if socket_path.exists() {
            return Ok(());
        }
        if Instant::now() > deadline {
            child.kill().ok();
            child.wait().ok();
            bail!(
                "grpc_execd socket did not appear within {}s",
                SOCKET_TIMEOUT.as_secs(),
            );
        }
        if let Ok(Some(status)) = child.try_wait() {
            bail!("sandbox exited early with {status}");
        }
        std::thread::sleep(SOCKET_POLL_INTERVAL);
    }
}

/// Wait for the child to exit.  SIGINT and SIGTERM are ignored (via
/// sigaction in main) so the launcher survives signals and can clean up.
/// The child (sandbox/nsjail) gets default signal handlers via pre_exec
/// and handles shutdown independently.
fn wait_for_child(child: &mut Child) -> u8 {
    match child.wait() {
        Ok(status) => status.code().unwrap_or(1) as u8,
        Err(e) => {
            eprintln!("error: waiting for sandbox: {e}");
            1
        }
    }
}

/// Remove runtime artifacts (socket and PID file).
fn cleanup(layout: &Layout) {
    let _ = fs::remove_file(&layout.socket_path);
    let _ = fs::remove_file(&layout.pid_path);
}

impl StartArgs {
    fn run(&self) -> Result<u8> {
        let workspace = resolve_path(&self.workspace)
            .canonicalize()
            .with_context(|| format!("resolving '{}'", self.workspace.display()))?;
        ensure!(workspace.is_dir(), "'{}' is not a directory", workspace.display());

        let layout = setup_layout(&workspace)?;
        check_stale(&layout.runtime_dir)?;

        // Atomically claim the workspace via exclusive file creation.
        // If two launchers race, only one will succeed at create_new().
        let mut pid_file = fs::File::create_new(&layout.pid_path)
            .context("claiming workspace (PID file already exists — concurrent launch?)")?;
        let _ = write!(pid_file, "{}", std::process::id());
        drop(pid_file);

        // Helper: after claiming the PID file, any error must clean up.
        // We use a closure so ? propagates errors while still cleaning up.
        let result = self.start_sandbox(&layout);
        if result.is_err() {
            cleanup(&layout);
        }
        result
    }

    /// Inner startup logic after the PID file has been claimed.
    /// Caller is responsible for cleanup on error.
    fn start_sandbox(&self, layout: &Layout) -> Result<u8> {
        let sandbox_bin = match &self.sandbox_bin {
            Some(p) => resolve_binary_path(p),
            None => find_binary("sandbox")?,
        };
        let grpc_execd_bin = match &self.grpc_execd_bin {
            Some(p) => resolve_binary_path(p).canonicalize()
                .context("resolving grpc_execd path")?,
            None => find_binary("grpc_execd")?
                .canonicalize()
                .context("resolving grpc_execd path")?,
        };

        // The grpc_execd binary must be visible inside the sandbox.
        // Mount its parent directory read-only.
        let grpc_execd_dir = grpc_execd_bin.parent()
            .context("grpc_execd binary has no parent directory")?;

        let mut cmd = Command::new(&sandbox_bin);
        cmd.arg("--home").arg(&layout.home_dir)
            .arg("--rw").arg(&layout.workspace)
            .arg("--log-file").arg(&layout.log_path);
        // Only add --ro if grpc_execd isn't already under the workspace.
        if !grpc_execd_bin.starts_with(&layout.workspace) {
            cmd.arg("--ro").arg(grpc_execd_dir);
        }
        // Restore default signal handlers in the child before exec.
        // SIG_IGN is inherited across fork+exec, so without this the
        // child would also ignore SIGTERM.
        unsafe {
            cmd.pre_exec(|| {
                libc::signal(libc::SIGTERM, libc::SIG_DFL);
                libc::signal(libc::SIGINT, libc::SIG_DFL);
                Ok(())
            });
        }
        let mut child = cmd
            .arg("--")
            .arg(&grpc_execd_bin)
            .arg("-addr").arg(&layout.socket_path)
            .spawn()
            .context("spawning sandbox")?;

        // Update PID file with the sandbox child PID for stale detection.
        // The launcher PID was written before spawn as a lock; now replace
        // it with the child PID so stale detection tracks the sandbox.
        if let Err(e) = fs::write(&layout.pid_path, child.id().to_string()) {
            child.kill().ok();
            child.wait().ok();
            return Err(e).context("updating PID file");
        }

        wait_for_socket(&layout.socket_path, &mut child)?;

        // Ready — print socket path on stdout for callers to consume.
        // Explicit flush ensures the line is delivered when stdout is piped.
        let _ = writeln!(std::io::stdout(), "ready: {}", layout.socket_path.display());
        let _ = std::io::stdout().flush();

        let code = wait_for_child(&mut child);

        cleanup(&layout);
        Ok(code)
    }
}

fn main() -> ExitCode {
    // Ignore SIGINT and SIGTERM so the launcher survives until the child
    // exits and cleanup can run.  SIG_IGN is inherited across fork but
    // reset by exec, so the sandbox child gets default signal handlers.
    unsafe {
        let mut sa: libc::sigaction = std::mem::zeroed();
        sa.sa_sigaction = libc::SIG_IGN;
        sa.sa_flags = libc::SA_RESTART;
        libc::sigemptyset(&mut sa.sa_mask);
        libc::sigaction(libc::SIGTERM, &sa, std::ptr::null_mut());
        libc::sigaction(libc::SIGINT, &sa, std::ptr::null_mut());
    }

    let cli = Cli::parse();

    match cli.command {
        Commands::Start(args) => match args.run() {
            Ok(code) => ExitCode::from(code),
            Err(e) => {
                eprintln!("error: {e:#}");
                ExitCode::from(1)
            }
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::Path;

    #[test]
    fn workspace_id_deterministic() {
        let id1 = workspace_id(Path::new("/tmp/my-workspace")).unwrap();
        let id2 = workspace_id(Path::new("/tmp/my-workspace")).unwrap();
        assert_eq!(id1, id2);
    }

    #[test]
    fn workspace_id_is_16_hex_chars() {
        let id = workspace_id(Path::new("/tmp/test")).unwrap();
        assert_eq!(id.len(), 16);
        assert!(id.chars().all(|c| c.is_ascii_hexdigit()));
    }

    #[test]
    fn workspace_id_differs_for_different_paths() {
        let id1 = workspace_id(Path::new("/tmp/workspace-a")).unwrap();
        let id2 = workspace_id(Path::new("/tmp/workspace-b")).unwrap();
        assert_ne!(id1, id2);
    }

    /// Serialize tests that mutate environment variables.
    static ENV_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

    // SAFETY: tests are serialized by ENV_LOCK and run single-threaded
    // within the lock, so concurrent env access is prevented.
    unsafe fn set_env(key: &str, val: impl AsRef<std::ffi::OsStr>) {
        std::env::set_var(key, val);
    }
    unsafe fn remove_env(key: &str) {
        std::env::remove_var(key);
    }

    #[test]
    fn setup_layout_rejects_long_socket_path() {
        let _lock = ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let tmp = std::env::temp_dir().join(format!("wsb-test-long-path-{}", std::process::id()));
        // Build a workspace path long enough that the socket exceeds 107 bytes.
        // socket = <workspace>/.agent-shell-tools/grpc_exec.sock (40 extra chars)
        let long_name = "a".repeat(100);
        let workspace = tmp.join(&long_name);
        std::fs::create_dir_all(&workspace).unwrap();
        unsafe { set_env("XDG_DATA_HOME", tmp.join("data")) };

        let err = setup_layout(&workspace).unwrap_err();
        assert!(
            format!("{err}").contains("exceeds Unix limit of 107"),
            "expected socket length error, got: {err}",
        );

        unsafe { remove_env("XDG_DATA_HOME") };
        std::fs::remove_dir_all(&tmp).ok();
    }

    #[test]
    fn setup_layout_creates_workspace_toml() {
        let _lock = ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let tmp = std::env::temp_dir().join(format!("wsb-test-meta-{}", std::process::id()));
        let workspace = tmp.join("ws");
        std::fs::create_dir_all(&workspace).unwrap();
        let data_dir = tmp.join("data");
        unsafe { set_env("XDG_DATA_HOME", &data_dir) };

        let layout = setup_layout(&workspace).unwrap();

        // workspace.toml should exist in the data dir.
        let meta_path = layout.data_dir.join("workspace.toml");
        assert!(meta_path.exists(), "workspace.toml not created");
        let contents = std::fs::read_to_string(&meta_path).unwrap();
        let meta: WorkspaceMeta = toml::from_str(&contents).unwrap();
        assert_eq!(meta.path, workspace.to_string_lossy());

        // home dir should exist.
        assert!(layout.home_dir.is_dir(), "home dir not created");

        // data_dir should be under our XDG_DATA_HOME.
        assert!(layout.data_dir.starts_with(&data_dir));

        unsafe { remove_env("XDG_DATA_HOME") };
        std::fs::remove_dir_all(&tmp).ok();
    }

    #[test]
    fn data_home_respects_xdg() {
        let _lock = ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        unsafe { set_env("XDG_DATA_HOME", "/custom/data") };
        let result = data_home().unwrap();
        assert_eq!(result, PathBuf::from("/custom/data"));
        unsafe { remove_env("XDG_DATA_HOME") };
    }

    #[test]
    fn data_home_falls_back_to_home() {
        let _lock = ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        unsafe { remove_env("XDG_DATA_HOME") };
        // HOME should be set in test environment.
        let home = std::env::var("HOME").unwrap();
        let result = data_home().unwrap();
        assert_eq!(result, PathBuf::from(home).join(".local/share"));
    }

    #[test]
    fn check_stale_cleans_dead_pid() {
        let tmp = std::env::temp_dir()
            .join(format!("wsb-test-stale-dead-{}", std::process::id()));
        std::fs::create_dir_all(&tmp).unwrap();

        // Write a PID file with a PID that doesn't exist.
        // PID 2^22-1 is extremely unlikely to be in use.
        let pid_path = tmp.join(PID_NAME);
        std::fs::write(&pid_path, "4194303").unwrap();
        // Also create a stale socket file.
        let socket_path = tmp.join(SOCKET_NAME);
        std::fs::write(&socket_path, "").unwrap();

        // check_stale should succeed (dead PID) and remove both files.
        check_stale(&tmp).unwrap();
        assert!(!pid_path.exists(), "PID file should be removed");
        assert!(!socket_path.exists(), "socket file should be removed");

        std::fs::remove_dir_all(&tmp).ok();
    }

    #[test]
    fn check_stale_rejects_live_pid() {
        let tmp = std::env::temp_dir()
            .join(format!("wsb-test-stale-live-{}", std::process::id()));
        std::fs::create_dir_all(&tmp).unwrap();

        // Write our own PID — definitely alive.
        let pid_path = tmp.join(PID_NAME);
        std::fs::write(&pid_path, std::process::id().to_string()).unwrap();

        let err = check_stale(&tmp).unwrap_err();
        assert!(
            format!("{err}").contains("already running"),
            "expected 'already running' error, got: {err}",
        );

        std::fs::remove_dir_all(&tmp).ok();
    }

    #[test]
    fn check_stale_no_pid_file() {
        let tmp = std::env::temp_dir()
            .join(format!("wsb-test-stale-none-{}", std::process::id()));
        std::fs::create_dir_all(&tmp).unwrap();

        // No PID file — should succeed without error.
        check_stale(&tmp).unwrap();

        std::fs::remove_dir_all(&tmp).ok();
    }
}
