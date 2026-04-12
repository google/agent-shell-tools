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

use std::path::PathBuf;
use std::process::ExitCode;

use clap::{Parser, Subcommand};

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

/// Resolve a potentially-relative path against the real working directory.
/// When launched via `bazel run`, the process cwd is inside the runfiles
/// tree.  Bazel sets BUILD_WORKING_DIRECTORY to the original shell cwd.
fn resolve_path(p: &std::path::Path) -> PathBuf {
    if p.is_absolute() {
        return p.to_path_buf();
    }
    if let Ok(dir) = std::env::var("BUILD_WORKING_DIRECTORY") {
        return PathBuf::from(dir).join(p);
    }
    p.to_path_buf()
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

        eprintln!("workspace: {}", workspace.display());
        ExitCode::SUCCESS
    }
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    match cli.command {
        Commands::Start(args) => args.run(),
    }
}
