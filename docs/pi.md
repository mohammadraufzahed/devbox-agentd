# Using devbox-agentd from the Pi coding agent

`devbox-agentd` is a persistent per-user daemon that exposes Devbox project
environments — exec, services, PTY sessions, git, workspaces — over a Unix
socket. One daemon serves many Pi sessions: services started in one session
keep running and stay visible to sessions in other projects.

There are two ways to integrate with Pi:

| Path | What | Effort |
|---|---|---|
| **A. Native extension** | `pi-devbox-agentd` package: 20 `devbox_*` tools, `/devbox` command, destructive-op confirmations, Esc-cancellation, streaming output | `pi install` |
| **B. MCP via pi-mcp-adapter** | Zero code — the daemon already speaks MCP on `mcp.sock` | one JSON stanza |

## Prerequisite: the daemon must be running

Neither integration starts the daemon by itself (the native extension runs
`devbox-agentd ensure`, which does auto-start it — the adapter path does
not). Options:

```sh
# socket-activated systemd user units (recommended on Linux)
install -Dm644 contrib/systemd/devbox-agentd.{socket,service} \
    ~/.config/systemd/user/
systemctl --user enable --now devbox-agentd.socket

# or just let clients start it on demand
devbox-agentd ensure
```

Sockets live in `$XDG_RUNTIME_DIR/devbox-agentd/` (fallback
`~/.local/state/devbox-agentd/`): `agent.sock` (native API, mode 0600) and
`mcp.sock` (MCP, newline-delimited JSON-RPC, stdio framing).

## A. Native extension: `pi-devbox-agentd`

```sh
pi install git:github.com/<owner>/devbox-agentd#pi
# or a local checkout:
pi install /path/to/devbox-agentd/pi
```

Test it without installing:

```sh
pi -e ./pi/index.ts
```

`/reload` after edits. See [pi/README.md](../pi/README.md) for the full tool
list and troubleshooting.

## B. MCP via `pi-mcp-adapter` (zero code)

1. Install the adapter package in Pi (see its README).
2. Make sure the daemon is running — **the adapter never starts it**:
   `systemctl --user enable --now devbox-agentd.socket`, or run
   `devbox-agentd ensure` once.
3. Add this to `~/.config/mcp/mcp.json`, `~/.pi/agent/mcp.json`, or a
   project-local `.mcp.json` / `.pi/mcp.json`:

```json
{
  "mcpServers": {
    "devbox": {
      "socket": "${XDG_RUNTIME_DIR}/devbox-agentd/mcp.sock",
      "lifecycle": "lazy-keep-alive",
      "directTools": ["exec", "service_*", "git_status", "git_diff"],
      "approveTools": ["service_stop", "service_restart", "git_reset", "git_checkout", "workspace_destroy"],
      "requestTimeoutMs": 600000
    }
  }
}
```

The daemon's MCP surface uses **unprefixed** tool names (`exec`,
`service_start`, `git_status`, …); the adapter prefixes them with the
server name, so tools appear as `devbox_exec`, `devbox_service_start`, etc.

`directTools` inlines the listed tools as first-class Pi tools; everything
else stays reachable through the adapter's `mcp({...})` meta-tool.
`approveTools` are gated by the adapter's own approval flow — the daemon
itself has no permission model.

## Notes

- **Cancellation**: pressing Esc on a `devbox_exec` call sends `$/cancel`
  and the daemon kills the process (native extension path).
- **Destructive ops**: the native extension prompts via `ctx.ui.confirm`
  and fails closed (blocked with a reason) in `pi -p`/non-UI modes. On the
  MCP path use `approveTools` as above.
- **Idle exit**: `devbox-agentd serve --idle-exit 5m` exits when unused —
  off by default since sessions expect the daemon to persist.
