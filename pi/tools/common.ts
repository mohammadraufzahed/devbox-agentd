/**
 * Shared helpers for devbox_* tools.
 */

import { Type, type JsonValue } from "@earendil-works/pi-ai";
import type { AgentToolResult } from "@earendil-works/pi-coding-agent";
import type { DevboxClient } from "../client.js";

/** Optional workspace root shared by every tool. */
export const WorkspaceProp = Type.Optional(
	Type.String({
		description:
			"Workspace root path (project dir). Defaults to the current working directory.",
	}),
);

/** Max chars of output returned to the model (tail-truncated). */
export const OUTPUT_TAIL_LIMIT = 16 * 1024;

export function tailTruncate(text: string, limit = OUTPUT_TAIL_LIMIT): { text: string; truncated: boolean } {
	if (text.length <= limit) return { text, truncated: false };
	return { text: text.slice(text.length - limit), truncated: true };
}

export function textResult(text: string, details?: JsonValue): AgentToolResult {
	return { content: [{ type: "text", text }], details };
}

export function jsonResult(v: unknown): AgentToolResult {
	return textResult(JSON.stringify(v, null, 2));
}

/**
 * Resolve the effective workspace root for a call.
 */
export function wsRoot(workspace: string | undefined, cwd: string): string {
	return workspace ?? cwd;
}

export type Call = <T = unknown>(
	method: string,
	params?: Record<string, unknown>,
	opts?: {
		signal?: AbortSignal;
		onNotification?: (p: Record<string, unknown>) => void;
	},
) => Promise<T>;

export function makeCall(client: DevboxClient): Call {
	return (m, p, o) => client.call(m, p, o);
}
