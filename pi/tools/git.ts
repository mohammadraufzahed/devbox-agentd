/**
 * Git tools: run git operations inside the workspace via the daemon.
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool } from "@earendil-works/pi-coding-agent";
import type { DevboxClient } from "../client.js";
import { jsonResult, WorkspaceProp, wsRoot, makeCall } from "./common.js";

export function gitTools(client: DevboxClient) {
	const call = makeCall(client);
	return [
		defineTool({
			name: "devbox_git_status",
			label: "Devbox: git status",
			description: "git status --short --branch for the workspace.",
			promptSnippet: "devbox_git_status — git status of a workspace",
			parameters: Type.Object({ workspace: WorkspaceProp }),
			async execute(_id, params, _signal, _onUpdate, ctx) {
				return jsonResult(
					await call("git.status", { workspace: wsRoot(params.workspace, ctx.cwd) }),
				);
			},
		}),
		defineTool({
			name: "devbox_git_diff",
			label: "Devbox: git diff",
			description: "git diff for the workspace (staged=true for --staged).",
			promptSnippet: "devbox_git_diff — git diff of a workspace",
			parameters: Type.Object({
				staged: Type.Optional(Type.Boolean({ description: "Diff staged changes" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, _signal, _onUpdate, ctx) {
				return jsonResult(
					await call("git.diff", {
						staged: params.staged,
						workspace: wsRoot(params.workspace, ctx.cwd),
					}),
				);
			},
		}),
		defineTool({
			name: "devbox_git_log",
			label: "Devbox: git log",
			description: "Recent git history (default 15 commits).",
			promptSnippet: "devbox_git_log — recent git history",
			parameters: Type.Object({
				limit: Type.Optional(Type.Number({ description: "Max commits (default 15)" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, _signal, _onUpdate, ctx) {
				return jsonResult(
					await call("git.log", {
						limit: params.limit,
						workspace: wsRoot(params.workspace, ctx.cwd),
					}),
				);
			},
		}),
		defineTool({
			name: "devbox_git_commit",
			label: "Devbox: git commit",
			description: "git commit -m <message> (all=true adds -a).",
			promptSnippet: "devbox_git_commit — commit with a message",
			executionMode: "sequential",
			parameters: Type.Object({
				message: Type.String({ description: "Commit message" }),
				all: Type.Optional(Type.Boolean({ description: "Stage tracked files (git -a)" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"git.commit",
						{
							message: params.message,
							all: params.all,
							workspace: wsRoot(params.workspace, ctx.cwd),
						},
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_git_checkout",
			label: "Devbox: git checkout",
			description: "git checkout a branch/ref. Destructive — requires confirmation.",
			promptSnippet: "devbox_git_checkout — checkout a branch/ref (destructive)",
			executionMode: "sequential",
			parameters: Type.Object({
				target: Type.String({ description: "Branch or ref to check out" }),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"git.checkout",
						{ target: params.target, workspace: wsRoot(params.workspace, ctx.cwd) },
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_git_reset",
			label: "Devbox: git reset",
			description: "git reset [--soft|--mixed|--hard] [target]. Destructive — requires confirmation.",
			promptSnippet: "devbox_git_reset — reset the workspace HEAD (destructive)",
			executionMode: "sequential",
			parameters: Type.Object({
				mode: Type.Optional(
					Type.Union([Type.Literal("soft"), Type.Literal("mixed"), Type.Literal("hard")], {
						description: "Reset mode",
					}),
				),
				target: Type.Optional(Type.String({ description: "Reset target (default HEAD)" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"git.reset",
						{
							mode: params.mode,
							target: params.target,
							workspace: wsRoot(params.workspace, ctx.cwd),
						},
						{ signal },
					),
				);
			},
		}),
	];
}
