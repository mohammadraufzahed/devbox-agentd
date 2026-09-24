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
- Long-running processes (dev servers, watchers): use
  `devbox_service_start` / `devbox_service_logs` — they survive this
  session and keep running for other sessions.
- Interactive programs: `devbox_pty_open` + `devbox_pty_write`.
- Truncated output: fetch the full text with `devbox_exec_output`.
- Git operations in the workspace: `devbox_git_*` tools.
