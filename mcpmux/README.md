# mcpmux

A meta-MCP proxy in Go that lets a coding agent dynamically register, reload,
and terminate child MCP servers at runtime through MCP tools themselves.
Today, `mcpmux` multiplexes management tools plus child tools behind one stdio
MCP server. It does not yet multiplex the full MCP feature surface.

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

## Install

```bash
go install github.com/google/agent-shell-tools/mcpmux@latest
```

Or build with Bazel:

```bash
bazel build //mcpmux
bazel-bin/mcpmux/mcpmux_/mcpmux
```

The proxy starts with no child servers. Use the management tools to add them:

```jsonc
// tools/call add_server
{"name": "mytools", "command": "node", "args": ["./my-mcp-server.js"]}

// tools/call mytools__some_tool
{"arg1": "value"}

// tools/call reload_server
{"name": "mytools"}

// tools/call remove_server
{"name": "mytools"}

// tools/call list_servers
{}
```

## What's implemented

The current server initializes as MCP `2025-11-25` and advertises:

- `tools` with `listChanged`
- `logging`

### Management tools

| Tool | Description |
|------|-------------|
| `add_server` | Spawn a child process, MCP handshake, discover tools, expose them with `{name}__{tool}` prefix |
| `remove_server` | Remove tools from proxy, gracefully shut down child (stdin close -> SIGTERM -> SIGKILL) |
| `reload_server` | Remove + re-add with the same config (re-discovers tools) |
| `list_servers` | List all servers with name, command, args, status, tools, PID, uptime |

### Tool proxying

- Child tools are namespaced as `{server}__{tool}` (split on first `__`)
- The child tool's `description` and `inputSchema` are preserved
- Other tool metadata is not currently preserved during re-registration
- Tool calls are forwarded to the correct child's MCP session
- `notifications/tools/list_changed` is sent to the client on add/remove/reload

### Child process lifecycle

- Spawned via `os/exec` with configurable command, args, env, cwd
- MCP initialize handshake via the SDK's `CommandTransport`
- Child stderr forwarded to proxy stderr with `[name]` prefix
- Crash detection via `session.Wait()` — marks server as `crashed`, removes its tools
- Graceful shutdown with timeout, falls back to kill if in-flight calls block
- Single-use `Child` instances with enforced state machine: `New -> Starting -> Running -> Stopped|Crashed`

### Transport

- Proxy serves over **stdio** (reads stdin, writes stdout)
- Child servers connected over **stdio** (per-child stdin/stdout pipes)

## What's NOT implemented

These MCP spec features are **not proxied** through `mcpmux` today:

| Feature | What it means | Spec methods |
|---------|--------------|--------------|
| **Resources** | Child file/data resources are invisible to client | `resources/list`, `resources/read`, `resources/templates/list`, `resources/subscribe`, `resources/unsubscribe` |
| **Prompts** | Child prompt templates are invisible to client | `prompts/list`, `prompts/get` |
| **Completions** | Tab-completion for prompts/resources not forwarded | `completion/complete` |
| **Logging** | Child log messages not relayed to client | `logging/setLevel`, `notifications/message` |
| **Sampling** | Children cannot request LLM completions through proxy | `sampling/createMessage` (server->client) |
| **Roots** | Children cannot discover workspace roots | `roots/list` (server->client) |
| **Elicitation** | Children cannot request structured user input through the proxy | `elicitation/create`, `notifications/elicitation/complete` |
| **Tasks** | Task-augmented requests and task inspection/cancellation are not forwarded | `tasks/list`, `tasks/get`, `tasks/result`, `tasks/cancel` |
| **Cancellation / progress** | Cancellation and progress notifications are not explicitly bridged between child and client | `notifications/cancelled`, `notifications/progress` |
| **Tool metadata** | Re-registered tools drop metadata beyond `description` and `inputSchema` | `title`, `icons`, `annotations`, `outputSchema`, extension fields |
| **Dynamic tool discovery** | If a child adds/removes tools at runtime, proxy doesn't notice until `reload_server` | `notifications/tools/list_changed` (child->proxy direction) |

Additional current limitations:

- Child MCP notifications are not generally bridged to the client
- Child server capabilities are not merged into the mux capability advertisement
- Only child tools are discovered at startup; prompts and resources are not
  listed or merged

### Transport limitations

- No SSE/HTTP child transports (stdio only)
- No config file for pre-registering servers on startup

## Testing

```bash
# All tests
bazel test //mcpmux/...

# Unit tests only
bazel test //mcpmux/internal/mcpmux:mcpmux_test

# Integration tests (spawns real processes)
bazel test //mcpmux/internal/integration:integration_test

# With race detector
bazel test //mcpmux/... --@rules_go//go/config:race
```

## Security

mcpmux spawns arbitrary child processes via `add_server`. If the client is not
already sandboxed, run mcpmux inside `//sandbox` to constrain filesystem and
process-level effects of child MCP servers.
