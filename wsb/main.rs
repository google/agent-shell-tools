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
use std::path::{Path, PathBuf};
use std::process::ExitCode;

use clap::{Parser, Subcommand};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

const SOCKET_NAME: &str = "grpc_exec.sock";
const PID_NAME: &str = "launcher.pid";
const RUNTIME_DIR_NAME: &str = ".agent-shell-tools";

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
}

/// Metadata written to the state directory for workspace ID recovery.
#[derive(Serialize, Deserialize)]
struct WorkspaceMeta {
    path: String,
}

/// Resolved paths for a workspace session.
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

/// XDG data home, defaulting to $HOME/.local/share.
fn data_home() -> Result<PathBuf, String> {
    if let Ok(xdg) = std::env::var("XDG_DATA_HOME") {
        return Ok(PathBuf::from(xdg));
    }
    let home = std::env::var("HOME")
        .map_err(|_| "neither XDG_DATA_HOME nor HOME is set".to_string())?;
    Ok(PathBuf::from(home).join(".local/share"))
}

/// First 16 hex chars of SHA-256 of the canonical workspace path.
fn workspace_id(canonical: &Path) -> Result<String, String> {
    let path_str = canonical.to_str().ok_or_else(|| {
        format!("workspace path is not valid UTF-8: {}", canonical.display())
    })?;
    let mut hasher = Sha256::new();
    hasher.update(path_str.as_bytes());
    let hash = hasher.finalize();
    Ok(hash[..8].iter().map(|b| format!("{b:02x}")).collect())
}

/// Build the Layout for a workspace and create all necessary directories.
fn setup_layout(workspace: &Path) -> Result<Layout, String> {
    let id = workspace_id(workspace)?;
    let data_dir = data_home()?.join("agent-shell-tools").join(&id);
    let home_dir = data_dir.join("home");
    let runtime_dir = workspace.join(RUNTIME_DIR_NAME);
    let socket_path = runtime_dir.join(SOCKET_NAME);
    let pid_path = runtime_dir.join(PID_NAME);
    let log_path = data_dir.join("sandbox.log");

    // Check socket path length (Unix limit is typically 107 bytes).
    let socket_str = socket_path.to_str().ok_or("non-UTF-8 socket path")?;
    if socket_str.len() > 107 {
        return Err(format!(
            "socket path is {} bytes, exceeds Unix limit of 107: {socket_str}",
            socket_str.len(),
        ));
    }

    fs::create_dir_all(&home_dir)
        .map_err(|e| format!("creating state directory '{}': {e}", home_dir.display()))?;
    fs::create_dir_all(&runtime_dir)
        .map_err(|e| format!("creating runtime directory '{}': {e}", runtime_dir.display()))?;

    // Write workspace metadata for ID recovery.
    let meta = WorkspaceMeta {
        path: workspace.to_string_lossy().into_owned(),
    };
    let meta_path = data_dir.join("workspace.toml");
    fs::write(&meta_path, toml::to_string_pretty(&meta).unwrap())
        .map_err(|e| format!("writing '{}': {e}", meta_path.display()))?;

    Ok(Layout { workspace: workspace.to_path_buf(), data_dir, home_dir, runtime_dir, socket_path, pid_path, log_path })
}

/// Check for a running instance and clean up stale state.
///
/// Uses kill(pid, 0) to probe process liveness.  This cannot distinguish
/// PID reuse (a different process now owns that PID), but that is an
/// inherent limitation of PID files.  We treat EPERM as "alive" since
/// the process exists but is owned by another user.
fn check_stale(runtime_dir: &Path) -> Result<(), String> {
    let pid_path = runtime_dir.join(PID_NAME);
    if let Ok(contents) = fs::read_to_string(&pid_path) {
        if let Ok(pid) = contents.trim().parse::<i32>() {
            let ret = unsafe { libc::kill(pid, 0) };
            if ret == 0 {
                return Err(format!(
                    "another instance (pid {pid}) is already running for this workspace"
                ));
            }
            // EPERM means the process exists but we can't signal it.
            let errno = std::io::Error::last_os_error().raw_os_error().unwrap_or(0);
            if errno == libc::EPERM {
                return Err(format!(
                    "another instance (pid {pid}) is running for this workspace (owned by another user)"
                ));
            }
        }
        let _ = fs::remove_file(&pid_path);
    }
    let socket_path = runtime_dir.join(SOCKET_NAME);
    let _ = fs::remove_file(&socket_path);
    Ok(())
}

impl StartArgs {
    fn run(&self) -> ExitCode {
        let workspace = match resolve_path(&self.workspace).canonicalize() {
            Ok(p) => p,
            Err(e) => {
                eprintln!("error: resolving '{}': {e}", self.workspace.display());
                return ExitCode::from(1);
            }
        };
        if !workspace.is_dir() {
            eprintln!("error: '{}' is not a directory", workspace.display());
            return ExitCode::from(1);
        }

        let layout = match setup_layout(&workspace) {
            Ok(l) => l,
            Err(e) => {
                eprintln!("error: {e}");
                return ExitCode::from(1);
            }
        };

        if let Err(e) = check_stale(&layout.runtime_dir) {
            eprintln!("error: {e}");
            return ExitCode::from(1);
        }

        eprintln!("workspace: {}", layout.workspace.display());
        eprintln!("state:     {}", layout.data_dir.display());
        eprintln!("socket:    {}", layout.socket_path.display());
        ExitCode::SUCCESS
    }
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    match cli.command {
        Commands::Start(args) => args.run(),
    }
}
