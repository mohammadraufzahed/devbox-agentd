---
name: devbox
description: Run commands inside the project's Devbox environment via devbox-agentd
---

# Devbox environments

The workspace is served by `devbox-agentd`, a daemon that runs commands
inside the project's Devbox environment (pinned toolchain from
`devbox.json`).

- Prefer `devbox_exec` over `bash` whenever a command needs the project's
  toolchain (node, go, python, linters, formatters, tests).
  - `cwd` for subdirs, `env` for secrets (never embed secrets in the
    command string), `timeoutSec`, `stdin`.
  - `background: true` returns an exec id immediately; collect with
    `devbox_exec_wait` or listen via `devbox_events` instead of polling.
- Long-running processes (dev servers, watchers): use
  `devbox_service_start` / `devbox_service_logs` — they survive this
  session and keep running for other sessions. `devbox_service_wait`
  blocks until ready (optionally until a TCP port accepts). Pass
  `portEnv: "PORT"` to let the daemon assign a free port — avoids
  collisions when parallel agents run the same service in different
  worktrees.
- Multi-agent/worktree safety: the extension binds this session to its
  own workspace at `session_start` (`devbox_workspace_bind`), so calls
  always resolve to this project root even when other agents share the
  daemon.
- Interactive programs: `devbox_pty_open` + `devbox_pty_write`.
- Truncated output: fetch the full text with `devbox_exec_output`.
- File edits through the daemon: `devbox_fs_read` / `devbox_fs_write` /
  `devbox_fs_patch` (workspace-confined).
- Git operations in the workspace: `devbox_git_*` tools (status, diff,
  log, commit with path staging, add, fetch, pull, push, branch, stash,
  show, checkout, reset).
- Async awareness: `devbox_events` collects daemon events (exec.done,
  service.exited, pty.exited, ...) for N seconds — use after background
  work instead of polling loops. Destructive tools are flagged in
  `meta.info` and prompt before running.
