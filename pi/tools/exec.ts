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
	done?: boolean;
	timedOut?: boolean;
	signal?: string;
	durationMs?: number;
	background?: boolean;
	pid?: number;
}

export function execTools(client: DevboxClient) {
	const call = makeCall(client);
	return [
		defineTool({
			name: "devbox_exec",
			label: "Devbox: exec",
			description:
				"Run a shell command in the project workspace via devbox-agentd. Inside a devbox project the command runs under `devbox run`, so the project's pinned toolchain (node, python, go, ...) is available. Streams output and returns it tail-truncated; use devbox_exec_output for full output. background=true returns immediately — use devbox_exec_wait or devbox_events to collect the result later.",
			promptSnippet: "devbox_exec — run a shell command inside the project's devbox environment",
			promptGuidelines: [
				"Use devbox_exec instead of bash for anything that needs the project's toolchain.",
				"Put secrets in `env`, not in the command string — commands are logged.",
			],
			parameters: Type.Object({
				command: Type.String({ description: "Shell command to run" }),
				cwd: Type.Optional(
					Type.String({ description: "Working directory relative to the workspace root" }),
				),
				env: Type.Optional(
					Type.Record(Type.String(), Type.String(), {
						description:
							"Extra environment variables (KEY=value). Preferred over embedding secrets in the command.",
					}),
				),
				timeoutSec: Type.Optional(
					Type.Number({ description: "Kill the command after this many seconds" }),
				),
				stdin: Type.Optional(
					Type.String({ description: "Data piped to the command's stdin" }),
				),
				background: Type.Optional(
					Type.Boolean({
						description:
							"Run detached and return the exec id immediately; completion emits an exec.done event",
					}),
				),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, onUpdate, ctx) {
				let streamed = "";
				const res = await call<ExecRunResult>(
					"exec.run",
					{
						command: params.command,
						cwd: params.cwd,
						env: params.env,
						timeoutSec: params.timeoutSec,
						stdin: params.stdin,
						background: params.background,
						workspace: wsRoot(params.workspace, ctx.cwd),
					},
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
				if (res.background) {
					return textResult(
						`exec started in background: id=${res.id} pid=${res.pid}\nCollect with devbox_exec_wait { id: "${res.id}" } or devbox_exec_output.`,
						{ id: res.id, background: true, pid: res.pid ?? 0 },
					);
				}
				const { text, truncated } = tailTruncate(res.output);
				const note = truncated
					? `\n\n[output truncated — full output via devbox_exec_output { id: "${res.id}" }]`
					: "";
				const meta = [
					`exitCode: ${res.exitCode}`,
					res.timedOut ? "timedOut: true" : "",
					res.signal ? `signal: ${res.signal}` : "",
					res.durationMs != null ? `durationMs: ${res.durationMs}` : "",
				]
					.filter(Boolean)
					.join("\n");
				return textResult(`${text}${note}\n\n${meta}`, {
					id: res.id,
					exitCode: res.exitCode,
					truncated,
					timedOut: res.timedOut ?? false,
					signal: res.signal ?? "",
					durationMs: res.durationMs ?? 0,
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
		defineTool({
			name: "devbox_exec_list",
			label: "Devbox: list execs",
			description: "List running and finished execs (optionally for one workspace).",
			promptSnippet: "devbox_exec_list — list execs on the daemon",
			parameters: Type.Object({ workspace: WorkspaceProp }),
			async execute(_id, params, _signal, _onUpdate, ctx) {
				return jsonResult(
					await call("exec.list", { workspace: wsRoot(params.workspace, ctx.cwd) }),
				);
			},
		}),
		defineTool({
			name: "devbox_exec_cancel",
			label: "Devbox: cancel exec",
			description: "Kill a running exec by id. Destructive — requires confirmation.",
			promptSnippet: "devbox_exec_cancel — kill a running exec (destructive)",
			executionMode: "sequential",
			parameters: Type.Object({
				id: Type.String({ description: "Exec id to cancel" }),
			}),
			async execute(_id, params, signal, _onUpdate, _ctx) {
				return jsonResult(await call("exec.cancel", { id: params.id }, { signal }));
			},
		}),
		defineTool({
			name: "devbox_exec_wait",
			label: "Devbox: wait for exec",
			description:
				"Block until a background devbox_exec finishes (or timeoutSec elapses) and return its output and exit code.",
			promptSnippet: "devbox_exec_wait — block until a background exec finishes",
			parameters: Type.Object({
				id: Type.String({ description: "Exec id returned by devbox_exec" }),
				timeoutSec: Type.Optional(
					Type.Number({ description: "Max seconds to wait (default 600)" }),
				),
			}),
			async execute(_id, params, signal, _onUpdate, _ctx) {
				const res = await call<ExecRunResult>(
					"exec.wait",
					{ id: params.id, timeoutSec: params.timeoutSec },
					{ signal },
				);
				const { text, truncated } = tailTruncate(res.output);
				const note = truncated
					? `\n\n[output truncated — full output via devbox_exec_output { id: "${res.id}" }]`
					: "";
				return textResult(
					`${text}${note}\n\nexitCode: ${res.exitCode}${res.timedOut ? " (timed out)" : ""}`,
					{ id: res.id, exitCode: res.exitCode, timedOut: res.timedOut ?? false, truncated },
				);
			},
		}),
	];
}
