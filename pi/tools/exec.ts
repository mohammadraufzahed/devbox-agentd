/**
 * Exec tools: run shell commands inside the workspace's devbox environment.
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool } from "@earendil-works/pi-coding-agent";
import type { DevboxClient } from "../client.js";
import { jsonResult, tailTruncate, textResult, WorkspaceProp, wsRoot, makeCall } from "./common.js";

interface ExecRunResult {
	id: string;
	exitCode: number;
	output: string;
}

export function execTools(client: DevboxClient) {
	const call = makeCall(client);
	return [
		defineTool({
			name: "devbox_exec",
			label: "Devbox: exec",
			description:
				"Run a shell command in the project workspace via devbox-agentd. Inside a devbox project the command runs under `devbox run`, so the project's pinned toolchain (node, python, go, ...) is available. Streams output and returns it tail-truncated; use devbox_exec_output for full output.",
			promptSnippet: "devbox_exec — run a shell command inside the project's devbox environment",
			promptGuidelines: [
				"Use devbox_exec instead of bash for anything that needs the project's toolchain.",
			],
			parameters: Type.Object({
				command: Type.String({ description: "Shell command to run" }),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, onUpdate, ctx) {
				let streamed = "";
				const res = await call<ExecRunResult>(
					"exec.run",
					{ command: params.command, workspace: wsRoot(params.workspace, ctx.cwd) },
					{
						signal,
						onNotification: (p) => {
							if (typeof p.data === "string") {
								streamed += p.data;
								onUpdate?.(textResult(tailTruncate(streamed).text));
							}
						},
					},
				);
				const { text, truncated } = tailTruncate(res.output);
				const note = truncated
					? `\n\n[output truncated — full output via devbox_exec_output { id: "${res.id}" }]`
					: "";
				return textResult(`${text}${note}\n\nexitCode: ${res.exitCode}`, {
					id: res.id,
					exitCode: res.exitCode,
					truncated,
				});
			},
		}),
		defineTool({
			name: "devbox_exec_output",
			label: "Devbox: exec output",
			description: "Fetch the full buffered output of a previous devbox_exec call by its id.",
			promptSnippet: "devbox_exec_output — fetch full output of a previous devbox_exec",
			parameters: Type.Object({
				id: Type.String({ description: "Exec id returned by devbox_exec" }),
			}),
			async execute(_id, params, _signal, _onUpdate, _ctx) {
				return jsonResult(await call("exec.output", { id: params.id }));
			},
		}),
	];
}
