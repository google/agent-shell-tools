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

use std::ffi::c_int;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::ExitCode;

use clap::Parser;
use prost::Message;

/// Base jail config, converted from jail.txtpb to binary proto at build time.
const BASE_CONFIG: &[u8] = include_bytes!(env!("JAIL_CONFIG_PB"));

unsafe extern "C" {
    fn run_jail(config_pb: *const u8, config_pb_len: usize) -> c_int;
}

/// Sandboxed execution environment for coding agents.
#[derive(Parser)]
#[command(name = "agent-sandbox")]
struct Cli {
    /// Persistent home directory to bind-mount at $HOME inside the jail.
    /// If omitted, a tmpfs is used (ephemeral, discarded on exit).
    #[arg(long)]
    home: Option<PathBuf>,

    /// File to write nsjail's own log output to (default: stderr).
    #[arg(long)]
    log_file: Option<PathBuf>,

    /// Read-write bind mount (repeatable).
    #[arg(long = "rw")]
    rw_mounts: Vec<PathBuf>,

    /// Read-only bind mount (repeatable).
    #[arg(long = "ro")]
    ro_mounts: Vec<PathBuf>,

    /// Command and arguments to run inside the sandbox.
    #[arg(last = true, required = true)]
    command: Vec<String>,
}

/// Returns true if `path` is `home` or an ancestor of `home`.
/// Both paths are canonicalized to resolve symlinks and `..`.
fn exposes_home(path: &Path, home: &Path) -> bool {
    let Ok(path) = path.canonicalize() else { return false };
    let Ok(home) = home.canonicalize() else { return false };
    home.starts_with(&path)
}

/// Directories that may be symlinks (merged-usr) or real directories.
/// The symlink targets vary by distro (e.g. Arch: /sbin -> usr/bin,
/// Debian: /sbin -> usr/sbin), so we read the host layout at runtime.
const USR_LAYOUT_DIRS: &[&str] = &["/bin", "/sbin", "/lib", "/lib32", "/lib64"];

/// Returns /bin, /sbin, /lib, /lib64 and optional DNS-resolver mounts
/// based on the host filesystem layout.
fn get_host_layout_mounts() -> Vec<nsjail::MountPt> {
    let mut mounts = Vec::new();
    for &dir in USR_LAYOUT_DIRS {
        let path = Path::new(dir);
        if let Ok(target) = fs::read_link(path) {
            // Symlink (merged-usr): mirror the exact target inside the jail.
            mounts.push(nsjail::MountPt {
                src: Some(target.to_string_lossy().into_owned()),
                dst: dir.into(),
                is_symlink: Some(true),
                mandatory: Some(true),
                ..Default::default()
            });
        } else if path.is_dir() {
            // Real directory (non-merged-usr): bind-mount it.
            mounts.push(nsjail::MountPt {
                src: Some(dir.into()),
                dst: dir.into(),
                is_bind: Some(true),
                rw: Some(false),
                mandatory: Some(true),
                ..Default::default()
            });
        }
        // Missing entirely — skip (e.g. /lib64 on some 32-bit systems).
    }

    // DNS resolution: systemd-resolved stub listener needs its socket.
    if Path::new("/run/systemd/resolve").is_dir() {
        mounts.push(nsjail::MountPt {
            src: Some("/run/systemd/resolve".into()),
            dst: "/run/systemd/resolve".into(),
            is_bind: Some(true),
            rw: Some(false),
            mandatory: Some(true),
            ..Default::default()
        });
    }
    mounts
}

fn bind_mount(path: &PathBuf, rw: bool) -> nsjail::MountPt {
    let s = path.to_str().expect("non-UTF-8 path");
    nsjail::MountPt {
        src: Some(s.into()),
        dst: s.into(),
        is_bind: Some(true),
        rw: Some(rw),
        mandatory: Some(true),
        ..Default::default()
    }
}

impl Cli {
    fn run(&self) -> ExitCode {
        let mut config = nsjail::NsJailConfig::decode(BASE_CONFIG)
            .expect("failed to decode embedded jail config");

        config.mount.extend(get_host_layout_mounts());

        let home = std::env::var("HOME").expect("HOME not set");
        let home_path = Path::new(&home);
        config.cwd = Some(home.clone());

        // Reject mounts that would expose the real home directory.
        for path in self.rw_mounts.iter().chain(self.ro_mounts.iter()) {
            if exposes_home(path, home_path) {
                eprintln!(
                    "error: refusing to mount '{}': would expose $HOME ({})",
                    path.display(),
                    home,
                );
                return ExitCode::from(1);
            }
        }

        if let Some(ref home_dir) = self.home {
            config.mount.push(bind_mount(home_dir, true));
            // Remap to $HOME inside the jail.
            config.mount.last_mut().unwrap().dst = home;
        } else {
            config.mount.push(nsjail::MountPt {
                dst: home,
                fstype: Some("tmpfs".into()),
                rw: Some(true),
                options: Some("size=8589934592".into()),
                mandatory: Some(true),
                ..Default::default()
            });
        }

        for path in &self.rw_mounts {
            config.mount.push(bind_mount(path, true));
        }
        for path in &self.ro_mounts {
            config.mount.push(bind_mount(path, false));
        }

        if let Some(ref path) = self.log_file {
            config.log_file = Some(path.to_str().expect("non-UTF-8 path").into());
        }

        config.exec_bin = Some(nsjail::Exe {
            path: self.command[0].clone(),
            arg0: Some(self.command[0].clone()),
            arg: self.command[1..].to_vec(),
            exec_fd: None,
        });

        let bytes = config.encode_to_vec();

        let exit_code = unsafe { run_jail(bytes.as_ptr(), bytes.len()) };

        ExitCode::from(exit_code as u8)
    }
}

fn main() -> ExitCode {
    Cli::parse().run()
}
