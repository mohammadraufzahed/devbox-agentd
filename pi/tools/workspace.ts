/**
 * Workspace tools: list/open/destroy project workspaces on the daemon.
 */

import { Type } from "@earendil-works/pi-ai";
import { defineTool } from "@earendil-works/pi-coding-agent";
import type { DevboxClient } from "../client.js";
import { jsonResult, makeCall } from "./common.js";

export function workspaceTools(client: DevboxClient) {
	const call = makeCall(client);
	return [
		defineTool({
			name: "devbox_workspace_list",
			label: "Devbox: list workspaces",
			description: "List workspaces currently open on the devbox-agentd daemon.",
			promptSnippet: "devbox_workspace_list — list open devbox workspaces",
			parameters: Type.Object({}),
			async execute(_id, _params, _signal, _onUpdate, _ctx) {
				return jsonResult(await call("workspace.list"));
			},
		}),
		defineTool({
			name: "devbox_workspace_open",
			label: "Devbox: open workspace",
			description: "Open/bind a project workspace (root path) on the daemon. Idempotent.",
			promptSnippet: "devbox_workspace_open — bind a project root as a devbox workspace",
			parameters: Type.Object({
				root: Type.String({ description: "Project root path" }),
			}),
			async execute(_id, params, _signal, _onUpdate, _ctx) {
				return jsonResult(await call("workspace.open", { root: params.root }));
			},
		}),
		defineTool({
			name: "devbox_workspace_bind",
			label: "Devbox: bind workspace",
			description:
				"Pin this session's default workspace so calls without a workspace param resolve to it — gives each agent session its own worktree when several share the daemon.",
			promptSnippet: "devbox_workspace_bind — pin the session's default workspace",
			parameters: Type.Object({
				root: Type.String({ description: "Workspace root path" }),
			}),
			async execute(_id, params, _signal, _onUpdate, _ctx) {
				return jsonResult(await call("workspace.bind", { root: params.root }));
			},
		}),
		defineTool({
			name: "devbox_workspace_destroy",
			label: "Devbox: destroy workspace",
			description:
				"Close a workspace and kill all its services and pty sessions. Destructive — requires confirmation.",
			promptSnippet: "devbox_workspace_destroy — close a workspace and kill its services (destructive)",
			executionMode: "sequential",
			parameters: Type.Object({
				root: Type.String({ description: "Workspace root path" }),
			}),
			async execute(_id, params, _signal, _onUpdate, _ctx) {
				return jsonResult(await call("workspace.destroy", { root: params.root }));
			},
		}),
	];
}
