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

//! pktexecd: host-side daemon for pktexec.
//!
//! Listens on a SOCK_SEQPACKET Unix socket, receives command execution
//! requests with file descriptors via SCM_RIGHTS, spawns commands, and
//! manages their lifecycle. See DESIGN.md §5.1.

use std::io::IoSliceMut;
use std::os::fd::OwnedFd;
use std::path::PathBuf;
use std::process::Stdio;

use anyhow::{Context, Result, bail, ensure};
use clap::Parser;
use command_fds::{CommandFdExt, FdMapping};
use tokio::io::AsyncReadExt;
use tokio::signal::unix::{SignalKind, signal};
use tokio_seqpacket::ancillary::OwnedAncillaryMessage;
use tokio_seqpacket::{UnixSeqpacket, UnixSeqpacketListener};

/// Host-side daemon for pktexec.
#[derive(Parser)]
#[command(name = "pktexecd")]
struct Args {
    /// Path for the listening Unix socket.
    #[arg(long)]
    sock: PathBuf,
}

const MAX_MSG_SIZE: usize = 65536;
const ANCILLARY_BUF_SIZE: usize = 128;
const STATUS_FD_CHILD: i32 = 3;

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse();

    // Remove stale socket if present.
    let _ = std::fs::remove_file(&args.sock);

    let mut listener = UnixSeqpacketListener::bind(&args.sock)
        .with_context(|| format!("bind {:?}", args.sock))?;
    eprintln!("pktexecd: listening on {:?}", args.sock);

    // Graceful shutdown on SIGTERM.
    let mut sigterm = signal(SignalKind::terminate())?;

    loop {
        tokio::select! {
            result = listener.accept() => {
                let conn = result.context("accept")?;
                tokio::spawn(async move {
                    if let Err(e) = handle_connection(conn).await {
                        eprintln!("pktexecd: connection error: {e:#}");
                    }
                });
            }
            _ = sigterm.recv() => {
                eprintln!("pktexecd: shutting down");
                break;
            }
        }
    }

    let _ = std::fs::remove_file(&args.sock);
    Ok(())
}

async fn handle_connection(conn: UnixSeqpacket) -> Result<()> {
    // Receive ExecRequest + 3 stdio fds via SCM_RIGHTS.
    let mut buf = vec![0u8; MAX_MSG_SIZE];
    let mut ancillary_buf = [0u8; ANCILLARY_BUF_SIZE];
    let (n, ancillary) = conn
        .recv_vectored_with_ancillary(
            &mut [IoSliceMut::new(&mut buf)],
            &mut ancillary_buf,
        )
        .await
        .context("recv ExecRequest")?;

    // Extract file descriptors from ancillary data.
    let mut fds: Vec<OwnedFd> = Vec::new();
    for msg in ancillary.into_messages() {
        if let OwnedAncillaryMessage::FileDescriptors(file_descriptors) = msg {
            fds.extend(file_descriptors);
        }
    }
    ensure!(fds.len() == 3, "expected 3 fds, got {}", fds.len());

    let msg: wire::ClientMessage =
        wire::decode(&buf[..n]).context("decode ExecRequest")?;

    let (argv, working_dir) = match msg {
        wire::ClientMessage::ExecRequest { argv, working_dir } => (argv, working_dir),
        _ => bail!("expected ExecRequest, got {msg:?}"),
    };
    ensure!(!argv.is_empty(), "empty argv");

    let stdin_fd = fds.remove(0);
    let stdout_fd = fds.remove(0);
    let stderr_fd = fds.remove(0);

    // No filter — allow all commands.
    let allow = wire::ServerMessage::FilterResult {
        allowed: true,
        reason: String::new(),
    };
    let allow_bytes = wire::encode(&allow)?;
    conn.send(&allow_bytes)
        .await
        .context("send FilterResult")?;

    // Create status pipe for setup error reporting (cloexec pipe pattern).
    // O_CLOEXEC on both ends: the read end stays in the parent, and the
    // write end is dup2'd by command-fds into the child as STATUS_FD_CHILD
    // (dup2 does not copy O_CLOEXEC, so the child's copy starts without it;
    // pre_exec then sets it so exec closes it on success).
    let (status_r, status_w) =
        nix::unistd::pipe2(nix::fcntl::OFlag::O_CLOEXEC)
            .context("pipe for status")?;

    // Spawn the command directly (pktexec-ns integration deferred).
    let mut cmd = tokio::process::Command::new(&argv[0]);
    cmd.args(&argv[1..])
        .stdin(Stdio::from(stdin_fd))
        .stdout(Stdio::from(stdout_fd))
        .stderr(Stdio::from(stderr_fd));
    if !working_dir.is_empty() {
        cmd.current_dir(&working_dir);
    }
    cmd.kill_on_drop(true);

    // Pass the status pipe write end as fd 3 in the child.
    cmd.fd_mappings(vec![FdMapping {
        parent_fd: status_w.into(),
        child_fd: STATUS_FD_CHILD,
    }])
    .context("fd_mappings")?;

    // New process group + set O_CLOEXEC on the status fd in the child.
    unsafe {
        cmd.pre_exec(|| {
            libc::setpgid(0, 0);
            libc::fcntl(STATUS_FD_CHILD, libc::F_SETFD, libc::FD_CLOEXEC);
            Ok(())
        });
    }

    let mut child = match cmd.spawn() {
        Ok(child) => child,
        Err(e) => {
            // Spawn failed after ALLOW was sent. The client has already closed
            // its stdio, so we must report via the protocol, not stderr.
            let exit_msg = wire::ServerMessage::Exit { code: 127 };
            let _ = conn.send(&wire::encode(&exit_msg)?).await;
            return Err(e).with_context(|| format!("spawn {:?}", argv[0]));
        }
    };
    let pid = child.id().unwrap_or(0) as i32;

    // Drop cmd to close the parent's copy of the status pipe write end.
    // The write end was moved into FdMapping and held by cmd's pre_exec closure.
    drop(cmd);

    // Read status pipe: data = setup error, EOF = exec succeeded.
    let status_r_std = std::fs::File::from(OwnedFd::from(status_r));
    let mut status_r_tokio = tokio::fs::File::from_std(status_r_std);
    let mut status_buf = Vec::new();
    status_r_tokio
        .read_to_end(&mut status_buf)
        .await
        .context("read status pipe")?;

    if !status_buf.is_empty() {
        // Setup error — command never started.
        let status = child.wait().await.context("wait after setup error")?;
        let code = exit_code(&status);
        let exit_msg = wire::ServerMessage::Exit { code };
        let _ = conn.send(&wire::encode(&exit_msg)?).await;
        return Ok(());
    }

    // EOF on status pipe. Check if child already exited (ambiguous early death)
    // vs. exec succeeded.
    //
    // For now, enter the event loop — child.wait() will return immediately if
    // the child already exited.

    // Event loop: signal forwarding + wait for child exit.
    loop {
        let mut msg_buf = vec![0u8; MAX_MSG_SIZE];
        tokio::select! {
            result = conn.recv(&mut msg_buf) => {
                match result {
                    Ok(0) | Err(_) => {
                        // Client disconnected — kill the process group.
                        kill_pg(pid, libc::SIGKILL);
                        let _ = child.wait().await;
                        return Ok(());
                    }
                    Ok(n) => {
                        if let Ok(wire::ClientMessage::TermSignal { signo }) =
                            wire::decode(&msg_buf[..n])
                        {
                            kill_pg(pid, signo);
                        }
                    }
                }
            }
            result = child.wait() => {
                let status = result.context("wait")?;
                // Kill any lingering processes in the group.
                kill_pg(pid, libc::SIGKILL);
                let code = exit_code(&status);
                let exit_msg = wire::ServerMessage::Exit { code };
                let _ = conn.send(&wire::encode(&exit_msg)?).await;
                return Ok(());
            }
        }
    }
}

fn kill_pg(pid: i32, sig: i32) {
    if pid > 0 {
        let _ = nix::sys::signal::killpg(
            nix::unistd::Pid::from_raw(pid),
            nix::sys::signal::Signal::try_from(sig).ok(),
        );
    }
}

/// Extract exit code from a process status. For signal deaths, return
/// 128 + signal number (standard shell convention).
fn exit_code(status: &std::process::ExitStatus) -> i32 {
    use std::os::unix::process::ExitStatusExt;
    if let Some(code) = status.code() {
        code
    } else if let Some(sig) = status.signal() {
        128 + sig
    } else {
        1
    }
}
