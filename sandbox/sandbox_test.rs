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

use std::path::{Path, PathBuf};
use std::process::{Command, ExitStatus, Output};

use googletest::prelude::*;

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

fn sandbox_bin() -> PathBuf {
    let rootpath = std::env::var("AGENT_SANDBOX_BIN")
        .expect("AGENT_SANDBOX_BIN must be set");
    match std::env::var("RUNFILES_DIR") {
        Ok(dir) => PathBuf::from(dir).join("_main").join(rootpath),
        Err(_) => PathBuf::from(rootpath),
    }
}

#[derive(Debug)]
struct SandboxOutput {
    pub status: ExitStatus,
    pub stdout: String,
    pub stderr: String,
}

fn sandbox(args: &[&str]) -> SandboxOutput {
    sandbox_with_flags(&[], args)
}

fn sandbox_with_flags(flags: &[&str], cmd: &[&str]) -> SandboxOutput {
    let home = tempfile::tempdir().expect("creating test HOME");
    sandbox_with_home(home.path(), flags, cmd)
}

fn sandbox_with_home(home: &Path, flags: &[&str], cmd: &[&str]) -> SandboxOutput {
    let Output { status, stdout, stderr } = Command::new(sandbox_bin())
        .env("HOME", home)
        .args(flags)
        .args(["--"])
        .args(cmd)
        .output()
        .expect("failed to spawn agent-sandbox");
    let output = SandboxOutput {
        status,
        stdout: String::from_utf8_lossy(&stdout).into_owned(),
        stderr: String::from_utf8_lossy(&stderr).into_owned(),
    };
    if output.status.code() == Some(255) {
        panic!("Sandbox infrastructure failure (exit 255):\n{:#?}", output);
    }
    output
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[googletest::test]
fn echo_stdout() {
    let r = sandbox(&["/bin/echo", "hello"]);
    expect_that!(r.status.code(), some(eq(0)));
    expect_that!(r.stdout, eq("hello\n"));
}

#[googletest::test]
fn multi_arg_echo() {
    let r = sandbox(&["/bin/echo", "one", "two", "three"]);
    expect_that!(r.status.code(), some(eq(0)));
    expect_that!(r.stdout, eq("one two three\n"));
}

#[googletest::test]
fn exit_code_true() {
    let r = sandbox(&["/bin/true"]);
    expect_that!(r.status.code(), some(eq(0)));
}

#[googletest::test]
fn exit_code_false() {
    let r = sandbox(&["/bin/false"]);
    expect_that!(r.status.code(), some(eq(1)));
}

#[googletest::test]
fn exit_code_42() {
    let r = sandbox(&["/bin/sh", "-c", "exit 42"]);
    expect_that!(r.status.code(), some(eq(42)));
}

#[googletest::test]
fn pid_namespace() {
    let r = sandbox(&["/bin/sh", "-c", "cat /proc/self/status | grep ^NSpid"]);
    expect_that!(r.status.code(), some(eq(0)));
    // Jailed process is pid 2 (nsjail init is pid 1).
    expect_that!(r.stdout, ends_with("\t2\n"));
}

#[googletest::test]
fn uts_namespace() {
    let r = sandbox(&["/bin/cat", "/proc/sys/kernel/hostname"]);
    expect_that!(r.status.code(), some(eq(0)));
    expect_that!(r.stdout, eq("coding-agent\n"));
}

#[googletest::test]
fn home_exists_and_is_writable() {
    let home = tempfile::tempdir().unwrap();
    let home_str = home.path().to_str().unwrap();
    let r = sandbox_with_home(home.path(), &[], &["/bin/sh", "-c",
        &format!("test -d {home_str} && touch {home_str}/.test && rm {home_str}/.test")]);
    expect_that!(r.status.code(), some(eq(0)));
}

#[googletest::test]
fn home_matches_host() {
    let home = tempfile::tempdir().unwrap();
    let home_str = home.path().to_str().unwrap();
    let r = sandbox_with_home(home.path(), &[], &["/bin/sh", "-c", "echo $HOME"]);
    expect_that!(r.status.code(), some(eq(0)));
    expect_that!(r.stdout, eq(&format!("{home_str}\n")));
}

#[googletest::test]
fn persistent_home() {
    let dir = std::env::temp_dir().join("agent-sandbox-test-home");
    std::fs::create_dir_all(&dir).unwrap();
    let dir_str = dir.to_str().unwrap();

    // Write a file via the jail.
    let r = sandbox_with_flags(
        &["--home", dir_str],
        &["/bin/sh", "-c", "echo persist > $HOME/.marker"],
    );
    expect_that!(r.status.code(), some(eq(0)));

    // Verify it's visible on the host.
    let contents = std::fs::read_to_string(dir.join(".marker")).unwrap();
    expect_that!(contents.trim(), eq("persist"));

    std::fs::remove_dir_all(&dir).ok();
}

#[googletest::test]
fn all_cpus_available() {
    let r = sandbox(&["/bin/sh", "-c", "nproc"]);
    expect_that!(r.status.code(), some(eq(0)));

    // getconf reports online CPUs regardless of affinity masks, so it
    // reflects the true hardware count even if Bazel restricts the test.
    let host = Command::new("getconf")
        .arg("_NPROCESSORS_ONLN")
        .output()
        .unwrap();
    let expected = String::from_utf8_lossy(&host.stdout);

    expect_that!(r.stdout, eq(&*expected));
}

#[googletest::test]
fn rlimits_inherited() {
    let r = sandbox(&["/bin/sh", "-c", "ulimit -n"]);
    expect_that!(r.status.code(), some(eq(0)));

    let host = Command::new("/bin/sh")
        .args(["-c", "ulimit -n"])
        .output()
        .unwrap();
    let expected = String::from_utf8_lossy(&host.stdout);

    expect_that!(r.stdout, eq(&*expected));
}

#[googletest::test]
fn rw_mount() {
    let dir = std::env::temp_dir().join("agent-sandbox-test-rw");
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::write(dir.join("input"), "hello").unwrap();
    let dir_str = dir.to_str().unwrap();

    // Read the file and write a new one.
    let r = sandbox_with_flags(
        &["--rw", dir_str],
        &["/bin/sh", "-c", &format!("cat {dir_str}/input && echo world > {dir_str}/output")],
    );
    expect_that!(r.status.code(), some(eq(0)));
    expect_that!(r.stdout, eq("hello"));

    let output = std::fs::read_to_string(dir.join("output")).unwrap();
    expect_that!(output.trim(), eq("world"));

    std::fs::remove_dir_all(&dir).ok();
}

#[googletest::test]
fn ro_mount() {
    let dir = std::env::temp_dir().join("agent-sandbox-test-ro");
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::write(dir.join("data"), "readonly").unwrap();
    let dir_str = dir.to_str().unwrap();

    // Reading should work.
    let r = sandbox_with_flags(
        &["--ro", dir_str],
        &["/bin/cat", &format!("{dir_str}/data")],
    );
    expect_that!(r.status.code(), some(eq(0)));
    expect_that!(r.stdout, eq("readonly"));

    // Writing should fail.
    let r = sandbox_with_flags(
        &["--ro", dir_str],
        &["/bin/sh", "-c", &format!("echo fail > {dir_str}/new 2>&1; echo $?")],
    );
    expect_that!(r.stdout.trim(), not(eq("0")));

    std::fs::remove_dir_all(&dir).ok();
}

#[googletest::test]
fn rejects_mount_of_home() {
    let home = tempfile::tempdir().unwrap();
    let r = sandbox_with_home(home.path(), &["--rw", home.path().to_str().unwrap()], &["/bin/true"]);
    expect_that!(r.status.code(), some(eq(1)));
    expect_that!(r.stderr, contains_substring("would expose $HOME"));
}

#[googletest::test]
fn rejects_mount_of_home_parent() {
    let r = sandbox_with_flags(&["--ro", "/"], &["/bin/true"]);
    expect_that!(r.status.code(), some(eq(1)));
    expect_that!(r.stderr, contains_substring("would expose $HOME"));
}

#[googletest::test]
fn allows_mount_of_home_child() {
    // A child of $HOME does not "expose" $HOME, so it should be allowed.
    let home = tempfile::tempdir().unwrap();
    let child = home.path().join("project");
    std::fs::create_dir_all(&child).unwrap();
    let r = sandbox_with_home(
        home.path(),
        &["--rw", child.to_str().unwrap()],
        &["/bin/true"],
    );
    expect_that!(r.status.code(), some(eq(0)));
}

#[googletest::test]
fn stderr_passthrough() {
    let r = sandbox(&["/bin/sh", "-c", "echo err >&2"]);
    expect_that!(r.status.code(), some(eq(0)));
    expect_that!(r.stderr, contains_substring("err"));
}

#[googletest::test]
fn log_file_redirects_nsjail_output() {
    let log = std::env::temp_dir().join("agent-sandbox-test.log");

    // With --log-file, nsjail output goes to file, not stderr.
    let r = sandbox_with_flags(
        &["--log-file", log.to_str().unwrap()],
        &["/bin/echo", "hello"],
    );
    expect_that!(r.status.code(), some(eq(0)));
    expect_that!(r.stdout, eq("hello\n"));
    // stderr should be clean (no nsjail INFO lines).
    expect_that!(r.stderr, eq(""));

    // Log file should contain nsjail output.
    let log_contents = std::fs::read_to_string(&log).unwrap();
    expect_that!(log_contents, contains_substring("Mode:"));

    std::fs::remove_file(&log).ok();
}
