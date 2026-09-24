# pi-devbox-agentd

Pi coding-agent extension for [`devbox-agentd`](../): a persistent per-user
daemon that exposes Devbox project environments (exec, services, PTY
sessions, git, workspaces) over a Unix-socket JSON-RPC API.

One daemon serves many Pi sessions — services started in one session keep
running and stay visible to sessions in other projects.

## Requirements

- Pi ≥ a version with extension support (`pi --version`).
- **No daemon install needed** — on first use the extension downloads the
  pinned `devbox-agentd` binary (SHA-256 verified) into
  `~/.local/share/devbox-agentd/bin` and keeps it in sync with the
  extension's expected version. A `devbox-agentd` already on `PATH` is
  used when its version matches.

## Install

```sh
# from the git repo (this package lives in pi/)
pi install git:github.com/mohammadraufzahed/devbox-agentd#pi

# or from a local checkout
pi install /path/to/devbox-agentd/pi
```

For ad-hoc testing without installing:

```sh
pi -e ./pi/index.ts
```

After installing or editing the extension, run `/reload` in Pi.

## What you get

- **Tools** (all `devbox_`-prefixed):
  `devbox_exec` (cwd/env/timeoutSec/stdin/background), `devbox_exec_output`,
  `devbox_exec_list`, `devbox_exec_cancel`, `devbox_exec_wait`,
  `devbox_events` (listen for exec.done/service.exited/... events),
  `devbox_service_{list,start,stop,restart,logs,wait}` (list reports
  uptime, restart count, detected ports),
  `devbox_pty_{open,write,read,kill}`,
  `devbox_fs_{read,write,patch}`,
  `devbox_git_{status,diff,log,commit,checkout,reset,add,fetch,pull,push,branch,stash,show}`,
  `devbox_workspace_{list,open,bind,destroy}`.
- **`/devbox` command**: daemon pid, socket, open workspaces, services.
- **Footer status** `devbox: <workspace>` in interactive mode.
- **Per-session workspace binding**: `session_start` calls
  `workspace.bind` with the project cwd — parallel agents on different
  worktrees never cross-talk even if a call omits `workspace`.
- **Service port assignment**: `devbox_service_start` accepts `portEnv`
  (daemon picks a free TCP port and injects it) — no port collisions
  between worktrees; `devbox_service_wait` blocks until ready.
- **Destructive confirmation**: `devbox_service_stop`,
  `devbox_service_restart`, `devbox_git_reset`, `devbox_git_checkout`,
  `devbox_git_push`, `devbox_git_stash` (drop/pop), `devbox_git_branch`
  (delete), `devbox_exec_cancel`, `devbox_workspace_destroy`,
  `devbox_pty_kill` prompt in interactive mode and are blocked with a
  reason in `pi -p` / non-UI modes. The full destructive list is also
  available programmatically via the daemon's `meta.info` method.
- **Cancellation**: Esc during a long `devbox_exec` cancels the daemon-side
  process (JSON-RPC `$/cancel`).
- **Streaming**: exec/pty/service-log output streams into the tool result;
  the final result is tail-truncated to 16 KB — fetch the rest with
  `devbox_exec_output`.

## Daemon lifecycle

The extension runs `devbox-agentd ensure` on `session_start` (and lazily on
first tool call), which connects to
`$XDG_RUNTIME_DIR/devbox-agentd/agent.sock` or starts the daemon detached.
If the daemon dies, the next tool call re-ensures it transparently.

To run it under systemd instead:

```sh
install -Dm644 contrib/systemd/devbox-agentd.{socket,service} ~/.config/systemd/user/
systemctl --user enable --now devbox-agentd.socket
```

## Troubleshooting

| Symptom | Fix |
|---|---|
| `devbox-agentd ensure failed` | install `devbox-agentd` on PATH (`go install ./cmd/devbox-agentd` or check the repo README) |
| `devbox: offline` in the footer | run `devbox-agentd serve` in a terminal to see the daemon's logs |
| tool blocked "requires interactive confirmation" | expected in `pi -p`; run interactively to approve destructive ops |
| stale socket after crash | safe — the next call re-ensures; or `rm $XDG_RUNTIME_DIR/devbox-agentd/agent.sock` |

## Zero-code alternative

Don't want the native extension? The daemon also speaks MCP on
`mcp.sock` — see [docs/pi.md](../docs/pi.md) for the `pi-mcp-adapter` setup.
