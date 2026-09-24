/**
 * pi-devbox-agentd — Pi extension for the devbox-agentd daemon.
 *
 * One persistent daemon per user serves many Pi sessions; this extension
 * attaches to it over a Unix socket and exposes devbox_* tools.
 *
 * IMPORTANT (pi extension lifecycle rule — see pi docs, extensions.md):
 * never start processes, sockets, watchers or timers in the factory.
 * The daemon connection is opened in `session_start` (or lazily on first
 * tool call) and closed in an idempotent `session_shutdown` handler.
 */

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { DevboxClient } from "./client.js";
import { workspaceTools } from "./tools/workspace.js";
import { execTools } from "./tools/exec.js";
import { serviceTools } from "./tools/services.js";
import { ptyTools } from "./tools/pty.js";
import { gitTools } from "./tools/git.js";

/** Tools that mutate state destructively — confirmed in UI, blocked otherwise. */
const DESTRUCTIVE = new Set([
	"devbox_service_stop",
	"devbox_service_restart",
	"devbox_git_reset",
	"devbox_git_checkout",
	"devbox_workspace_destroy",
	"devbox_pty_kill",
]);

interface WorkspaceInfo {
	root: string;
	name: string;
	devbox: boolean;
}

interface StatusResult {
	pid: number;
	socket: string;
	uptimeS: number;
	workspaces: WorkspaceInfo[];
	services: { name: string; workspace: string; running: boolean; pid?: number }[];
}

export default function (pi: ExtensionAPI) {
	const client = new DevboxClient();
	let boundWorkspace: WorkspaceInfo | undefined;

	// --- tools (cheap to register; no I/O in the factory) ---
	for (const t of workspaceTools(client)) pi.registerTool(t);
	for (const t of execTools(client)) pi.registerTool(t);
	for (const t of serviceTools(client)) pi.registerTool(t);
	for (const t of ptyTools(client)) pi.registerTool(t);
	for (const t of gitTools(client)) pi.registerTool(t);

	// --- session lifecycle ---
	pi.on("session_start", async (_event, ctx) => {
		try {
			await client.ensureAndConnect();
			const ws = await client.call<WorkspaceInfo>("workspace.open", { root: ctx.cwd });
			boundWorkspace = ws;
			if (ctx.hasUI) {
				ctx.ui.setStatus("devbox", `devbox: ${ws.name}`);
			}
		} catch (e) {
			if (ctx.hasUI) {
				ctx.ui.setStatus("devbox", "devbox: offline");
				ctx.ui.notify(
					`devbox-agentd unavailable: ${e instanceof Error ? e.message : e}`,
					"warning",
				);
			}
		}
	});

	pi.on("session_shutdown", () => {
		client.close();
	});

	// --- destructive confirmation gate ---
	// Fail closed: without a dialog-capable UI there is no way to confirm.
	pi.on("tool_call", async (event, ctx) => {
		if (!DESTRUCTIVE.has(event.toolName)) return;
		if (!ctx.hasUI) {
			return {
				block: true,
				reason: `${event.toolName} is destructive and requires interactive confirmation; not available in non-interactive (print/json) mode.`,
			};
		}
		const ok = await ctx.ui.confirm(
			`${event.toolName}?`,
			`This is a destructive operation. Input: ${JSON.stringify(event.input)}`,
		);
		if (!ok) {
			return { block: true, reason: `${event.toolName} declined by user` };
		}
	});

	// --- /devbox status command ---
	pi.registerCommand("devbox", {
		description: "Show devbox-agentd status (pid, socket, workspaces, services)",
		handler: async (_args, ctx) => {
			try {
				await client.ensureAndConnect();
				const s = await client.call<StatusResult>("status");
				const lines = [
					`devbox-agentd pid ${s.pid} — ${s.socket} (up ${s.uptimeS}s)`,
					``,
					`workspaces (${s.workspaces.length}):`,
					...s.workspaces.map(
						(w) => `  • ${w.name} — ${w.root}${w.devbox ? " (devbox)" : ""}`,
					),
					``,
					`services (${s.services.length}):`,
					...(s.services.length
						? s.services.map(
								(sv) =>
									`  • ${sv.name} ${sv.running ? "running" : "stopped"}${sv.pid ? ` pid ${sv.pid}` : ""} — ${sv.workspace}`,
							)
						: ["  (none)"]),
				];
				ctx.ui.notify(lines.join("\n"), "info");
			} catch (e) {
				ctx.ui.notify(
					`devbox-agentd unavailable: ${e instanceof Error ? e.message : e}`,
					"error",
				);
			}
		},
	});
}
