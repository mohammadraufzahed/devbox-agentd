// Package mcp exposes the daemon's methods over a second Unix socket using
// newline-delimited MCP JSON-RPC (stdio framing). Each accepted connection
// is an independent MCP session; the daemon never prefixes tool names —
// clients such as pi-mcp-adapter add their own server-name prefix.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"sync"

	"devbox-agentd/internal/rpc"
	"devbox-agentd/internal/state"
)

// protocolVersion is the MCP revision we speak.
const protocolVersion = "2024-11-05"

// tool describes one MCP tool.
type tool struct {
	name        string
	description string
	schema      map[string]any
	// method is the internal JSON-RPC method it maps to.
	method string
	// rename maps MCP argument names to internal param names.
	rename map[string]string
}

// Adapter serves MCP on a listener, dispatching to api handlers.
type Adapter struct {
	log *slog.Logger
	st  *state.Store
	api *apiCaller
}

// apiCaller invokes internal handlers directly (in-process).
type apiCaller struct {
	handlers map[string]rpc.Handler
}

// NewAdapter wires the MCP adapter to the same handlers as the JSON-RPC
// server. srvHandlers must be the handler map of the api rpc.Server —
// we re-register methods on an internal dispatcher instead.
func NewAdapter(log *slog.Logger, st *state.Store, dispatch map[string]rpc.Handler) *Adapter {
	return &Adapter{log: log, st: st, api: &apiCaller{handlers: dispatch}}
}

// Serve accepts MCP connections until ctx is done.
func (a *Adapter) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go a.handleConn(c)
	}
}

func (a *Adapter) handleConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	var wmu sync.Mutex
	bw := bufio.NewWriter(c)
	write := func(v any) {
		wmu.Lock()
		defer wmu.Unlock()
		_ = json.NewEncoder(bw).Encode(v)
		_ = bw.Flush()
	}

	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var req rpc.Request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			write(resp(req.ID, nil, rpc.Errf(rpc.CodeParseError, "%v", err)))
			continue
		}
		if req.IsNotification() {
			continue // notifications/initialized etc.
		}
		go a.dispatch(&req, write)
	}
}

type respMsg = map[string]any

func resp(id json.RawMessage, result any, e *rpc.Error) respMsg {
	m := respMsg{"jsonrpc": rpc.Version, "id": id}
	if e != nil {
		m["error"] = e
	} else {
		m["result"] = result
	}
	return m
}

func (a *Adapter) dispatch(req *rpc.Request, write func(any)) {
	switch req.Method {
	case "initialize":
		write(resp(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "devbox-agentd", "version": "0.2.0"},
		}, nil))
	case "ping":
		write(resp(req.ID, map[string]any{}, nil))
	case "tools/list":
		write(resp(req.ID, map[string]any{"tools": toolList()}, nil))
	case "tools/call":
		a.toolsCall(req, write)
	case "resources/list":
		write(resp(req.ID, map[string]any{"resources": []any{}}, nil))
	case "prompts/list":
		write(resp(req.ID, map[string]any{"prompts": []any{}}, nil))
	default:
		write(resp(req.ID, nil, rpc.Errf(rpc.CodeMethodNotFound, "method not found: %s", req.Method)))
	}
}

func (a *Adapter) toolsCall(req *rpc.Request, write func(any)) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := rpc.DecodeParams(req.Params, &p); err != nil {
		write(resp(req.ID, nil, err.(*rpc.Error)))
		return
	}
	t := findTool(p.Name)
	if t == nil {
		write(resp(req.ID, nil, rpc.Errf(rpc.CodeInvalidParams, "unknown tool %q", p.Name)))
		return
	}
	args := map[string]any{}
	for k, v := range p.Arguments {
		if to, ok := t.rename[k]; ok {
			args[to] = v
		} else {
			args[k] = v
		}
	}
	h, ok := a.api.handlers[t.method]
	if !ok {
		write(resp(req.ID, nil, rpc.Errf(rpc.CodeInternal, "no handler for %s", t.method)))
		return
	}
	raw, _ := json.Marshal(args)
	// MCP calls don't carry a streaming connection; pass a nil conn — only
	// exec.run/pty.open/service.logs(follow) notify, and conn.Notify on a
	// nil conn would panic, so guard in handler usage via safeNotify.
	res, err := h(context.Background(), noopConn, raw)
	text := ""
	isErr := false
	if err != nil {
		text = err.Error()
		isErr = true
	} else {
		b, _ := json.MarshalIndent(res, "", "  ")
		text = string(b)
	}
	write(resp(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}, nil))
}

// noopConn is a *rpc.Conn whose Notify is a no-op — MCP has no streaming.
var noopConn = rpc.NewNoopConn()

// --- tool table ---

func obj(props map[string]any, req ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": req}
}
func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

var wsProp = map[string]any{"workspace": str("Workspace root path; defaults to the most recently opened workspace")}

func withWS(props map[string]any) map[string]any {
	m := map[string]any{}
	for k, v := range props {
		m[k] = v
	}
	for k, v := range wsProp {
		m[k] = v
	}
	return m
}

var tools = []tool{
	{name: "workspace_open", description: "Open/bind a project workspace by root path",
		method: "workspace.open", schema: obj(map[string]any{"root": str("Project root path")}, "root")},
	{name: "workspace_list", description: "List open workspaces",
		method: "workspace.list", schema: obj(map[string]any{})},
	{name: "workspace_destroy", description: "Close a workspace and kill its services/pty sessions",
		method: "workspace.destroy", schema: obj(map[string]any{"root": str("Workspace root")}, "root")},
	{name: "workspace_bind", description: "Pin this connection's default workspace so calls without a workspace param resolve to it",
		method: "workspace.bind", schema: obj(map[string]any{"root": str("Workspace root")}, "root")},
	{name: "workspace_unbind", description: "Clear the connection's bound workspace",
		method: "workspace.unbind", schema: obj(map[string]any{})},
	{name: "exec", description: "Run a shell command inside the workspace (devbox environment when present); returns output and exitCode. Optional cwd (subdir), env map, timeoutSec, stdin, background (returns immediately, emits exec.done event)",
		method: "exec.run", schema: obj(withWS(map[string]any{
			"command":    str("Shell command to run"),
			"cwd":        str("Working dir relative to workspace root"),
			"env":        map[string]any{"type": "object", "description": "Extra env vars"},
			"timeoutSec": integer("Kill after N seconds"),
			"stdin":      str("Data piped to stdin"),
			"background": boolean("Run detached; returns id immediately"),
		}), "command")},
	{name: "exec_output", description: "Fetch buffered output of a previous exec call",
		method: "exec.output", schema: obj(map[string]any{"id": str("Exec id")}, "id")},
	{name: "exec_list", description: "List running and finished execs",
		method: "exec.list", schema: obj(withWS(map[string]any{}))},
	{name: "exec_cancel", description: "Kill a running exec by id",
		method: "exec.cancel", schema: obj(map[string]any{"id": str("Exec id")}, "id")},
	{name: "exec_wait", description: "Block until an exec finishes (timeoutSec cap)",
		method: "exec.wait", schema: obj(map[string]any{"id": str("Exec id"), "timeoutSec": integer("Max seconds to wait")}, "id")},
	{name: "fs_read", description: "Read a file inside the workspace (offset/limit byte-based)",
		method: "fs.read", schema: obj(withWS(map[string]any{"path": str("File path"), "offset": integer("Byte offset"), "limit": integer("Max bytes")}), "path")},
	{name: "fs_write", description: "Write a file inside the workspace (append flag appends)",
		method: "fs.write", schema: obj(withWS(map[string]any{"path": str("File path"), "content": str("Content"), "append": boolean("Append instead of overwrite")}), "path", "content")},
	{name: "fs_patch", description: "Apply exact-match text edits to a file (each old must match once)",
		method: "fs.patch", schema: obj(withWS(map[string]any{
			"path":  str("File path"),
			"edits": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"old": str("Exact text to replace"), "new": str("Replacement")}, "required": []string{"old", "new"}}},
		}), "path", "edits")},
	{name: "service_list", description: "List services, optionally for one workspace",
		method: "service.list", schema: obj(withWS(map[string]any{}))},
	{name: "service_start", description: "Start a named long-running service in the workspace. Optional cwd, env map; portEnv assigns a free TCP port into that env var (or use an explicit port)",
		method: "service.start", schema: obj(withWS(map[string]any{
			"name":    str("Service name"),
			"command": str("Command to run"),
			"cwd":     str("Working dir relative to workspace root"),
			"env":     map[string]any{"type": "object", "description": "Extra env vars"},
			"portEnv": str("Env var name to receive an assigned TCP port"),
			"port":    integer("Explicit port to assign (must be free)"),
		}), "name", "command")},
	{name: "service_stop", description: "Stop a running service",
		method: "service.stop", schema: obj(withWS(map[string]any{"name": str("Service name")}), "name")},
	{name: "service_restart", description: "Restart a service with its original command",
		method: "service.restart", schema: obj(withWS(map[string]any{"name": str("Service name")}), "name")},
	{name: "service_logs", description: "Fetch buffered service logs",
		method: "service.logs", schema: obj(withWS(map[string]any{"name": str("Service name")}), "name")},
	{name: "service_wait", description: "Block until a service is running (optionally accepting TCP on port) or timeout",
		method: "service.wait", schema: obj(withWS(map[string]any{"name": str("Service name"), "port": integer("Require this TCP port accepting"), "timeoutSec": integer("Max seconds to wait")}), "name")},
	{name: "pty_open", description: "Open an interactive process session; returns a pty id",
		method: "pty.open", schema: obj(withWS(map[string]any{"command": str("Command (default: sh)")}))},
	{name: "pty_write", description: "Write data to a pty session's stdin",
		method: "pty.write", schema: obj(map[string]any{"id": str("Pty id"), "data": str("Bytes to write")}, "id", "data")},
	{name: "pty_read", description: "Read buffered pty output",
		method: "pty.read", schema: obj(map[string]any{"id": str("Pty id")}, "id")},
	{name: "pty_kill", description: "Kill a pty session",
		method: "pty.kill", schema: obj(map[string]any{"id": str("Pty id")}, "id")},
	{name: "git_status", description: "git status --short --branch for the workspace",
		method: "git.status", schema: obj(withWS(map[string]any{}))},
	{name: "git_diff", description: "git diff for the workspace (staged flag for --staged)",
		method: "git.diff", schema: obj(withWS(map[string]any{"staged": boolean("Diff staged changes")}))},
	{name: "git_log", description: "Recent git history (limit, default 15)",
		method: "git.log", schema: obj(withWS(map[string]any{"limit": integer("Max commits")}))},
	{name: "git_commit", description: "git commit -m message (all flag for -a; paths stages specific files first)",
		method: "git.commit", schema: obj(withWS(map[string]any{
			"message": str("Commit message"),
			"all":     boolean("Stage tracked files (-a)"),
			"paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Stage these paths before committing"},
		}), "message")},
	{name: "git_add", description: "git add -- <paths> (stage specific files)",
		method: "git.add", schema: obj(withWS(map[string]any{"paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Paths to stage"}}), "paths")},
	{name: "git_fetch", description: "git fetch [--prune] [remote]",
		method: "git.fetch", schema: obj(withWS(map[string]any{"remote": str("Remote name"), "prune": boolean("Prune deleted remote refs")}))},
	{name: "git_pull", description: "git pull [--rebase]",
		method: "git.pull", schema: obj(withWS(map[string]any{"rebase": boolean("Pull with rebase")}))},
	{name: "git_push", description: "git push [--set-upstream|--force-with-lease] [remote] [branch] (publishes work)",
		method: "git.push", schema: obj(withWS(map[string]any{"remote": str("Remote name"), "branch": str("Branch"), "setUpstream": boolean("Set upstream"), "force": boolean("Force with lease")}))},
	{name: "git_branch", description: "List branches, or create/delete one",
		method: "git.branch", schema: obj(withWS(map[string]any{"name": str("Branch name"), "startPoint": str("Start point for new branch"), "delete": boolean("Delete the named branch")}))},
	{name: "git_stash", description: "git stash: list|push|pop|apply|drop (message for push)",
		method: "git.stash", schema: obj(withWS(map[string]any{"action": str("list|push|pop|apply|drop"), "message": str("Stash message")}))},
	{name: "git_show", description: "git show <ref> or <ref>:<path> (default HEAD)",
		method: "git.show", schema: obj(withWS(map[string]any{"ref": str("Ref"), "path": str("Path inside the ref")}))},
	{name: "git_checkout", description: "git checkout a branch/ref (destructive)",
		method: "git.checkout", schema: obj(withWS(map[string]any{"target": str("Branch or ref")}), "target")},
	{name: "git_reset", description: "git reset [--soft|--mixed|--hard] [target] (destructive)",
		method: "git.reset", schema: obj(withWS(map[string]any{"mode": str("soft|mixed|hard"), "target": str("Reset target")}))},
}

func toolList() []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.schema,
		})
	}
	return out
}

func findTool(name string) *tool {
	for i := range tools {
		if tools[i].name == name {
			return &tools[i]
		}
	}
	return nil
}
