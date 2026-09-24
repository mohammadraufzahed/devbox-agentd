/**
 * Pty tools: interactive process sessions (pipe-backed) on the daemon.
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool } from "@earendil-works/pi-coding-agent";
import type { DevboxClient } from "../client.js";
import { jsonResult, tailTruncate, textResult, WorkspaceProp, wsRoot, makeCall } from "./common.js";

const idProp = Type.String({ description: "Pty id returned by devbox_pty_open" });

export function ptyTools(client: DevboxClient) {
	const call = makeCall(client);
	return [
		defineTool({
			name: "devbox_pty_open",
			label: "Devbox: open pty",
			description:
				"Open an interactive process session (default: sh) in the workspace. Streams output; use devbox_pty_write to send input.",
			promptSnippet: "devbox_pty_open — open an interactive process session",
			parameters: Type.Object({
				command: Type.Optional(Type.String({ description: "Command to run (default: sh)" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, onUpdate, ctx) {
				let buf = "";
				const res = await call<{ id: string }>(
					"pty.open",
					{ command: params.command, workspace: wsRoot(params.workspace, ctx.cwd) },
					{
						signal,
						onNotification: (p) => {
							if (typeof p.data === "string") {
								buf += p.data;
								onUpdate?.(textResult(tailTruncate(buf).text));
							}
						},
					},
				);
				return jsonResult(res);
			},
		}),
		defineTool({
			name: "devbox_pty_write",
			label: "Devbox: pty write",
			description: "Write data to a pty session's stdin.",
			promptSnippet: "devbox_pty_write — send input to a pty session",
			parameters: Type.Object({
				id: idProp,
				data: Type.String({ description: "Data to write (include \\n for Enter)" }),
			}),
			async execute(_id, params, signal, _onUpdate, _ctx) {
				return jsonResult(await call("pty.write", { id: params.id, data: params.data }, { signal }));
			},
		}),
		defineTool({
			name: "devbox_pty_read",
			label: "Devbox: pty read",
			description: "Read the buffered output of a pty session.",
			promptSnippet: "devbox_pty_read — read buffered pty output",
			parameters: Type.Object({ id: idProp }),
			async execute(_id, params, _signal, _onUpdate, _ctx) {
				return jsonResult(await call("pty.read", { id: params.id }));
			},
		}),
		defineTool({
			name: "devbox_pty_kill",
			label: "Devbox: pty kill",
			description: "Kill a pty session. Destructive — requires confirmation.",
			promptSnippet: "devbox_pty_kill — kill a pty session (destructive)",
			executionMode: "sequential",
			parameters: Type.Object({ id: idProp }),
			async execute(_id, params, signal, _onUpdate, _ctx) {
				return jsonResult(await call("pty.kill", { id: params.id }, { signal }));
			},
		}),
	];
}
