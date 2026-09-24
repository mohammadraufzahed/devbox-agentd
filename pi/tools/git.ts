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
			description:
				"git commit -m <message> (all=true adds -a; paths stages specific files first).",
			promptSnippet: "devbox_git_commit — commit with a message",
			executionMode: "sequential",
			parameters: Type.Object({
				message: Type.String({ description: "Commit message" }),
				all: Type.Optional(Type.Boolean({ description: "Stage tracked files (git -a)" })),
				paths: Type.Optional(
					Type.Array(Type.String(), {
						description: "Stage these paths before committing (mutually exclusive with all)",
					}),
				),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"git.commit",
						{
							message: params.message,
							all: params.all,
							paths: params.paths,
							workspace: wsRoot(params.workspace, ctx.cwd),
						},
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_git_add",
			label: "Devbox: git add",
			description: "Stage specific paths (git add -- <paths>).",
			promptSnippet: "devbox_git_add — stage specific paths",
			executionMode: "sequential",
			parameters: Type.Object({
				paths: Type.Array(Type.String(), { description: "Paths to stage" }),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"git.add",
						{ paths: params.paths, workspace: wsRoot(params.workspace, ctx.cwd) },
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_git_fetch",
			label: "Devbox: git fetch",
			description: "git fetch [--prune] [remote].",
			promptSnippet: "devbox_git_fetch — fetch from a remote",
			parameters: Type.Object({
				remote: Type.Optional(Type.String({ description: "Remote name" })),
				prune: Type.Optional(Type.Boolean({ description: "Prune deleted remote refs" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"git.fetch",
						{
							remote: params.remote,
							prune: params.prune,
							workspace: wsRoot(params.workspace, ctx.cwd),
						},
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_git_pull",
			label: "Devbox: git pull",
			description: "git pull [--rebase].",
			promptSnippet: "devbox_git_pull — pull from upstream",
			executionMode: "sequential",
			parameters: Type.Object({
				rebase: Type.Optional(Type.Boolean({ description: "Pull with rebase" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"git.pull",
						{ rebase: params.rebase, workspace: wsRoot(params.workspace, ctx.cwd) },
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_git_push",
			label: "Devbox: git push",
			description:
				"git push [--set-upstream|--force-with-lease] [remote] [branch]. Publishes work — requires confirmation.",
			promptSnippet: "devbox_git_push — push to a remote (destructive)",
			executionMode: "sequential",
			parameters: Type.Object({
				remote: Type.Optional(Type.String({ description: "Remote name (e.g. origin)" })),
				branch: Type.Optional(Type.String({ description: "Branch to push" })),
				setUpstream: Type.Optional(Type.Boolean({ description: "Set upstream (-u)" })),
				force: Type.Optional(
					Type.Boolean({ description: "Force push (uses --force-with-lease)" }),
				),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"git.push",
						{
							remote: params.remote,
							branch: params.branch,
							setUpstream: params.setUpstream,
							force: params.force,
							workspace: wsRoot(params.workspace, ctx.cwd),
						},
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_git_branch",
			label: "Devbox: git branch",
			description: "List branches, create one (name, optional startPoint), or delete (delete=true).",
			promptSnippet: "devbox_git_branch — list/create/delete branches",
			parameters: Type.Object({
				name: Type.Optional(Type.String({ description: "Branch name" })),
				startPoint: Type.Optional(
					Type.String({ description: "Start point for the new branch" }),
				),
				delete: Type.Optional(Type.Boolean({ description: "Delete the named branch" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"git.branch",
						{
							name: params.name,
							startPoint: params.startPoint,
							delete: params.delete,
							workspace: wsRoot(params.workspace, ctx.cwd),
						},
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_git_stash",
			label: "Devbox: git stash",
			description: "git stash operations: list (default), push, pop, apply, drop.",
			promptSnippet: "devbox_git_stash — stash operations",
			executionMode: "sequential",
			parameters: Type.Object({
				action: Type.Optional(
					Type.Union(
						[
							Type.Literal("list"),
							Type.Literal("push"),
							Type.Literal("pop"),
							Type.Literal("apply"),
							Type.Literal("drop"),
						],
						{ description: "Stash action (default list)" },
					),
				),
				message: Type.Optional(Type.String({ description: "Stash message (push only)" })),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"git.stash",
						{
							action: params.action,
							message: params.message,
							workspace: wsRoot(params.workspace, ctx.cwd),
						},
						{ signal },
					),
				);
			},
		}),
		defineTool({
			name: "devbox_git_show",
			label: "Devbox: git show",
			description: "git show <ref> or <ref>:<path> (default HEAD) — commit details or file contents at a ref.",
			promptSnippet: "devbox_git_show — show a commit or file at a ref",
			parameters: Type.Object({
				ref: Type.Optional(Type.String({ description: "Ref (default HEAD)" })),
				path: Type.Optional(
					Type.String({ description: "File path inside the ref (shows its contents)" }),
				),
				workspace: WorkspaceProp,
			}),
			async execute(_id, params, signal, _onUpdate, ctx) {
				return jsonResult(
					await call(
						"git.show",
						{ ref: params.ref, path: params.path, workspace: wsRoot(params.workspace, ctx.cwd) },
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
