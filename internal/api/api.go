// Package api implements the daemon's JSON-RPC method surface.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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
	reg("workspace.bind", a.WorkspaceBind)
	reg("workspace.unbind", a.WorkspaceUnbind)

	reg("exec.run", a.ExecRun)
	reg("exec.output", a.ExecOutput)
	reg("exec.list", a.ExecList)
	reg("exec.cancel", a.ExecCancel)
	reg("exec.wait", a.ExecWait)

	reg("events.subscribe", a.EventsSubscribe)

	reg("service.list", a.ServiceList)
	reg("service.start", a.ServiceStart)
	reg("service.stop", a.ServiceStop)
	reg("service.restart", a.ServiceRestart)
	reg("service.logs", a.ServiceLogs)
	reg("service.wait", a.ServiceWait)

	reg("fs.read", a.FsRead)
	reg("fs.write", a.FsWrite)
	reg("fs.patch", a.FsPatch)

	reg("meta.info", a.MetaInfo)

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
	reg("git.add", a.GitAdd)
	reg("git.fetch", a.GitFetch)
	reg("git.pull", a.GitPull)
	reg("git.push", a.GitPush)
	reg("git.branch", a.GitBranch)
	reg("git.stash", a.GitStash)
	reg("git.show", a.GitShow)
	return a
}

// workspaceParam is embedded by every workspace-scoped request.
type workspaceParam struct {
	// Workspace is the project root path; defaults to the most recently
	// opened workspace.
	Workspace string `json:"workspace,omitempty"`
}

// workspace resolves the effective workspace for a call: an explicit
// workspace param wins, then the connection's bound workspace (set via
// workspace.bind), then the most recently opened one.
func (a *API) workspace(p workspaceParam, conn *rpc.Conn) (*state.Workspace, error) {
	if p.Workspace != "" {
		return a.st.ResolveWorkspace(p.Workspace)
	}
	if conn != nil {
		if bound := conn.BoundWorkspace(); bound != "" {
			return a.st.ResolveWorkspace(bound)
		}
	}
	return a.st.ResolveWorkspace("")
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

// runOpts customizes a spawned command.
type runOpts struct {
	// Cwd is a directory relative to the workspace root.
	Cwd string
	// Env is extra KEY=VALUE pairs appended to the environment. Values may
	// carry secrets — they are never stored or echoed back.
	Env map[string]string
	// Stdin, when non-nil, is wired to the command's standard input.
	Stdin *string
}

// resolveDir returns the absolute working dir for opts.Cwd inside ws.Root,
// rejecting paths that escape the workspace.
func resolveDir(ws *state.Workspace, cwd string) (string, error) {
	if cwd == "" {
		return ws.Root, nil
	}
	abs := cwd
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(ws.Root, abs)
	}
	abs = filepath.Clean(abs)
	if abs != ws.Root && !strings.HasPrefix(abs, ws.Root+string(os.PathSeparator)) {
		return "", fmt.Errorf("cwd %q escapes workspace root", cwd)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cwd %q: %w", cwd, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("cwd %q is not a directory", cwd)
	}
	return abs, nil
}

// runCmd builds the *exec.Cmd for a shell command inside ws. When the
// workspace has a devbox.json and devbox is on PATH, the command runs via
// `devbox run --` so it sees the project's toolchain.
func runCmd(ctx context.Context, ws *state.Workspace, command string, opts runOpts) (*exec.Cmd, error) {
	dir, err := resolveDir(ws, opts.Cwd)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	var c *exec.Cmd
	if ws.Devbox {
		if _, err := exec.LookPath("devbox"); err == nil {
			// Wrap inside the shell so the process entry point stays the
			// constant shell binary: `devbox run -- sh -c <command>`.
			wrapped := "exec devbox run -- " + shellBin + " " + shellFlag + " " + shellQuote(command)
			// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- running the caller's shell command is the documented purpose of exec.run; argv[0] stays the constant shell binary.
			c = exec.CommandContext(ctx, shellBin, shellFlag, wrapped)
		}
	}
	if c == nil {
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- running the caller's shell command is the documented purpose of exec.run.
		c = exec.CommandContext(ctx, shellBin, shellFlag, command)
	}
	c.Dir = dir
	if len(opts.Env) > 0 {
		env := os.Environ()
		for k, v := range opts.Env {
			env = append(env, k+"="+v)
		}
		c.Env = env
	}
	if opts.Stdin != nil {
		c.Stdin = strings.NewReader(*opts.Stdin)
	}
	return c, nil
}

// setupProcGroup puts the command in its own process group so
// killOnCancel can reap wrapper children (devbox, sh) and grandchildren
// together — CommandContext alone only kills the direct child.
func setupProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killOnCancel kills the whole process group when ctx is done. Call after
// cmd.Start().
func killOnCancel(ctx context.Context, cmd *exec.Cmd) {
	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
		}
	}()
}

// shellQuote single-quotes s for POSIX shell embedding.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- workspaces ---

// WorkspaceOpen binds a project root.
func (a *API) WorkspaceOpen(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
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

// WorkspaceBind pins the connection's default workspace so subsequent
// calls with no workspace param resolve to it — gives each session its
// own workspace even when several agents share the daemon.
func (a *API) WorkspaceBind(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
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
	conn.SetBoundWorkspace(ws.Root)
	return ws, nil
}

// WorkspaceUnbind clears the connection's bound workspace.
func (a *API) WorkspaceUnbind(_ context.Context, conn *rpc.Conn, _ json.RawMessage) (any, error) {
	prev := conn.BoundWorkspace()
	conn.SetBoundWorkspace("")
	return map[string]any{"unbound": prev}, nil
}

// WorkspaceDestroy closes a workspace and kills its services/pty sessions.
func (a *API) WorkspaceDestroy(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
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
	a.st.Publish(state.Event{Type: "workspace.destroyed", Workspace: p.Root})
	return map[string]any{"destroyed": true, "root": p.Root}, nil
}

// --- exec ---

// execResult is the result shape shared by exec.run, exec.wait and the
// exec.done event payload.
func execResult(e *state.Exec) map[string]any {
	code := -1
	if e.ExitCode != nil {
		code = *e.ExitCode
	}
	return map[string]any{
		"id":         e.ID,
		"workspace":  e.Workspace,
		"command":    e.Command,
		"cwd":        e.Cwd,
		"exitCode":   code,
		"done":       e.Done,
		"timedOut":   e.TimedOut,
		"signal":     e.Signal,
		"durationMs": e.DurationMs,
		"startedAt":  e.StartedAt,
		"finishedAt": e.FinishedAt,
		"output":     string(e.Output.Bytes()),
	}
}

// execSignal extracts the signal name that terminated the process, if any.
func execSignal(cmd *exec.Cmd) string {
	if cmd.ProcessState == nil {
		return ""
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}

// ExecRun runs a shell command in the workspace, streaming output as
// exec.output notifications {id, reqId, data} on the calling connection.
// Cancel via `$/cancel` or exec.cancel. With background=true the request
// returns immediately; completion is published as an exec.done event.
func (a *API) ExecRun(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Command    string            `json:"command"`
		Cwd        string            `json:"cwd,omitempty"`
		Env        map[string]string `json:"env,omitempty"`
		TimeoutSec int               `json:"timeoutSec,omitempty"`
		Stdin      *string           `json:"stdin,omitempty"`
		Background bool              `json:"background,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Command == "" {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "command is required")
	}
	ws, err := a.workspace(p.workspaceParam, conn)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}

	e := state.NewExec(a.st.NewID("exec"), ws.Root, p.Command)
	e.Cwd = p.Cwd
	e.Background = p.Background

	// Detached context for background execs; timeout applies to both modes.
	runCtx := ctx
	if p.Background {
		runCtx = context.Background()
	}
	runCtx, cancel := context.WithCancel(runCtx)
	if p.TimeoutSec > 0 {
		runCtx, cancel = context.WithTimeout(runCtx, time.Duration(p.TimeoutSec)*time.Second)
	}
	e.AttachCancel(cancel)
	a.st.PutExec(e)

	// Stream combined stdout+stderr.
	pr, pw, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, rpc.Errf(rpc.CodeInternal, "%v", err)
	}
	cmd, err := runCmd(runCtx, ws, p.Command, runOpts{Cwd: p.Cwd, Env: p.Env, Stdin: p.Stdin})
	if err != nil {
		cancel()
		_ = pr.Close()
		_ = pw.Close()
		return nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	setupProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		cancel()
		_ = pr.Close()
		_ = pw.Close()
		return nil, rpc.Errf(rpc.CodeInternal, "start: %v", err)
	}
	killOnCancel(runCtx, cmd)

	reqID := rpc.ReqID(ctx)
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

	finish := func() {
		werr := cmd.Wait()
		_ = pw.Close()
		<-copyDone
		_ = pr.Close()

		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		timedOut := runCtx.Err() == context.DeadlineExceeded
		if runCtx.Err() != nil {
			code = -1
		} else if werr != nil && code == 0 {
			code = 1
		}
		e.Finish(code, timedOut, execSignal(cmd))
		a.st.Publish(state.Event{
			Type:      "exec.done",
			Workspace: ws.Root,
			ID:        e.ID,
			Data: map[string]any{
				"exitCode":   code,
				"timedOut":   timedOut,
				"durationMs": e.DurationMs,
			},
		})
	}

	if p.Background {
		go finish()
		return map[string]any{
			"id":         e.ID,
			"background": true,
			"pid":        cmd.Process.Pid,
			"workspace":  ws.Root,
		}, nil
	}

	finish()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return execResult(e), nil
}

// ExecList returns execs for one workspace or all.
func (a *API) ExecList(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p workspaceParam
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	root := ""
	if p.Workspace != "" {
		ws, err := a.workspace(p, conn)
		if err != nil {
			return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
		}
		root = ws.Root
	}
	execs := a.st.ListExecs(root)
	out := make([]map[string]any, 0, len(execs))
	for _, e := range execs {
		code := -1
		if e.ExitCode != nil {
			code = *e.ExitCode
		}
		out = append(out, map[string]any{
			"id":         e.ID,
			"workspace":  e.Workspace,
			"command":    e.Command,
			"cwd":        e.Cwd,
			"background": e.Background,
			"done":       e.Done,
			"exitCode":   code,
			"timedOut":   e.TimedOut,
			"signal":     e.Signal,
			"durationMs": e.DurationMs,
			"startedAt":  e.StartedAt,
			"finishedAt": e.FinishedAt,
		})
	}
	return map[string]any{"execs": out}, nil
}

// ExecCancel kills a running exec by id.
func (a *API) ExecCancel(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
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
	if e.Done {
		return map[string]any{"id": e.ID, "cancelled": false, "done": true}, nil
	}
	e.Cancel()
	return map[string]any{"id": e.ID, "cancelled": true}, nil
}

// ExecWait blocks until an exec finishes or timeoutSec elapses.
func (a *API) ExecWait(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		ID         string `json:"id"`
		TimeoutSec int    `json:"timeoutSec,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	e, ok := a.st.GetExec(p.ID)
	if !ok {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "unknown exec id %q", p.ID)
	}
	timeout := time.Duration(p.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	select {
	case <-e.DoneChan():
		return execResult(e), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, rpc.Errf(rpc.CodeInternal, "exec %s still running after %ds", e.ID, p.TimeoutSec)
	}
}

// EventsSubscribe streams daemon lifecycle events (exec.done,
// service.exited, pty.exited, ...) as `event` notifications until the
// request is cancelled.
func (a *API) EventsSubscribe(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Types []string `json:"types,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	filter := map[string]bool{}
	for _, t := range p.Types {
		filter[t] = true
	}
	root := ""
	if p.Workspace != "" {
		ws, err := a.workspace(p.workspaceParam, conn)
		if err != nil {
			return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
		}
		root = ws.Root
	}
	sub, unsub := a.st.Subscribe()
	defer unsub()
	reqID := rpc.ReqID(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev := <-sub.C:
			if len(filter) > 0 && !filter[ev.Type] {
				continue
			}
			if root != "" && ev.Workspace != "" && ev.Workspace != root {
				continue
			}
			m := map[string]any{
				"reqId":     reqID,
				"type":      ev.Type,
				"workspace": ev.Workspace,
				"id":        ev.ID,
				"name":      ev.Name,
				"data":      ev.Data,
				"at":        ev.At,
			}
			_ = conn.Notify("event", m)
		}
	}
}

// ExecOutput returns the buffered output of a past or running exec.
func (a *API) ExecOutput(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
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
		Name    string            `json:"name"`
		Command string            `json:"command"`
		Cwd     string            `json:"cwd,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
		PortEnv string            `json:"portEnv,omitempty"`
		Port    int               `json:"port,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Name == "" || p.Command == "" {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "name and command are required")
	}
	ws, err := a.workspace(p.workspaceParam, conn)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	// Validate cwd before mutating any state.
	if _, err := resolveDir(ws, p.Cwd); err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	// Resolve the assigned port: explicit port must be free; portEnv
	// without port picks one automatically.
	port := p.Port
	if p.PortEnv != "" && port == 0 {
		port, err = freePort()
		if err != nil {
			return nil, rpc.Errf(rpc.CodeInternal, "assign port: %v", err)
		}
	} else if p.PortEnv != "" && portOpen(port) {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "port %d is already in use", port)
	}
	if existing, ok := a.st.GetService(ws.Root, p.Name); ok {
		existing.Kill()
	}
	svc := &state.Service{
		Name:      p.Name,
		Workspace: ws.Root,
		Command:   p.Command,
		Cwd:       p.Cwd,
		PortEnv:   p.PortEnv,
		Port:      port,
		Env:       p.Env,
		Logs:      state.NewRing(state.ExecBufCap),
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
	env := svc.Env
	if svc.PortEnv != "" && svc.Port > 0 {
		env = map[string]string{}
		for k, v := range svc.Env {
			env[k] = v
		}
		env[svc.PortEnv] = strconv.Itoa(svc.Port)
	}
	cmd, err := runCmd(context.Background(), ws, svc.Command,
		runOpts{Cwd: svc.Cwd, Env: env})
	if err != nil {
		return err
	}
	pw := &fanoutWriter{svc: svc}
	cmd.Stdout = pw
	cmd.Stderr = pw
	setupProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		return rpc.Errf(rpc.CodeInternal, "start service: %v", err)
	}
	svc.Attach(cmd.Process)
	svc.RestartCount++
	svc.Ports = nil
	if svc.Port > 0 {
		svc.Ports = []int{svc.Port}
	}
	a.st.Publish(state.Event{
		Type: "service.started", Workspace: ws.Root, Name: svc.Name,
		Data: map[string]any{"pid": cmd.Process.Pid},
	})
	go func() {
		_ = cmd.Wait()
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		svc.Exited(code)
		a.st.Publish(state.Event{
			Type: "service.exited", Workspace: ws.Root, Name: svc.Name,
			Data: map[string]any{"exitCode": code},
		})
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
func (a *API) ServiceList(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p workspaceParam
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	root := ""
	if p.Workspace != "" {
		ws, err := a.workspace(p, conn)
		if err != nil {
			return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
		}
		root = ws.Root
	}
	svcs := a.st.ListServices(root)
	out := make([]map[string]any, 0, len(svcs))
	for _, svc := range svcs {
		if svc.Running && svc.PID > 0 {
			svc.Ports = listeningPorts(svc.PID)
		}
		out = append(out, svc.Snapshot())
	}
	return map[string]any{"services": out}, nil
}

// ServiceWait blocks until a service is running (and optionally accepting
// TCP connections on port) or timeoutSec elapses.
func (a *API) ServiceWait(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Name       string `json:"name"`
		Port       int    `json:"port,omitempty"`
		TimeoutSec int    `json:"timeoutSec,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	ws, err := a.workspace(p.workspaceParam, conn)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	svc, ok := a.st.GetService(ws.Root, p.Name)
	if !ok {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "unknown service %q", p.Name)
	}
	timeout := time.Duration(p.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if svc.Running {
			if p.Port == 0 || portOpen(p.Port) {
				svc.Ports = listeningPorts(svc.PID)
				snap := svc.Snapshot()
				snap["ready"] = true
				return snap, nil
			}
		} else if svc.ExitCode != nil {
			return nil, rpc.Errf(rpc.CodeInternal,
				"service %q exited with code %d", p.Name, *svc.ExitCode)
		}
		if time.Now().After(deadline) {
			return nil, rpc.Errf(rpc.CodeInternal,
				"service %q not ready after %ds", p.Name, p.TimeoutSec)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// freePort returns an available TCP port on loopback.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// portOpen reports whether localhost:port accepts TCP connections.
func portOpen(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// listeningPorts best-effort reports TCP ports the pid listens on
// (Linux /proc only; empty elsewhere or on error).
func listeningPorts(pid int) []int {
	if runtime.GOOS != "linux" || pid <= 0 {
		return nil
	}
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return nil
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
		if err == nil && strings.HasPrefix(link, "socket:[") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
		}
	}
	if len(inodes) == 0 {
		return nil
	}
	ports := map[int]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if i == 0 {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" { // 0A = LISTEN
				continue
			}
			if !inodes[fields[9]] {
				continue
			}
			parts := strings.Split(fields[1], ":")
			if port, err := strconv.ParseInt(parts[len(parts)-1], 16, 32); err == nil {
				ports[int(port)] = true
			}
		}
	}
	out := make([]int, 0, len(ports))
	for p := range ports {
		out = append(out, p)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// ServiceStop stops a service without removing it.
func (a *API) ServiceStop(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	svc, err := a.serviceParam(params, conn)
	if err != nil {
		return nil, err
	}
	svc.Kill()
	return svc, nil
}

// ServiceRestart restarts a service with its original command.
func (a *API) ServiceRestart(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	svc, err := a.serviceParam(params, conn)
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
	ws, err := a.workspace(p.workspaceParam, conn)
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

// --- filesystem ---

// resolvePath returns the absolute path for p inside ws.Root, rejecting
// paths that escape the workspace.
func resolvePath(ws *state.Workspace, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(ws.Root, abs)
	}
	abs = filepath.Clean(abs)
	if abs != ws.Root && !strings.HasPrefix(abs, ws.Root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes workspace root", p)
	}
	return abs, nil
}

// FsRead reads a file inside the workspace. offset/limit are byte-based
// (limit=0 → whole file); returns content plus size/truncated metadata.
func (a *API) FsRead(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Path   string `json:"path"`
		Offset int64  `json:"offset,omitempty"`
		Limit  int64  `json:"limit,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	ws, err := a.workspace(p.workspaceParam, conn)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	abs, err := resolvePath(ws, p.Path)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInternal, "read %s: %v", p.Path, err)
	}
	size := int64(len(data))
	if p.Offset > size {
		p.Offset = size
	}
	data = data[p.Offset:]
	truncated := false
	if p.Limit > 0 && int64(len(data)) > p.Limit {
		data = data[:p.Limit]
		truncated = true
	}
	return map[string]any{
		"path":      abs,
		"size":      size,
		"offset":    p.Offset,
		"truncated": truncated,
		"content":   string(data),
	}, nil
}

// FsWrite writes (or appends to) a file inside the workspace, creating
// parent directories as needed.
func (a *API) FsWrite(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	ws, err := a.workspace(p.workspaceParam, conn)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	abs, err := resolvePath(ws, p.Path)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, rpc.Errf(rpc.CodeInternal, "%v", err)
	}
	if p.Append {
		f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, rpc.Errf(rpc.CodeInternal, "%v", err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.WriteString(p.Content); err != nil {
			return nil, rpc.Errf(rpc.CodeInternal, "%v", err)
		}
	} else if err := os.WriteFile(abs, []byte(p.Content), 0o644); err != nil {
		return nil, rpc.Errf(rpc.CodeInternal, "%v", err)
	}
	fi, _ := os.Stat(abs)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	return map[string]any{"path": abs, "size": size, "appended": p.Append}, nil
}

// FsPatch applies exact-match text replacements to a file. Each edit's
// old string must appear exactly once; all-or-nothing.
func (a *API) FsPatch(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Path  string `json:"path"`
		Edits []struct {
			Old string `json:"old"`
			New string `json:"new"`
		} `json:"edits"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if len(p.Edits) == 0 {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "edits is required")
	}
	ws, err := a.workspace(p.workspaceParam, conn)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	abs, err := resolvePath(ws, p.Path)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInternal, "read %s: %v", p.Path, err)
	}
	content := string(data)
	applied := 0
	for i, e := range p.Edits {
		if e.Old == "" {
			return nil, rpc.Errf(rpc.CodeInvalidParams, "edits[%d].old must be non-empty", i)
		}
		if n := strings.Count(content, e.Old); n != 1 {
			return nil, rpc.Errf(rpc.CodeInvalidParams,
				"edits[%d].old matches %d times (need exactly 1)", i, n)
		}
		content = strings.Replace(content, e.Old, e.New, 1)
		applied++
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return nil, rpc.Errf(rpc.CodeInternal, "%v", err)
	}
	return map[string]any{"path": abs, "editsApplied": applied, "size": len(content)}, nil
}

// --- meta ---

// destructiveMethods is the canonical list of methods callers should
// confirm before invoking; surfaced via meta.info.
var destructiveMethods = map[string]bool{
	"workspace.destroy": true,
	"service.stop":      true,
	"service.restart":   true,
	"pty.kill":          true,
	"git.checkout":      true,
	"git.reset":         true,
	"git.push":          true,
	"exec.cancel":       true,
}

// MetaInfo returns daemon capabilities: registered methods with their
// destructive flags, buffer caps, and version.
func (a *API) MetaInfo(_ context.Context, _ *rpc.Conn, _ json.RawMessage) (any, error) {
	methods := make([]map[string]any, 0, len(a.Dispatch))
	for m := range a.Dispatch {
		methods = append(methods, map[string]any{
			"method":      m,
			"destructive": destructiveMethods[m],
		})
	}
	return map[string]any{
		"version":    "0.2.0",
		"methods":    methods,
		"bufferCap":  state.ExecBufCap,
		"eventTypes": []string{"exec.done", "service.started", "service.exited", "pty.exited", "workspace.destroyed"},
	}, nil
}

func (a *API) serviceParam(params json.RawMessage, conn *rpc.Conn) (*state.Service, error) {
	var p struct {
		workspaceParam
		Name string `json:"name"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	ws, err := a.workspace(p.workspaceParam, conn)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	svc, ok := a.st.GetService(ws.Root, p.Name)
	if !ok {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "unknown service %q", p.Name)
	}
	return svc, nil
}
