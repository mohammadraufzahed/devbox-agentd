/**
 * Event tools: listen to daemon lifecycle events (exec.done, service.exited, ...).
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool } from "@earendil-works/pi-coding-agent";
import type { DevboxClient } from "../client.js";
import { RpcError } from "../client.js";
import { jsonResult, WorkspaceProp, wsRoot, makeCall } from "./common.js";

export function eventTools(client: DevboxClient) {
	const call = makeCall(client);
	return [
		defineTool({
			name: "devbox_events",
			label: "Devbox: listen for events",
			description:
				"Collect daemon lifecycle events (exec.done, service.started, service.exited, pty.exited, workspace.destroyed) for up to timeoutSec, then return them. Use after starting background execs/services instead of polling. Optional types filter and workspace scoping.",
			promptSnippet: "devbox_events — collect daemon events for N seconds",
			parameters: Type.Object({
				timeoutSec: Type.Optional(
					Type.Number({ description: "How long to listen (default 20)" }),
				),
				types: Type.Optional(
					Type.Array(Type.String(), {
						description: "Only these event types, e.g. [\"exec.done\", \"service.exited\"]",
					}),
				),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				const events: Record<string, unknown>[] = [];
				const timeoutSec = params.timeoutSec ?? 20;
				const timeout = AbortSignal.timeout(timeoutSec * 1000);
				// Combine the tool's signal with our timeout: whichever fires first.
				const combined = signal
					? AbortSignal.any([signal, timeout])
					: timeout;
				try {
					await call(
						"events.subscribe",
						{
							types: params.types,
							workspace: params.workspace ? wsRoot(params.workspace, ctx.cwd) : undefined,
						},
						{
							signal: combined,
							onNotification: (p) => {
								if (typeof p.type === "string") {
									events.push({
										type: p.type,
										workspace: p.workspace,
										id: p.id,
										name: p.name,
										data: p.data,
										at: p.at,
									});
								}
							},
						},
					);
				} catch (e) {
					// Cancellation (timeout or user abort) just ends the collection.
					const cancelled =
						(e instanceof RpcError && e.code === -32800) ||
						combined.aborted ||
						(e instanceof Error && /cancel/i.test(e.message));
					if (!cancelled) throw e;
				}
				return jsonResult({ events, listenedSec: timeoutSec });
			},
		}),
	];
}
