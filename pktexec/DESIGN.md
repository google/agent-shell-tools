# pktexec: Host-Side Filtered Command Execution

## Status

Proposed. Not yet implemented.

## 1. Problem

An autonomous coding agent runs inside a sandbox (nsjail). Most commands
execute freely inside the sandbox, but some operations require host-side
execution with the developer's ambient credentials — for example, fetching
CI logs from an authenticated endpoint via `gh`, or querying an internal
build dashboard.

These host-side commands need two layers of protection:

1. **Policy**: a command filter that restricts which command lines are
   permitted, based on developer-authored rules.
2. **Mechanism**: a kernel-enforced mount namespace that prevents
   symlink-based path traversal, closing TOCTOU gaps the string-level
   filter cannot catch.

The existing `grpc_exec` service handles sandbox-side execution over gRPC.
It cannot be reused for host-side execution because gRPC does not support
`SCM_RIGHTS` file descriptor passing, which is required for the agent's
shell pipelines to compose with host-side commands.

## 2. Goals

- Let the agent run filtered commands on the host and compose them with
  sandbox-side pipes (`host_exec gh run view 123 | jq '.jobs[]'`).
- Enforce command filter rules and mount namespace isolation on every
  host-side execution.
- Support concurrent command execution from a single agent session.
- Avoid `unsafe` Rust in the entire pktexec codebase.

## 3. Non-Goals

- Replacing `grpc_exec` for sandbox-side execution.
- Implementing the command filter rule engine (separate component,
  `//command_filter`).
- Network-level egress filtering.

## 4. Design Overview

pktexec is three binaries connected by a Unix domain socket
(`AF_UNIX`, `SOCK_SEQPACKET`):

```
┌─────────────────────────────────────────────────────────┐
│                     Sandbox                              │
│                                                          │
│  Agent runs via grpc_exec:                               │
│    sh -c "pktexec gh run view 123 | jq '.jobs[]'"        │
│                │                            ▲             │
│                │ argv + fds (SCM_RIGHTS)    │ stdout      │
│                ▼                            │ (direct)    │
│         /run/pktexec.sock ─────────────────────────────  │
└──────────────────────┬──────────────────────────────────┘
                       │ bind-mounted UDS
┌──────────────────────▼──────────────────────────────────┐
│                      Host                                │
│                                                          │
│  pktexecd (daemon)                                       │
│    1. recv argv + fds                                    │
│    2. command_filter(argv) → allow / deny                │
│    3. spawn: pktexec-ns /workspace -- gh run view 123    │
│              (stdin/stdout/stderr = passed fds)          │
│              (status pipe for setup error reporting)     │
│    4. read status pipe: EOF → exec ok, data → error      │
│    5. wait for exit, send exit code                      │
│                                                          │
│  pktexec-ns (helper)                                     │
│    1. unshare(CLONE_NEWNS)                               │
│    2. bind + remount workspace with MS_NOSYMFOLLOW       │
│    3. exec(gh, run, view, 123)                           │
└──────────────────────────────────────────────────────────┘
```

Data flows directly between the host command and the sandbox pipe via
passed file descriptors. Neither `pktexec` (client) nor `pktexecd`
(daemon) is in the data path after the command starts.

### 4.1 Why SOCK_SEQPACKET

`SOCK_SEQPACKET` provides connection-oriented, message-boundary-preserving
delivery over `AF_UNIX`. Compared to `SOCK_STREAM`:

- **No framing needed.** Each `sendmsg`/`recvmsg` is one complete message.
  The protocol has four message types; hand-rolled length prefixing would
  be pure overhead.
- **SCM_RIGHTS attachment is per-message.** File descriptors attach to a
  specific packet, not to an arbitrary point in a byte stream.
- **Reliable and ordered**, unlike `SOCK_DGRAM`.

### 4.2 Why Not gRPC

gRPC operates over HTTP/2, which does not support `SCM_RIGHTS`. Without
fd passing, the daemon would need to relay all command I/O through its own
process — copying bytes between the host command's stdout and the gRPC
stream. This breaks pipe composition: `jq` in the sandbox would read from
the gRPC client's stdout, not from the host command's stdout, adding
latency and a copy.

### 4.3 Why Not nsjail

nsjail is used for the sandbox itself but is not suitable for host_exec.
nsjail's `pivot_root` model rebuilds the mount tree from scratch; host_exec
needs the opposite — inherit the entire host filesystem and change one mount
flag (`MS_NOSYMFOLLOW` on the workspace). Skipping `pivot_root` by using a
recursive bind mount of `/` is possible but means working against nsjail's
design, and still requires `pivot_root` cleanup. More concretely, nsjail's
protobuf mount config exposes specific flags (`nosuid`, `nodev`, `noexec`,
etc.) and may not support `MS_NOSYMFOLLOW` — a relatively recent flag
(Linux 5.10). The design also requires precise mount propagation control
(`MS_REC | MS_PRIVATE` on `/` before the remount; see §5.3), which is
difficult to express through nsjail's abstractions.

A small helper binary (`pktexec-ns`) that calls `unshare` + `mount`
directly is simpler, has no dependency on nsjail's flag support, and is
correct.

## 5. Components

### 5.1 `pktexecd` — Daemon (Host)

Long-running daemon on the host. Listens on a `SOCK_SEQPACKET` Unix
socket. Handles concurrent client connections, each in a separate tokio
task.

**Startup:**

```
pktexecd --sock /run/pktexec.sock [--rules /etc/agent/gh.rules]
```

- `--sock`: path for the listening socket.
- `--rules`: optional command filter rule file. If omitted, all commands
  are allowed (useful for development/testing).

**Per-connection flow:**

```
 accept() → spawn tokio task
   │
   ├─ recv ExecRequest + SCM_RIGHTS{stdin, stdout, stderr}
   │
   ├─ validate working_dir (canonicalize, check workspace prefix)
   │
   ├─ if rules loaded: filter(argv, working_dir)
   │     denied  → send FilterResult{allowed: false, reason}, close
   │     allowed → send FilterResult{allowed: true}
   │
   ├─ create status pipe (O_CLOEXEC on both ends)
   │
   ├─ spawn child: pktexec-ns <workspace> --cwd <working_dir>
   │                          --status-fd N -- <argv...>
   │     child inherits the three passed fds as its stdin/stdout/stderr
   │     child inherits the status pipe write end (via command-fds)
   │     child placed in a new process group (Setpgid)
   │
   ├─ read status pipe (read end) + child.wait():
   │     data        → setup failed: log error, send Exit{code}, close fds, done
   │     EOF + ex 0  → exec succeeded: command is running, enter event loop
   │     EOF + ex >0 → ambiguous early death: treat as setup failure
   │
   ├─ loop {
   │     select! {
   │       msg = conn.recv()    → TermSignal: forward signal to process group
   │       status = child.wait() → kill process group (cleanup),
   │                                send Exit{code}, close passed fds, break
   │       _ = conn.closed()    → kill process group (SIGKILL), break
   │     }
   │   }
   │
   └─ drop connection
```

**Process group cleanup.** The child is spawned with `Setpgid`, creating
a new process group. All signals and cleanup target the group
(`kill(-pgid, sig)`), not the immediate child PID. This catches
subprocesses that remain in the command's process group (background
jobs, pipelines, helpers). When the command exits normally, the daemon
also kills the group to reap lingering background children before
closing the passed fds. (This matches `grpc_exec`'s existing behavior.)

Processes that explicitly leave the group via `setsid()` or `setpgid()`
will not be caught. This is accepted: the command filter controls which
commands run, and commands that daemonize are uncommon in this context.
Full containment of arbitrary process trees requires PID namespace
isolation, which is what the sandbox provides.

### 5.2 `pktexec` — Client CLI (Sandbox)

A small CLI binary installed inside the sandbox. The agent uses it as a
command prefix in shell pipelines:

```sh
pktexec gh run view 123          # simple
pktexec gh run view 123 | jq .   # pipes work naturally
echo '{}' | pktexec curl -d @-   # stdin works too
```

**Argument handling:** Everything after `pktexec` is the command argv.
No shell interpretation — the agent's `sh -c` handles pipes and
redirects; `pktexec` receives its argv from `execve`.

```
pktexec [--sock PATH] [--] <command> [args...]
```

Default socket path: `/run/pktexec.sock` (or from `$PKTEXEC_SOCK`).

**Flow:**

```
 connect(sock)
   │
   ├─ send ExecRequest{argv, working_dir}
   │   + SCM_RIGHTS{own stdin, stdout, stderr}
   │
   ├─ recv FilterResult
   │     denied  → write reason to stderr, exit 1
   │     allowed → close own stdin/stdout/stderr
   │               (host process now owns them)
   │
   ├─ loop {
   │     select! {
   │       sig = signal(SIGTERM|SIGINT|SIGQUIT) → send TermSignal{sig}
   │       msg = conn.recv() → match msg {
   │           Exit{code} → break with code
   │           conn closed → break with 1
   │       }
   │     }
   │   }
   │
   └─ exit with code
```

**Why close own stdio after ALLOW:** The host process writes directly to
the pipe that downstream commands (e.g. `jq`) read from. If the client
kept its copies open, downstream would not receive EOF until the client
exited — a correctness issue when the host command finishes before the
client processes the exit code.

### 5.3 `pktexec-ns` — Namespace Helper (Host)

Minimal binary that sets up the mount namespace and execs the command.
Spawned by `pktexecd` as a child process.

```
pktexec-ns <workspace-path> [--cwd <dir>] [--status-fd N] -- <command> [args...]
```

**Status pipe.** If `--status-fd` is provided, pktexec-ns uses it to
report setup failures back to pktexecd via the cloexec pipe pattern
(used by `posix_spawn`, OpenSSH, and systemd for post-fork/pre-exec
error reporting).

The fd is inherited from the daemon without `O_CLOEXEC` — `command-fds`
clears the flag via `dup2` so the fd survives exec into pktexec-ns.
**pktexec-ns must restore `O_CLOEXEC` on the status fd as its very
first action** (step 0 below), before any fallible setup work. This
ensures that any subsequent failure — panic, setup error, or failed
exec — leaves the write end open, and the helper writes an error
message before exiting. Only a successful `exec` (step 6) closes the
fd via `O_CLOEXEC`, producing the EOF that the daemon interprets as
"command started."

The remaining window — between process start and the `fcntl` call in
step 0 — is a bootstrapping boundary: the fd hasn't been opened yet,
so errors cannot be reported through it regardless of protocol design.
A crash in this window (signal, OOM) produces EOF plus a non-zero
child exit status; the daemon should treat EOF paired with a non-zero
exit as an ambiguous failure rather than a successful exec (see §5.1
status pipe read).

The protocol:

- **Data on the pipe** → setup failed. The data is the error message.
  pktexec-ns exits with code 125 (setup error) or 127 (exec failed).
- **EOF + exit code 0** → `exec` succeeded, the command is running.
- **EOF + non-zero exit code** → ambiguous early death (crash before
  step 0). The daemon logs and reports this as a setup failure.

**Implementation** (~50 lines):

0. `fcntl(status_fd, F_SETFD, FD_CLOEXEC)` — restores `O_CLOEXEC`
   immediately. This is the first action after argument parsing and
   must precede all fallible work. From this point on, the cloexec
   pipe invariant holds: data means error, EOF means exec succeeded.
1. `unshare(CLONE_NEWNS)` — creates a new mount namespace, inheriting
   all existing host mounts.
2. `mount("/", MS_REC | MS_PRIVATE)` — recursively sets all mounts in
   the new namespace to private, cutting mount propagation in both
   directions. Without this step, on systemd-based hosts where the root
   mount is `shared`, the subsequent bind and remount would propagate
   back into the host namespace — adding `nosymfollow` to the host's
   workspace mount and breaking symlink-dependent operations (including
   the sandbox itself).
3. `mount(workspace, workspace, MS_BIND)` — creates a distinct mount
   entry for the workspace (it may not be a separate mount on the host).
4. `mount(workspace, MS_REMOUNT | MS_BIND | MS_NOSYMFOLLOW)` — adds the
   `nosymfollow` flag to the workspace mount.
5. `chdir(cwd)` — sets the working directory. This must happen after
   the remount: if `cwd` contains a symlink component under the
   workspace, `chdir` will fail with `ELOOP` because `nosymfollow` is
   now in effect. This closes a TOCTOU gap — if the daemon applied
   the cwd before the remount (e.g. via `Command::current_dir`), the
   kernel would follow the symlink before `nosymfollow` was active,
   and the process would land outside the workspace.
6. `exec(command, args)` — replaces the helper process with the actual
   command.

All operations use safe Rust APIs (`nix::sched::unshare`,
`nix::mount::mount`, `std::env::set_current_dir`,
`std::process::Command::exec`, `std::fs::File::from` for the status
fd). Zero `unsafe` blocks.

**Why a separate binary instead of `pre_exec`:**
`Command::pre_exec` requires an `unsafe` block because the closure runs
after `fork` in an async-signal-unsafe context. Moving the namespace
setup into a separate binary avoids `unsafe` entirely — the operations
run in a normal `main()`, not a post-fork closure.

## 6. Wire Protocol

`SOCK_SEQPACKET` preserves message boundaries, so no framing is needed.
Each message is a single `sendmsg`/`recvmsg` call.

### 6.1 Message Types

Four message types, two in each direction:

```
Client → Server:
  ExecRequest  { argv: Vec<String>, working_dir: String }
  TermSignal   { signo: i32 }

Server → Client:
  FilterResult { allowed: bool, reason: String }
  Exit         { code: i32 }
```

`ExecRequest` is always the first message on a connection. The three
stdio file descriptors are attached to this message via `SCM_RIGHTS`
ancillary data.

**Working directory.** The client is untrusted — `pktexecd` is the trust
boundary, and all client inputs are validated before use. The daemon
lexically canonicalizes `working_dir` and rejects it unless it falls
within the workspace. The validated cwd is used in two places:

1. **Filter path resolution.** Relative `<path:r>`/`<path:w>` arguments
   in argv are joined with the cwd and canonicalized before prefix
   checking. The final resolved path must still fall within the allowed
   directories — the cwd cannot widen the filter's approval.
2. **Child process cwd.** The daemon passes the validated cwd to
   `pktexec-ns` via `--cwd`, which applies it after the `nosymfollow`
   remount (see §5.3). This ensures symlink components in the path
   are caught by the kernel, and the command's path resolution agrees
   with the filter's.

If `working_dir` is empty, the daemon defaults to the workspace root.

`TermSignal` may be sent zero or more times while the command is running.

`FilterResult` is sent exactly once in response to `ExecRequest`.

`Exit` is sent exactly once after the command terminates (only if
`FilterResult.allowed` was true). The connection closes after this
message.

### 6.2 Serialization

Messages are serialized with [postcard](https://crates.io/crates/postcard)
(a compact serde-based binary format). The postcard dependency is confined
to a `wire` module that exposes `encode`/`decode` functions and
serde-derived message types. Swapping to another serde-compatible format
requires changing only this module.

### 6.3 Protocol Sequence

**Allowed command:**

```
Client                              Server
  │                                   │
  ├─ ExecRequest + fds ──────────────►│
  │                                   ├─ filter(argv) → allow
  │◄────────────── FilterResult{ok} ──┤
  │  (client closes own stdio)        ├─ spawn pktexec-ns
  │                                   │     child writes to passed fds
  │                                   │     ...
  ├─ TermSignal ─────────────────────►│  (optional, on signal)
  │                                   ├─ forward signal to child
  │                                   │     ...
  │◄──────────────── Exit{code: 0} ──┤  (child exits)
  │  (connection closes)              │
```

**Denied command:**

```
Client                              Server
  │                                   │
  ├─ ExecRequest + fds ──────────────►│
  │                                   ├─ filter(argv) → deny
  │◄─── FilterResult{denied, reason} ─┤
  │  (client prints reason, exits 1)  │  (connection closes)
```

**Client disconnect (SIGKILL):**

```
Client                              Server
  │                                   │
  ├─ ExecRequest + fds ──────────────►│
  │◄────────────── FilterResult{ok} ──┤
  │                                   ├─ spawn child
  X  (client killed)                  │
                                      ├─ detect conn closed (EPOLLHUP)
                                      ├─ kill child (SIGKILL)
                                      ├─ close passed fds
                                      └─ clean up
```

## 7. Signal Handling and Termination

### 7.1 Client-Side Signal Forwarding

The client traps `SIGTERM`, `SIGINT`, and `SIGQUIT`. On receipt, it sends
a `TermSignal` message to the daemon, which forwards the signal to the
child's process group. The client does not exit on signal — it waits for
the `Exit` message so it can report the correct exit code.

### 7.2 SIGKILL (Untrappable)

When the client is killed by `SIGKILL`:

1. The kernel closes all client file descriptors, including the UDS
   connection.
2. The daemon detects the closed connection via `EPOLLHUP` / read
   returning EOF.
3. The daemon sends `SIGKILL` to the child's process group.
4. The daemon closes the passed stdio fds.

This is the same model as `sshd`: connection drop = kill child.

### 7.3 Daemon Shutdown

On `SIGTERM`, the daemon:

1. Stops accepting new connections.
2. Sends `SIGTERM` to all running process groups.
3. Waits for children to exit (with a timeout).
4. Sends `SIGKILL` to any remaining process groups.
5. Exits.

## 8. Security Properties

### 8.1 Defense in Depth

The command filter and mount namespace are independent layers:

| Layer | Type | Catches |
|-------|------|---------|
| Command filter | Userspace policy | Wrong binary, wrong flags, wrong paths |
| Mount namespace + `nosymfollow` | Kernel mechanism | Symlink traversal, TOCTOU races |

Neither alone is sufficient. The filter operates on strings and cannot
detect symlinks created after the check. The mount namespace does not
know which commands are permitted. Together they cover each other's
blind spots.

### 8.2 Path Validation

The command filter validates `<path:r>` and `<path:w>` placeholders at
the string level:

1. **Lexical canonicalization.** `..` segments are resolved by pure
   string manipulation (no filesystem access). `/allowed/../../etc/shadow`
   canonicalizes to `/etc/shadow` and is rejected.
2. **Prefix matching.** Canonicalized paths are checked against permitted
   directory prefixes.
3. **No symlink resolution.** The filter checks literal paths only.
   Symlink escapes are caught by the mount namespace.

### 8.3 FD Passing Security

File descriptors are passed via `SCM_RIGHTS` over a `SOCK_SEQPACKET`
connection. The kernel authenticates the sender (same-machine, connected
socket). The daemon does not expose a network port.

### 8.4 No Data Path Copying

After the command starts, the host process reads from and writes to the
same pipe file descriptions as the sandbox processes. The daemon is not
in the data path — it only waits for the child to exit and monitors the
connection for signals or disconnects. This means the daemon cannot
inspect, log, or modify command I/O.

### 8.5 Hardlinks

Hardlinks are bounded by filesystem, not mount boundaries — a file
inside the workspace could in principle be hardlinked to a file outside
the workspace on the same filesystem. However, this is not an attack
vector in this threat model:

- **The agent cannot create hardlinks to files it cannot see.** Inside
  the sandbox, credential files are not mounted. Via host_exec, creating
  a hardlink requires referencing the target path, which the command
  filter rejects if it falls outside allowed directories.
- **Pre-existing hardlinks are a workspace integrity concern.** If the
  workspace already contains hardlinks to sensitive files, that is a
  property of how the developer provisioned the workspace, not something
  the agent can exploit. The same applies to any file content the
  developer places in the workspace.

No additional mitigation is needed. The filter prevents the agent from
naming out-of-workspace paths, and the sandbox prevents the agent from
discovering them.

## 9. Implementation Details

### 9.1 Language and Dependencies

All three binaries are Rust. Zero `unsafe` blocks.

| Crate | Used by | Purpose |
|-------|---------|---------|
| `tokio` | pktexecd, pktexec | Async runtime |
| `tokio-seqpacket` | pktexecd, pktexec | Async `SOCK_SEQPACKET` with ancillary data |
| `command-fds` | pktexecd | Safe extra fd passing to child processes |
| `serde` | all three | Derive macros for message types |
| `postcard` | pktexecd, pktexec | Binary serialization (confined to `wire` module) |
| `nix` | pktexec-ns | `unshare`, `mount` (safe APIs) |
| `anyhow` | all three | Error handling |

### 9.2 Build System

Bazel with `rules_rust`, consistent with the existing `//sandbox` binary.
Three `rust_binary` targets:

```
//pktexec:pktexecd          # daemon
//pktexec:pktexec           # client CLI
//pktexec:pktexec-ns        # namespace helper
```

A shared `rust_library` for the wire protocol:

```
//pktexec:wire              # message types + encode/decode
```

### 9.3 Concurrency Model

```
pktexecd (tokio runtime)
  │
  ├─ listener task: accept loop
  │    └─ for each connection → spawn per-connection task
  │
  └─ per-connection task:
       ├─ recv ExecRequest + fds
       ├─ filter(argv)
       ├─ create status pipe, spawn pktexec-ns (tokio::process::Command)
       ├─ read status pipe + child exit:
       │    data        → setup error: send Exit{code}, close fds, done
       │    EOF + ex 0  → exec succeeded, enter event loop
       │    EOF + ex >0 → ambiguous early death, treat as setup failure
       └─ loop { select! {
            msg = conn.recv() → forward signal to process group
            status = child.wait() → send Exit, close fds, break
            _ = conn.closed() → SIGKILL process group, break
          } }
```

Each connection is fully independent. No shared mutable state between
tasks (the filter rule set is loaded once at startup and shared via
`Arc`).

### 9.4 OwnedFd and Safe FD Handling

Received file descriptors are represented as `OwnedFd` (Rust 1.63+),
which guarantees validity and ownership. Conversion to `Stdio` for
the child process is safe:

```rust
let stdout: OwnedFd = received_fds.remove(1);
Command::new("pktexec-ns")
    .stdout(Stdio::from(stdout))  // safe: From<OwnedFd> for Stdio
    .spawn()?;
```

The status pipe write end is passed to pktexec-ns as an extra fd using
`command-fds`:

```rust
use command_fds::{CommandFdExt, FdMapping};

let (status_r, status_w) = nix::unistd::pipe2(OFlag::O_CLOEXEC)?;
let status_fd = 3;  // target fd number in child
Command::new("pktexec-ns")
    .args(["--status-fd", &status_fd.to_string()])
    .fd_mappings(vec![FdMapping {
        parent_fd: status_w.into(),
        child_fd: status_fd,
    }])?
    .spawn()?;
// daemon reads status_r to detect setup success/failure
```

`command-fds` uses `dup2` in a `pre_exec` hook to map the fd into the
child. `dup2` does not preserve `O_CLOEXEC`, so the fd survives exec
into pktexec-ns without the flag. **pktexec-ns must restore `O_CLOEXEC`
as its very first action** (step 0 in §5.3) before any fallible work,
so that a successful exec of the actual command closes the fd and
produces the EOF the daemon expects. See §5.3 for the full rationale
and the handling of the narrow bootstrapping window.

### 9.5 Socket Deployment

The daemon creates the socket on the host. The sandbox bind-mounts it
at a well-known path:

```
Host:    /run/agent/pktexec.sock   (created by pktexecd)
Sandbox: /run/pktexec.sock        (bind-mounted from host)
```

The socket path is configurable via `--sock` (daemon) and `--sock` or
`$PKTEXEC_SOCK` (client).

## 10. Testing Strategy

### 10.1 Unit Tests

- **Wire module:** Round-trip encode/decode for all message types.
  Property-based tests (e.g., `proptest`) for serialization correctness.
- **Filter integration:** Mock filter that returns canned allow/deny
  results. Verify the daemon sends correct `FilterResult` messages.

### 10.2 Integration Tests

- **End-to-end allowed command:** Start daemon (no rules), connect
  client, send `echo hello`, verify stdout and exit code.
- **End-to-end denied command:** Start daemon with rules, send a
  disallowed command, verify denial reason and exit code 1.
- **Pipe composition:** Run `pktexec echo hello | cat` inside the
  sandbox, verify output.
- **Signal forwarding:** Start a long-running command, send `TermSignal`,
  verify it exits with signal status.
- **Client disconnect:** Start a command, kill the client, verify the
  host process is cleaned up (no orphans).
- **Concurrent commands:** Run multiple commands simultaneously, verify
  independent completion.
- **Setup failure reporting:** Run with an invalid workspace path (e.g.
  nonexistent directory), verify the daemon reports the setup error via
  the status pipe and the client receives a non-zero exit code.

### 10.3 Mount Namespace Tests

- **`pktexec-ns` isolation:** Create a symlink in the workspace pointing
  outside, run a command via `pktexec-ns`, verify it gets `ELOOP`.
- **`..` canonicalization:** Submit a path with `..` traversal, verify
  the filter rejects it before execution.

## 11. Alternatives Considered

### 11.1 Extend grpc_exec with an ExecutionTarget Field

Add `HOST` mode to the existing gRPC proto. Rejected because gRPC
cannot pass file descriptors, which is essential for pipe composition.

### 11.2 Two grpc_exec Instances

Run a second `grpc_execd` on the host with filtering. Same gRPC
limitation — the daemon would need to relay all I/O, adding latency
and preventing direct pipe composition.

### 11.3 Use nsjail for the Mount Namespace

nsjail's `pivot_root` model rebuilds the mount tree from scratch; host_exec
needs the full host filesystem with one mount flag changed. A recursive bind
mount of `/` could approximate this, but it works against nsjail's design
and still requires `pivot_root` cleanup. nsjail's protobuf mount config may
not support `MS_NOSYMFOLLOW` (Linux 5.10), and expressing the required mount
propagation control (`MS_REC | MS_PRIVATE` on `/`; see §5.3) through its
abstractions is difficult. A ~35-line helper binary that calls `unshare` +
`mount` directly is simpler and correct.

### 11.4 SOCK_STREAM Instead of SOCK_SEQPACKET

Would require manual message framing (length prefixes) and careful
association of `SCM_RIGHTS` ancillary data with specific messages in
the byte stream. `SOCK_SEQPACKET` provides both for free.

### 11.5 pre_exec Instead of Helper Binary

`Command::pre_exec` requires `unsafe` because the closure runs after
`fork`. A helper binary moves the same operations into a normal `main()`
where all APIs are safe.
