/**
 * Filesystem tools: read/write/patch files inside the workspace via the daemon.
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool } from "@earendil-works/pi-coding-agent";
import type { DevboxClient } from "../client.js";
import { jsonResult, WorkspaceProp, wsRoot, makeCall } from "./common.js";

export function fsTools(client: DevboxClient) {
	const call = makeCall(client);
	return [
		defineTool({
			name: "devbox_fs_read",
			label: "Devbox: read file",
			description:
				"Read a file inside the workspace via devbox-agentd. offset/limit are byte-based for partial reads; returns size and truncated flag.",
			promptSnippet: "devbox_fs_read — read a workspace file via the daemon",
			parameters: Type.Object({
				path: Type.String({ description: "File path (relative to workspace root or absolute inside it)" }),
				offset: Type.Optional(Type.Number({ description: "Byte offset to start at" })),
				limit: Type.Optional(Type.Number({ description: "Max bytes to return" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"fs.read",
						{
							path: params.path,
							offset: params.offset,
							limit: params.limit,
							workspace: wsRoot(params.workspace, ctx.cwd),
						},
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_fs_write",
			label: "Devbox: write file",
			description:
				"Write a file inside the workspace via devbox-agentd (creates parent dirs; append=true appends).",
			promptSnippet: "devbox_fs_write — write a workspace file via the daemon",
			parameters: Type.Object({
				path: Type.String({ description: "File path" }),
				content: Type.String({ description: "Content to write" }),
				append: Type.Optional(Type.Boolean({ description: "Append instead of overwrite" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"fs.write",
						{
							path: params.path,
							content: params.content,
							append: params.append,
							workspace: wsRoot(params.workspace, ctx.cwd),
						},
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_fs_patch",
			label: "Devbox: patch file",
			description:
				"Apply exact-match text replacements to a workspace file via devbox-agentd. Each edits[].old must match exactly once; all-or-nothing.",
			promptSnippet: "devbox_fs_patch — exact-match edits on a workspace file",
			parameters: Type.Object({
				path: Type.String({ description: "File path" }),
				edits: Type.Array(
					Type.Object({
						old: Type.String({ description: "Exact text to replace (must match once)" }),
						new: Type.String({ description: "Replacement text" }),
					}),
					{ description: "Exact-match replacements, applied in order" },
				),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"fs.patch",
						{
							path: params.path,
							edits: params.edits,
							workspace: wsRoot(params.workspace, ctx.cwd),
						},
						{ signal },
					),
				);
			},
		}),
	];
}
