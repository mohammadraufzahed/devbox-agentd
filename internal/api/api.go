// Package api implements the daemon's JSON-RPC method surface.
package api

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"devbox-agentd/internal/rpc"
	"devbox-agentd/internal/state"
)

// API wires the method handlers onto an rpc.Server.
type API struct {
	st      *state.Store
	started time.Time
	sock    string
	// Dispatch maps method names to handlers; shared with the MCP adapter
	// for in-process calls.
	Dispatch map[string]rpc.Handler
}

// New registers all methods on srv.
func New(srv *rpc.Server, st *state.Store, sock string) *API {
	a := &API{st: st, started: time.Now(), sock: sock, Dispatch: map[string]rpc.Handler{}}
	reg := func(method string, h rpc.Handler) {
		srv.Handle(method, h)
		a.Dispatch[method] = h
	}
	reg("ping", a.Ping)
	reg("status", a.Status)

	reg("workspace.open", a.WorkspaceOpen)
	reg("workspace.list", a.WorkspaceList)
	reg("workspace.destroy", a.WorkspaceDestroy)

	reg("exec.run", a.ExecRun)
	reg("exec.output", a.ExecOutput)

	reg("service.list", a.ServiceList)
	reg("service.start", a.ServiceStart)
	reg("service.stop", a.ServiceStop)
	reg("service.restart", a.ServiceRestart)
	reg("service.logs", a.ServiceLogs)

	reg("pty.open", a.PtyOpen)
	reg("pty.write", a.PtyWrite)
	reg("pty.read", a.PtyRead)
	reg("pty.kill", a.PtyKill)

	reg("git.status", a.GitStatus)
	reg("git.diff", a.GitDiff)
	reg("git.log", a.GitLog)
	reg("git.commit", a.GitCommit)
	reg("git.checkout", a.GitCheckout)
	reg("git.reset", a.GitReset)
	return a
}

// workspaceParam is embedded by every workspace-scoped request.
type workspaceParam struct {
	// Workspace is the project root path; defaults to the most recently
	// opened workspace.
	Workspace string `json:"workspace,omitempty"`
}

func (a *API) workspace(p workspaceParam) (*state.Workspace, error) {
	return a.st.ResolveWorkspace(p.Workspace)
}

// Ping returns daemon liveness info.
func (a *API) Ping(_ context.Context, _ *rpc.Conn, _ json.RawMessage) (any, error) {
	return map[string]any{
		"pid":     os.Getpid(),
		"socket":  a.sock,
		"uptimeS": int(time.Since(a.started).Seconds()),
	}, nil
}

// Status returns a full daemon snapshot for the /devbox command.
func (a *API) Status(_ context.Context, _ *rpc.Conn, _ json.RawMessage) (any, error) {
	ws := a.st.ListWorkspaces()
	svcs := a.st.ListServices("")
	return map[string]any{
		"pid":        os.Getpid(),
		"socket":     a.sock,
		"uptimeS":    int(time.Since(a.started).Seconds()),
		"workspaces": ws,
		"services":   svcs,
	}, nil
}

// shellBin/shellFlag run a command through the POSIX shell. This is the
// daemon's purpose: exec.run accepts an arbitrary shell command by design
// (same trust model as the agent's own bash tool), so this is not a
// command-injection sink.
const (
	shellBin  = "sh"
	shellFlag = "-c"
)

// runCmd builds the *exec.Cmd for a shell command inside ws. When the
// workspace has a devbox.json and devbox is on PATH, the command runs via
// `devbox run --` so it sees the project's toolchain.
func runCmd(ctx context.Context, ws *state.Workspace, command string) *exec.Cmd {
	if ws.Devbox {
		if _, err := exec.LookPath("devbox"); err == nil {
			// Wrap inside the shell so the process entry point stays the
			// constant shell binary: `devbox run -- sh -c <command>`.
			wrapped := "exec devbox run -- " + shellBin + " " + shellFlag + " " + shellQuote(command)
			c := exec.CommandContext(ctx, shellBin, shellFlag, wrapped)
			c.Dir = ws.Root
			return c
		}
	}
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- running the caller's shell command is the documented purpose of exec.run.
	c := exec.CommandContext(ctx, shellBin, shellFlag, command)
	c.Dir = ws.Root
	return c
}

// shellQuote single-quotes s for POSIX shell embedding.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- workspaces ---

// WorkspaceOpen binds a project root.
func (a *API) WorkspaceOpen(_ context.Context, _ *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		Root string `json:"root"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	ws, err := a.st.OpenWorkspace(p.Root)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	return ws, nil
}

// WorkspaceList returns all open workspaces.
func (a *API) WorkspaceList(_ context.Context, _ *rpc.Conn, _ json.RawMessage) (any, error) {
	return a.st.ListWorkspaces(), nil
}

// WorkspaceDestroy closes a workspace and kills its services/pty sessions.
func (a *API) WorkspaceDestroy(_ context.Context, _ *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		Root string `json:"root"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	cleanup, err := a.st.DestroyWorkspace(p.Root)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	cleanup()
	return map[string]any{"destroyed": true, "root": p.Root}, nil
}

// --- exec ---

const execBufCap = 4 * 1024 * 1024

// ExecRun runs a shell command in the workspace, streaming output as
// exec.output notifications {id, reqId, data} on the calling connection.
// Cancel via `$/cancel`.
func (a *API) ExecRun(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Command string `json:"command"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Command == "" {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "command is required")
	}
	ws, err := a.workspace(p.workspaceParam)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}

	e := &state.Exec{
		ID:        a.st.NewID("exec"),
		Workspace: ws.Root,
		Command:   p.Command,
		StartedAt: time.Now(),
		Output:    state.NewRing(execBufCap),
	}
	a.st.PutExec(e)

	reqID := rpc.ReqID(ctx)
	// Stream combined stdout+stderr.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInternal, "%v", err)
	}
	cmd := runCmd(ctx, ws, p.Command)
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, rpc.Errf(rpc.CodeInternal, "start: %v", err)
	}

	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		buf := make([]byte, 32*1024)
		for {
			n, rerr := pr.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				_, _ = e.Output.Write(chunk)
				_ = conn.Notify("exec.output", map[string]any{
					"id": e.ID, "reqId": reqID, "data": string(chunk),
				})
			}
			if rerr != nil {
				return
			}
		}
	}()

	werr := cmd.Wait()
	_ = pw.Close()
	<-copyDone
	_ = pr.Close()

	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		code = -1
	} else if werr != nil && code == 0 {
		code = 1
	}
	e.ExitCode = &code
	e.Done = true

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return map[string]any{
		"id":       e.ID,
		"exitCode": code,
		"output":   string(e.Output.Bytes()),
	}, nil
}

// ExecOutput returns the buffered output of a past or running exec.
func (a *API) ExecOutput(_ context.Context, _ *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		ID string `json:"id"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	e, ok := a.st.GetExec(p.ID)
	if !ok {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "unknown exec id %q", p.ID)
	}
	return map[string]any{
		"id":       e.ID,
		"done":     e.Done,
		"exitCode": e.ExitCode,
		"output":   string(e.Output.Bytes()),
	}, nil
}

// --- services ---

// ServiceStart spawns a named long-running process in the workspace.
func (a *API) ServiceStart(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Name    string `json:"name"`
		Command string `json:"command"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Name == "" || p.Command == "" {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "name and command are required")
	}
	ws, err := a.workspace(p.workspaceParam)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	if existing, ok := a.st.GetService(ws.Root, p.Name); ok {
		existing.Kill()
	}
	svc := &state.Service{
		Name:      p.Name,
		Workspace: ws.Root,
		Command:   p.Command,
		Logs:      state.NewRing(execBufCap),
	}
	if err := a.startServiceProc(ctx, conn, svc, ws); err != nil {
		return nil, err
	}
	a.st.PutService(ws.Root, svc)
	return svc, nil
}

func (a *API) startServiceProc(_ context.Context, _ *rpc.Conn, svc *state.Service, ws *state.Workspace) error {
	// Services outlive the request context — deliberately detached so a
	// client disconnect or $/cancel does not kill the service.
	cmd := runCmd(context.Background(), ws, svc.Command)
	pw := &fanoutWriter{svc: svc}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return rpc.Errf(rpc.CodeInternal, "start service: %v", err)
	}
	svc.Attach(cmd.Process)
	go func() {
		_ = cmd.Wait()
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		svc.Exited(code)
	}()
	return nil
}

// fanoutWriter appends service output to the ring buffer and pushes it to
// service.logs followers.
type fanoutWriter struct{ svc *state.Service }

func (w *fanoutWriter) Write(p []byte) (int, error) {
	w.svc.PushLog(string(p))
	return len(p), nil
}

// ServiceList lists services for one workspace or all.
func (a *API) ServiceList(_ context.Context, _ *rpc.Conn, params json.RawMessage) (any, error) {
	var p workspaceParam
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	root := ""
	if p.Workspace != "" {
		ws, err := a.workspace(p)
		if err != nil {
			return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
		}
		root = ws.Root
	}
	return map[string]any{"services": a.st.ListServices(root)}, nil
}

// ServiceStop stops a service without removing it.
func (a *API) ServiceStop(_ context.Context, _ *rpc.Conn, params json.RawMessage) (any, error) {
	svc, err := a.serviceParam(params)
	if err != nil {
		return nil, err
	}
	svc.Kill()
	return svc, nil
}

// ServiceRestart restarts a service with its original command.
func (a *API) ServiceRestart(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	svc, err := a.serviceParam(params)
	if err != nil {
		return nil, err
	}
	svc.Kill()
	ws, err := a.st.ResolveWorkspace(svc.Workspace)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInternal, "%v", err)
	}
	if err := a.startServiceProc(ctx, conn, svc, ws); err != nil {
		return nil, err
	}
	return svc, nil
}

// ServiceLogs returns the buffered log tail; with follow=true it streams
// service.logs notifications until cancelled.
func (a *API) ServiceLogs(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Name   string `json:"name"`
		Follow bool   `json:"follow,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	ws, err := a.workspace(p.workspaceParam)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	svc, ok := a.st.GetService(ws.Root, p.Name)
	if !ok {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "unknown service %q", p.Name)
	}
	if !p.Follow {
		return map[string]any{"name": svc.Name, "logs": string(svc.Logs.Bytes())}, nil
	}
	reqID := rpc.ReqID(ctx)
	f := &state.Follow{C: make(chan string, 64)}
	unfollow := svc.AddFollow(f)
	defer unfollow()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case chunk := <-f.C:
			_ = conn.Notify("service.logs", map[string]any{
				"name": svc.Name, "reqId": reqID, "data": chunk,
			})
		}
	}
}

func (a *API) serviceParam(params json.RawMessage) (*state.Service, error) {
	var p struct {
		workspaceParam
		Name string `json:"name"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	ws, err := a.workspace(p.workspaceParam)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	svc, ok := a.st.GetService(ws.Root, p.Name)
	if !ok {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "unknown service %q", p.Name)
	}
	return svc, nil
}
