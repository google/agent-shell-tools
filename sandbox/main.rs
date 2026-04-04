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
use std::io::{Seek, Write};
use std::os::fd::{AsRawFd, FromRawFd};
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::ExitCode;

use clap::Parser;
use prost::Message;
use prost_reflect::{DescriptorPool, DynamicMessage};

/// Base jail config, converted from jail.txtpb to binary proto at build time.
const BASE_CONFIG: &[u8] = include_bytes!(env!("JAIL_CONFIG_PB"));

/// File descriptor set for nsjail's config.proto, used for text proto
/// serialization via prost-reflect.
const CONFIG_DESCRIPTOR: &[u8] = include_bytes!(env!("CONFIG_DESCRIPTOR"));

/// The nsjail binary, embedded at build time. At runtime it is unpacked into
/// a memfd and executed — no C++ linking required.
const NSJAIL_BIN: &[u8] = include_bytes!(env!("NSJAIL_BIN"));

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

// MFD_EXEC (0x0010) requests an executable memfd.  Required on kernels with
// vm.memfd_noexec=1 (Linux 6.3+).  Older kernels reject unknown flags, so
// callers should fall back on EINVAL.
const MFD_EXEC: libc::c_uint = 0x0010;

/// Creates an anonymous in-memory file via memfd_create(2).
fn memfd_create(name: &[u8], flags: libc::c_uint) -> std::io::Result<std::fs::File> {
    let fd = unsafe { libc::memfd_create(name.as_ptr() as *const libc::c_char, flags) };
    if fd < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(unsafe { std::fs::File::from_raw_fd(fd) })
}

/// Creates a memfd suitable for execve.  Tries MFD_EXEC first (needed on
/// hardened kernels), falls back to plain CLOEXEC on older kernels.
fn memfd_create_exec(name: &[u8]) -> std::io::Result<std::fs::File> {
    match memfd_create(name, libc::MFD_CLOEXEC | MFD_EXEC) {
        Ok(f) => Ok(f),
        Err(e) if e.raw_os_error() == Some(libc::EINVAL) => {
            memfd_create(name, libc::MFD_CLOEXEC)
        }
        Err(e) => Err(e),
    }
}

/// Serializes an NsJailConfig to protobuf text format using prost-reflect.
fn config_to_textproto(config: &nsjail::NsJailConfig) -> String {
    let pool = DescriptorPool::decode(CONFIG_DESCRIPTOR).expect("bad descriptor set");
    let desc = pool
        .get_message_by_name("nsjail.NsJailConfig")
        .expect("NsJailConfig not in descriptor set");
    let bytes = config.encode_to_vec();
    let msg = DynamicMessage::decode(desc, &bytes[..]).expect("failed to decode config");
    msg.to_string()
}

/// Unpacks the embedded nsjail binary into a memfd and exec's it with the
/// given config.  On success this function never returns (the process image
/// is replaced).
fn exec_nsjail(config: &nsjail::NsJailConfig) -> std::io::Error {
    let text = config_to_textproto(config);

    // Config memfd — no CLOEXEC so nsjail can read /proc/self/fd/N after exec.
    let mut config_file = memfd_create(b"config\0", 0).expect("memfd_create(config)");
    config_file.write_all(text.as_bytes()).expect("write config");
    config_file.seek(std::io::SeekFrom::Start(0)).expect("seek config");

    // Nsjail binary memfd — CLOEXEC is fine; the kernel loads the ELF
    // before closing the fd.  MFD_EXEC is requested for hardened kernels.
    let mut nsjail_file = memfd_create_exec(b"nsjail\0").expect("memfd_create(nsjail)");
    nsjail_file.write_all(NSJAIL_BIN).expect("write nsjail");

    let nsjail_path = format!("/proc/self/fd/{}", nsjail_file.as_raw_fd());
    let config_path = format!("/proc/self/fd/{}", config_file.as_raw_fd());

    std::process::Command::new(&nsjail_path)
        .arg("-C")
        .arg(&config_path)
        .exec()
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

        let err = exec_nsjail(&config);
        eprintln!("error: exec nsjail: {err}");
        ExitCode::from(0xff)
    }
}

fn main() -> ExitCode {
    Cli::parse().run()
}
