# agent-shell-tools

Tools to let coding agents access the shell with opinionated defaults.

## Components

- **[`sandbox`](sandbox/)** — nsjail-based execution sandbox. The container
  boundary is the primary security model: the agent runs with full freedom
  inside the sandbox but cannot access host credentials or mutate the host
  filesystem.
- **[`command_filter`](command_filter/)** — rule language and filter for
  host-side command execution. Complements sandboxing by narrowly delegating
  specific CLI capabilities the agent may use with the user's ambient
  credentials.
- **[`exec_service`](exec_service/)** — gRPC service for streaming command
  execution over Unix sockets.
- **[`mcpmux`](mcpmux/)** — MCP proxy for developing and testing MCP servers.
  The agent can edit a server, start it through `mcpmux`, and exercise it
  through the same MCP session — a full edit-test cycle.

File access is out of scope. Agents use their native file tools for that.

## Compositions

The sandbox boundary can be drawn at different points.

**Agent inside the sandbox.** The agent process runs inside `sandbox` and
executes commands freely; the container wall is the only boundary.
`command_filter` governs any host-side commands the agent is granted.

**Agent outside the sandbox.** The agent runs on the host and sends commands
to `exec_service` inside the sandbox over a Unix socket. `command_filter`
is not needed for sandboxed execution but may still govern other host-side
commands.

## License

Apache-2.0

## Disclaimer

> [!CAUTION]
> This is **not** an officially supported Google product.
