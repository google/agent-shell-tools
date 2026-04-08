# mcpmux: Requirements & Session Plan

## Project Overview

Build a **meta-MCP proxy** in Go that lets a coding agent dynamically register, reload, and terminate child MCP servers at runtime — through MCP tools themselves. The proxy presents a single unified MCP server to the client while managing multiple backend MCP servers behind the scenes.

## Architecture

```
┌──────────────┐         ┌──────────────────────────────┐
│  MCP Client  │◄─stdio─►│          mcpmux               │
│              │         │  ┌────────────────────────┐   │
│              │         │  │ Management Tools:       │   │
│              │         │  │  • add_server           │   │
│              │         │  │  • remove_server        │   │
│              │         │  │  • reload_server        │   │
│              │         │  │  • list_servers         │   │
│              │         │  └────────────────────────┘   │
│              │         │  ┌────────────────────────┐   │
│              │         │  │ Proxied Tools:          │   │
│              │         │  │  • myserver__tool_a     │   │
│              │         │  │  • myserver__tool_b     │   │
│              │         │  │  • other__tool_x        │   │
│              │         │  └────────────────────────┘   │
│              │         │         │          │          │
│              │         │    ┌────▼───┐ ┌───▼────┐     │
│              │         │    │child 1 │ │child 2 │ ... │
│              │         │    │(stdio) │ │(stdio) │     │
│              │         │    └────────┘ └────────┘     │
└──────────────┘         └──────────────────────────────┘
```

## SDK

Use the **official Go MCP SDK**: `github.com/modelcontextprotocol/go-sdk/mcp` (v1.2.0+).

Key SDK features to leverage:
- `mcp.NewServer` — the proxy-facing server
- `mcp.AddTool` with generic typed inputs — for management tools
- Dynamic `AddTool`/`Server.RemoveTools` on a running server sends `notifications/tools/list_changed` automatically
- `mcp.NewClient` + `mcp.CommandTransport{Command: exec.Command(...)}` — to connect to each child MCP server process
- `client.Connect(...)` returns a `*mcp.ClientSession` for child server interactions
- `session.ListTools(...)` — discover child server tools
- `session.CallTool(...)` — forward calls to child servers

## Build System

- Bazel with `rules_go`
- Module: use existing `google/agent-shell-tools` `go.mod`
- All test targets runnable via `bazel test //...`

## Security Model

- `mcpmux` can spawn arbitrary child processes via the `add_server` management tool
- If the coding agent is not already running inside an equivalent sandbox, `mcpmux` must be run inside `//sandbox`
- The sandbox boundary is responsible for constraining filesystem and process-level effects of child MCP servers

---

## Functional Requirements

### FR-1: Management Tools

The proxy exposes these MCP tools to the client on startup:

#### `add_server`
```json
{
  "name": "string (required, unique identifier, e.g. 'my-dev-server')",
  "command": "string (required, e.g. 'node')",
  "args": ["string array (optional, e.g. ['./dist/index.js'])"],
  "env": {"key": "value (optional, merged into the proxy's environment)"},
  "cwd": "string (optional, working directory)"
}
```
- Spawns the child process with stdio transport
- Performs MCP `initialize` handshake with the child
- Calls `tools/list` on the child to discover its tools
- Re-exposes each child tool on the proxy with namespace prefix: `{name}__{tool_name}`
- Sends `notifications/tools/list_changed` to the client
- Succeeds even if the child exposes zero tools (server is still visible in `list_servers`)
- Returns success with list of discovered tools, or error if spawn/init fails
- Error if `name` already exists

#### `remove_server`
```json
{
  "name": "string (required)"
}
```
- Removes all namespaced tools from the proxy
- Sends `notifications/tools/list_changed`
- Removes the server from `list_servers` immediately (no `stopping` state)
- Sends graceful shutdown to the child (close stdin, wait with timeout)
- Force-kills if child doesn't exit within timeout (default 5s)
- Error if `name` not found

#### `reload_server`
```json
{
  "name": "string (required)"
}
```
- Equivalent to `remove_server` + `add_server` with the same config
- Preserves the original spawn configuration
- The SDK debounces `notifications/tools/list_changed` with a 10ms window, so rapid RemoveTool + AddTool calls coalesce into a single notification automatically
- Error if `name` not found

#### `list_servers`
No input. Returns:
```json
{
  "servers": [
    {
      "name": "my-dev-server",
      "command": "node",
      "args": ["./dist/index.js"],
      "status": "starting | running | crashed",
      "tools": ["my-dev-server__echo", "my-dev-server__greet"],
      "pid": 12345,
      "uptime_seconds": 120
    }
  ]
}
```

### FR-2: Tool Proxying

- When the client calls a namespaced tool (e.g. `myserver__echo`), the proxy:
  1. Strips the namespace prefix
  2. Forwards `tools/call` to the correct child via the stored MCP client
  3. Returns the child's response to the calling client
- Tool descriptions and input schemas are preserved from the child, with the tool name rewritten
- If a child process crashes mid-call, return an MCP error to the client

### FR-3: Namespace Collision Handling

- Tool calls containing `__` are routed to child servers (split on first `__`); all others are management tools
- Server names must be non-empty, unique, and must not contain `__`

### FR-4: Child Process Lifecycle

- Child processes are spawned with `os/exec.Command`, stdin/stdout wired as MCP stdio transport
- Child stderr is captured, prefixed with the server name, and logged to the proxy's stderr
- If a child process exits unexpectedly:
  - Mark server status as `crashed`
  - Remove its tools from the proxy
  - Send `notifications/tools/list_changed`
  - Do NOT auto-restart (the agent decides whether to `reload_server`)
- The proxy itself shutting down must clean up all child processes (SIGTERM, then SIGKILL after timeout)

### FR-5: Proxy Transport

- The proxy serves over **stdio**

---

## Non-Functional Requirements

### NFR-1: Concurrency Safety
- Multiple tool calls may be in flight simultaneously
- Child server map must be protected (sync.RWMutex or similar)
- Adding/removing servers while tool calls are in progress must not deadlock or race

### NFR-2: Graceful Degradation
- If a child is slow to respond, the proxy should respect context cancellation from the client
- If the child's stdin pipe breaks, detect and mark as crashed

### NFR-3: Logging
- Structured logging (slog) to stderr
- Log: child spawn/exit, tool discovery, tool calls (at debug level), errors

### NFR-4: No Python, No Node
- Pure Go. The proxy itself has zero runtime dependencies outside the Go binary.
- Child servers it manages can be anything (Node, Python, Rust, etc.) — the proxy doesn't care.

---

## Testing Plan

### Unit Tests

#### `TestNamespacing`
- Verify `{server}__{tool}` encoding and decoding
- Verify decoding splits on the first `__`
- Edge cases: tool names containing `__`, empty server names, server names containing `__`, Unicode

#### `TestServerRegistry`
- Add/remove/reload operations on an in-memory registry
- Verify concurrent add + remove doesn't race (run with `-race`)
- Verify duplicate `add_server` returns error
- Verify `remove_server` on nonexistent name returns error
- Verify `reload_server` preserves config

#### `TestToolMerging`
- Add two child servers with overlapping tool names → verify both appear with distinct namespaces
- Remove one → verify only its tools disappear
- Verify reserved management tool names cannot be overridden

#### `TestChildLifecycle`
- Spawn a mock child process (a simple Go binary that implements MCP)
- Verify initialize handshake completes
- Verify tools are discovered
- Kill the child externally → verify proxy detects crash and removes tools
- Verify graceful shutdown sends SIGTERM then SIGKILL

### Integration Tests

#### `TestEndToEndAddAndCall`
1. Start the proxy as a subprocess
2. Connect an MCP client to it
3. Call `add_server` with a test echo MCP server (built as a Bazel target)
4. Verify the echo server's tools appear via `tools/list`
5. Call the proxied echo tool → verify correct response
6. Call `remove_server` → verify tools disappear
7. Call the removed tool → verify error

#### `TestEndToEndReload`
1. Start proxy + add a test server (v1: exposes `echo` tool)
2. Swap the test server binary to v2 (exposes `echo` + `reverse` tools)
3. Call `reload_server`
4. Verify `reverse` tool now appears
5. Call `reverse` → verify correct response

#### `TestEndToEndCrashRecovery`
1. Start proxy + add a test server
2. Kill the test server's process directly (simulate crash)
3. Verify `list_servers` shows status `crashed`
4. Verify the crashed server's tools are no longer callable
5. Call `reload_server` → verify it comes back

#### `TestConcurrentCalls`
1. Start proxy + add a test server with a slow tool (sleeps 100ms)
2. Fire 10 concurrent `tools/call` requests
3. Verify all return correct results
4. While calls are in-flight, call `remove_server`
5. Verify in-flight calls either complete or return clean errors

### Test Fixtures (Bazel targets)

Build minimal test MCP servers as Go binaries:

#### `//mcpmux/test/echoserver`
- Implements MCP over stdio
- Exposes one tool: `echo` (returns input as-is)
- Exposes one tool: `slow_echo` (sleeps for a configurable duration, then returns input)

#### `//mcpmux/test/echoserver_v2`
- Same as above but adds a `reverse` tool (reverses input string)
- Used for reload testing

#### `//mcpmux/test/crashserver`
- Implements MCP, exposes a `crash` tool that calls `os.Exit(1)`
- Used for crash detection testing

### Test Execution

```bash
# All unit tests
bazel test //mcpmux/...

# Integration tests (spawns real processes)
bazel test //mcpmux/integration/...

# With race detector
bazel test //mcpmux/... --@io_bazel_rules_go//go/config:race

# Verbose for debugging
bazel test //mcpmux/... --test_output=all
```

---

## File Structure

```
mcpmux/
├── BUILD.bazel
├── main.go                 # Entry point: creates server, adds management tools, runs stdio
├── proxy.go                # Core proxy logic: server registry, child management
├── proxy_test.go           # Unit tests for registry, namespacing, merging
├── namespace.go            # Tool name encoding/decoding
├── namespace_test.go
├── child.go                # Child process lifecycle (spawn, monitor, kill)
├── child_test.go
├── integration/
│   ├── BUILD.bazel
│   ├── proxy_test.go       # End-to-end integration tests
│   └── testutil.go         # Helpers: start proxy, connect client, assert tools
└── test/
    ├── echoserver/
    │   ├── BUILD.bazel
    │   └── main.go
    ├── echoserver_v2/
    │   ├── BUILD.bazel
    │   └── main.go
    └── crashserver/
        ├── BUILD.bazel
        └── main.go
```

---

## Session Workflow

1. **Bootstrap**: Initialize the module, set up Bazel targets, add `go-sdk` dependency
2. **Namespace**: Implement and test `namespace.go` first — it's pure logic, easy to validate
3. **Registry**: Implement `proxy.go` with in-memory server map, add/remove/reload operations, unit test with `-race`
4. **Child process**: Implement `child.go` — spawn, MCP handshake, tool discovery, crash monitoring
5. **Test fixtures**: Build `echoserver`, `echoserver_v2`, `crashserver` as Bazel Go binaries
6. **Management tools**: Wire up `add_server`/`remove_server`/`reload_server`/`list_servers` as MCP tools in `main.go`
7. **Tool proxying**: Implement the call-forwarding logic with namespace resolution
8. **Integration tests**: End-to-end tests using real subprocesses
9. **Self-test**: `add_server` the proxy's own echo server, call it, reload it, remove it — verify the full loop

## Decisions

- **SSE/HTTP child transports**: Not in V1, stdio only
- **`watch_server` auto-reload on file changes**: No
- **Config file for pre-registering servers on startup**: Not in V1
- **Annotate tool descriptions with source server name**: Add if needed
- **Tool schema changes on reload**: TBD — if a child's tool schema changes after `reload_server`, the proxy re-discovers tools and updates schemas, but clients may cache old schemas
