# devbox-agentd

A persistent per-user daemon that exposes [Devbox](https://www.jetify.com/devbox)
project environments over a Unix-socket JSON-RPC 2.0 API — built for AI
coding agents, usable by anything that can speak JSON-RPC.

One daemon serves many agent sessions and many workspaces (including git
worktrees) at once. Commands run inside the workspace's `devbox run`
environment, so the project's pinned toolchain (node, go, python, ...) is
always available. Services started in one session keep running and stay
visible to other sessions.

## Install

**With the Pi extension** (recommended): `pi install
GitHub:<owner>/devbox-agentd#pi` — the extension downloads the matching
daemon binary from GitHub Releases on first use and keeps it in
`~/.local/share/devbox-agentd/bin`. No Go toolchain needed.

**Manual:**

```sh
go install ./cmd/devbox-agentd        # or download a release tarball:
# https://github.com/mohammadraufzahed/devbox-agentd/releases
```

Release versions are pinned: the extension carries `DAEMON_VERSION`, the
daemon carries `var version`, and the release workflow refuses to tag
when either drifts — upgrading the extension upgrades the daemon.

## Run

```sh
devbox-agentd serve [--socket PATH] [--mcp-socket PATH] [--idle-exit DUR]
devbox-agentd ensure    # connect-or-start detached; used by clients
devbox-agentd version
```

Sockets live under `$XDG_RUNTIME_DIR/devbox-agentd/` (fallback
`~/.local/state/devbox-agentd/`):

- `agent.sock` — newline-delimited JSON-RPC 2.0 API
- `mcp.sock` — MCP adapter (for clients that only speak MCP)

systemd socket activation units are in `contrib/systemd/`.

## Concepts

**Workspaces** — a bound project root (`workspace.open`). Everything
(execs, services, ptys, fs, git) is scoped to a workspace. When the root
has a `devbox.json` and `devbox` is on PATH, commands run under
`devbox run` automatically.

**Workspace binding** — `workspace.bind` pins a *connection's* default
workspace. With several agents sharing the daemon (e.g. parallel
worktrees), each session binds its own root and never has to pass
`workspace` again — no cross-talk. Explicit `workspace` params still
override the binding. `workspace.unbind` clears it.

**Events** — `events.subscribe` streams lifecycle events
(`exec.done`, `service.started`, `service.exited`, `pty.exited`,
`workspace.destroyed`) so clients get notified instead of polling.
Optional `types` and `workspace` filters.

**Destructive ops** — the daemon marks them in `meta.info`
(`destructive: true`); clients (like the Pi extension) confirm before
running them.

## API surface

| Method | Notes |
|---|---|
| `ping`, `status`, `meta.info` | liveness, snapshot, capabilities + destructive flags |
| `workspace.open` / `list` / `bind` / `unbind` / `destroy` | bind is per-connection; destroy kills its services/pty sessions |
| `exec.run` | `command`, `cwd`, `env`, `timeoutSec`, `stdin`, `background`; streams `exec.output` notifications; result has `exitCode`, `durationMs`, `timedOut`, `signal` |
| `exec.output` / `list` / `cancel` / `wait` | buffered output; cancel kills by id; wait blocks until done |
| `events.subscribe` | streams `event` notifications until cancelled |
| `service.start` / `stop` / `restart` / `list` / `logs` / `wait` | `start` accepts `cwd`, `env`, `portEnv` (auto-assigns a free TCP port into that env var), `port`; `list` reports uptime, restartCount, detected `ports`; `wait` blocks until running / port-accepting |
| `pty.open` / `write` / `read` / `kill` | pipe-backed interactive sessions |
| `fs.read` / `write` / `patch` | workspace-confined file ops; patch = exact-match multi-edit, all-or-nothing |
| `git.status` / `diff` / `log` / `commit` / `checkout` / `reset` / `add` / `fetch` / `pull` / `push` / `branch` / `stash` / `show` | commit supports `paths` for selective staging; push uses `--force-with-lease` when `force` |

All workspace-scoped methods accept an optional `workspace` (root path).

## Secrets & trust model

- The socket is `0600`, same user only. `exec.run` runs arbitrary shell
  commands **by design** — same trust model as the agent's own shell.
- Prefer the `env` param over embedding secrets in `command`: env values
  are never stored, serialized, or echoed back.
- `cwd`/`path` params are confined to the workspace root — `..` escapes
  are rejected.
- Output buffers are capped (`bufferCap` in `meta.info`, tail-kept).

## Pi extension

The `pi/` directory contains the [pi](https://github.com/earendil-works/pi)
extension exposing all of this as `devbox_*` tools, plus a `/devbox`
status command and destructive-operation confirmation. See
[pi/README.md](pi/README.md).

## Protocol notes

- Newline-delimited JSON-RPC 2.0; notifications carry `reqId` for
  correlation with the request that started the stream.
- `$/cancel` notification `{"id": <request id>}` cancels in-flight
  requests (kills the process for `exec.run`, ends follows/subscriptions).
- Background execs survive client disconnects; completion arrives as an
  `exec.done` event to `events.subscribe` listeners.
