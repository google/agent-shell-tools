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

use std::io::{BufRead, BufReader};
use std::os::unix::process::CommandExt;
use std::path::PathBuf;
use std::process::{Command, Stdio};

use googletest::prelude::*;

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

fn resolve_bin(env_var: &str) -> PathBuf {
    let rootpath = std::env::var(env_var)
        .unwrap_or_else(|_| panic!("{env_var} must be set"));
    match std::env::var("RUNFILES_DIR") {
        Ok(dir) => PathBuf::from(dir).join("_main").join(rootpath),
        Err(_) => PathBuf::from(rootpath),
    }
}

fn wsb_bin() -> PathBuf { resolve_bin("WSB_BIN") }
fn grpc_exec_bin() -> PathBuf { resolve_bin("GRPC_EXEC_BIN") }

/// Create a unique temporary workspace directory for a test.
fn temp_workspace(test_name: &str) -> PathBuf {
    let dir = std::env::temp_dir()
        .join(format!("wsb-test-{test_name}-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// Start wsb for a workspace directory. Returns the child process and
/// the socket path parsed from the "ready: <path>" line on stdout.
fn start_wsb(workspace: &std::path::Path) -> (std::process::Child, String) {
    let mut cmd = Command::new(wsb_bin());
    cmd.arg("start")
        .arg(workspace)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    // Create a new process group so stop_wsb can signal the whole tree.
    unsafe { cmd.pre_exec(|| { libc::setpgid(0, 0); Ok(()) }); }
    let mut child = cmd.spawn().expect("failed to spawn wsb");

    let stdout = child.stdout.take().expect("no stdout");
    let mut reader = BufReader::new(stdout);
    let mut line = String::new();

    // Read lines until we get "ready: ..." or timeout.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(15);
    loop {
        if std::time::Instant::now() > deadline {
            child.kill().ok();
            child.wait().ok();
            panic!("wsb did not print ready line within 15s");
        }
        line.clear();
        match reader.read_line(&mut line) {
            Ok(0) => {
                child.wait().ok();
                panic!("wsb exited before printing ready line");
            }
            Ok(_) => {
                if let Some(path) = line.trim().strip_prefix("ready: ") {
                    return (child, path.to_string());
                }
            }
            Err(e) => {
                child.kill().ok();
                child.wait().ok();
                panic!("reading wsb stdout: {e}");
            }
        }
    }
}

/// Stop wsb by sending SIGTERM to its process group.  Since start_wsb
/// creates a new process group via setpgid, this reaches both the
/// launcher and the sandbox child.
fn stop_wsb(mut child: std::process::Child) {
    let pgid = child.id() as i32;
    // SIGTERM to the process group — child is the group leader.
    unsafe { libc::kill(-pgid, libc::SIGTERM); }
    // Give it a moment to shut down gracefully.
    std::thread::sleep(std::time::Duration::from_secs(2));
    // Only SIGKILL if the launcher is still running (avoid PGID reuse).
    if let Ok(None) = child.try_wait() {
        unsafe { libc::kill(-pgid, libc::SIGKILL); }
    }
    child.wait().ok();
}

/// Run a command via grpc_exec and return stdout.
fn exec_command(socket: &str, cmd: &str) -> String {
    let output = Command::new(grpc_exec_bin())
        .arg("-addr").arg(socket)
        .arg(cmd)
        .output()
        .expect("failed to spawn grpc_exec");
    String::from_utf8_lossy(&output.stdout).into_owned()
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[googletest::test]
fn start_creates_directories() {
    let workspace = temp_workspace("dirs");

    let (child, socket_path) = start_wsb(&workspace);

    // Runtime dir should exist with socket.
    let runtime_dir = workspace.join(".agent-shell-tools");
    expect_that!(runtime_dir.is_dir(), eq(true));
    expect_that!(std::path::Path::new(&socket_path).exists(), eq(true));

    // PID file should exist.
    let pid_path = runtime_dir.join("launcher.pid");
    expect_that!(pid_path.exists(), eq(true));

    stop_wsb(child);
    std::fs::remove_dir_all(&workspace).ok();
}

#[googletest::test]
fn start_end_to_end() {
    let workspace = temp_workspace("e2e");

    let (child, socket_path) = start_wsb(&workspace);

    // Run a command inside the sandbox.
    let output = exec_command(&socket_path, "echo hello");
    expect_that!(output.trim(), eq("hello"));

    // Verify we're inside the sandbox (hostname = coding-agent).
    let hostname = exec_command(&socket_path, "cat /proc/sys/kernel/hostname");
    expect_that!(hostname.trim(), eq("coding-agent"));

    stop_wsb(child);
    std::fs::remove_dir_all(&workspace).ok();
}

#[googletest::test]
fn start_rejects_nonexistent_workspace() {
    let parent = temp_workspace("noexist");
    let missing = parent.join("does-not-exist");

    let output = Command::new(wsb_bin())
        .arg("start")
        .arg(&missing)
        .output()
        .expect("failed to spawn wsb");

    expect_that!(output.status.code(), some(eq(1)));
    let stderr = String::from_utf8_lossy(&output.stderr);
    expect_that!(stderr, contains_substring("error:"));

    std::fs::remove_dir_all(&parent).ok();
}
