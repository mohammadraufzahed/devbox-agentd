/**
 * Service tools: manage long-running processes per workspace.
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool } from "@earendil-works/pi-coding-agent";
import type { DevboxClient } from "../client.js";
import { jsonResult, tailTruncate, textResult, WorkspaceProp, wsRoot, makeCall } from "./common.js";

export function serviceTools(client: DevboxClient) {
	const call = makeCall(client);
	const nameProp = Type.String({ description: "Service name" });
	return [
		defineTool({
			name: "devbox_service_list",
			label: "Devbox: list services",
			description: "List services managed by devbox-agentd, optionally for one workspace.",
			promptSnippet: "devbox_service_list — list devbox-managed services",
			parameters: Type.Object({ workspace: WorkspaceProp }),
			async execute(_id, params, _signal, _onUpdate, ctx) {
				return jsonResult(
					await call("service.list", { workspace: wsRoot(params.workspace, ctx.cwd) }),
				);
			},
		}),
		defineTool({
			name: "devbox_service_start",
			label: "Devbox: start service",
			description:
				"Start a named long-running service (e.g. a dev server) in the workspace's devbox environment. Survives the current Pi session.",
			promptSnippet: "devbox_service_start — start a named long-running service",
			executionMode: "sequential",
			parameters: Type.Object({
				name: nameProp,
				command: Type.String({ description: "Command to run" }),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"service.start",
						{
							name: params.name,
							command: params.command,
							workspace: wsRoot(params.workspace, ctx.cwd),
						},
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_service_stop",
			label: "Devbox: stop service",
			description: "Stop a running service. Destructive — requires confirmation.",
			promptSnippet: "devbox_service_stop — stop a service (destructive)",
			executionMode: "sequential",
			parameters: Type.Object({ name: nameProp, workspace: WorkspaceProp }),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"service.stop",
						{ name: params.name, workspace: wsRoot(params.workspace, ctx.cwd) },
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_service_restart",
			label: "Devbox: restart service",
			description: "Restart a service with its original command. Destructive — requires confirmation.",
			promptSnippet: "devbox_service_restart — restart a service (destructive)",
			executionMode: "sequential",
			parameters: Type.Object({ name: nameProp, workspace: WorkspaceProp }),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"service.restart",
						{ name: params.name, workspace: wsRoot(params.workspace, ctx.cwd) },
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_service_logs",
			label: "Devbox: service logs",
			description:
				"Fetch buffered logs of a service. With follow=true streams logs until the call is cancelled.",
			promptSnippet: "devbox_service_logs — read or follow service logs",
			parameters: Type.Object({
				name: nameProp,
				follow: Type.Optional(Type.Boolean({ description: "Stream logs until cancelled" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, onUpdate, ctx) {
				if (!params.follow) {
					return jsonResult(
						await call(
							"service.logs",
							{ name: params.name, workspace: wsRoot(params.workspace, ctx.cwd) },
							{ signal },
						),
					);
				}
				let buf = "";
				await call<Record<string, unknown>>(
					"service.logs",
					{
						name: params.name,
						follow: true,
						workspace: wsRoot(params.workspace, ctx.cwd),
					},
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
				return textResult(tailTruncate(buf).text);
			},
		}),
	];
}
