package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"

	"devbox-agentd/internal/rpc"
	"devbox-agentd/internal/state"
)

// --- pty ---
// Pty sessions are backed by plain pipes (no real TTY allocation); enough
// for agent-driven interactive-ish commands without external deps.

// PtyOpen spawns a long-lived shell (or command) and streams output as
// pty.output notifications {id, reqId, data}.
func (a *API) PtyOpen(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Command string `json:"command,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	ws, err := a.workspace(p.workspaceParam, conn)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	command := p.Command
	if command == "" {
		command = shellBin
	}

	out := state.NewRing(state.ExecBufCap)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInternal, "%v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInternal, "%v", err)
	}

	pty := &state.Pty{
		ID:        a.st.NewID("pty"),
		Workspace: ws.Root,
		Command:   command,
	}
	cmd, err := runCmd(ctx, ws, command, runOpts{})
	if err != nil {
		return nil, err
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW
	if err := cmd.Start(); err != nil {
		return nil, rpc.Errf(rpc.CodeInternal, "pty start: %v", err)
	}
	pty.Attach(stdinW, cmd.Process, out)
	a.st.PutPty(pty)

	reqID := rpc.ReqID(ctx)
	go func() {
		defer func() {
			_ = stdoutR.Close()
			pty.Kill()
		}()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := stdoutR.Read(buf)
			if n > 0 {
				chunk := string(buf[:n])
				_, _ = out.Write([]byte(chunk))
				_ = conn.Notify("pty.output", map[string]any{
					"id": pty.ID, "reqId": reqID, "data": chunk,
				})
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() {
		_ = cmd.Wait()
		pty.Kill()
		_ = stdoutW.Close()
		a.st.Publish(state.Event{Type: "pty.exited", Workspace: ws.Root, ID: pty.ID})
	}()

	return map[string]any{"id": pty.ID, "workspace": ws.Root, "command": command}, nil
}

func (a *API) ptyParam(params json.RawMessage) (*state.Pty, error) {
	var p struct {
		ID string `json:"id"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	pty, ok := a.st.GetPty(p.ID)
	if !ok {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "unknown pty id %q", p.ID)
	}
	return pty, nil
}

// PtyWrite writes data to the pty's stdin.
func (a *API) PtyWrite(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		ID   string `json:"id"`
		Data string `json:"data"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	pty, ok := a.st.GetPty(p.ID)
	if !ok {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "unknown pty id %q", p.ID)
	}
	if err := pty.WriteStdin([]byte(p.Data)); err != nil {
		return nil, rpc.Errf(rpc.CodeInternal, "%v", err)
	}
	return map[string]any{"id": pty.ID, "written": len(p.Data)}, nil
}

// PtyRead drains the buffered pty output.
func (a *API) PtyRead(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	pty, err := a.ptyParam(params)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":      pty.ID,
		"running": pty.Running,
		"output":  string(pty.Output().Bytes()),
	}, nil
}

// PtyKill terminates a pty session.
func (a *API) PtyKill(_ context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	pty, err := a.ptyParam(params)
	if err != nil {
		return nil, err
	}
	pty.Kill()
	a.st.DeletePty(pty.ID)
	return map[string]any{"id": pty.ID, "killed": true}, nil
}

// --- git ---

// gitRun runs a git command in the workspace and returns combined output.
func (a *API) gitRun(ctx context.Context, ws *state.Workspace, args ...string) (string, int, error) {
	full := append([]string{"-C", ws.Root}, args...)
	// args are built from fixed git subcommands; user input (target, message)
	// is always passed as a separate argv element after "--" or as a flag
	// value, never through a shell.
	cmd := exec.CommandContext(ctx, "git", full...)
	pr, err := cmd.StdoutPipe()
	if err != nil {
		return "", 1, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return "", 1, rpc.Errf(rpc.CodeInternal, "git: %v", err)
	}
	out, _ := io.ReadAll(bufio.NewReader(pr))
	werr := cmd.Wait()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if werr != nil && code == 0 {
		code = 1
	}
	return string(out), code, nil
}

// GitStatus returns `git status --short --branch` output.
func (a *API) GitStatus(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	return a.gitCall(ctx, conn, params, "status", "--short", "--branch")
}

// GitDiff returns `git diff` (or --staged) output.
func (a *API) GitDiff(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Staged bool `json:"staged,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	args := []string{"diff"}
	if p.Staged {
		args = append(args, "--staged")
	}
	return a.gitCallArgs(ctx, conn, p.workspaceParam, args...)
}

// GitLog returns recent history (default 15 commits).
func (a *API) GitLog(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Limit int `json:"limit,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 15
	}
	return a.gitCallArgs(ctx, conn, p.workspaceParam,
		"log", "--oneline", "--decorate", "-n", itoa(limit))
}

// GitCommit stages (optionally) and commits with a message. paths stages
// specific files before committing (mutually exclusive with all).
func (a *API) GitCommit(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Message string   `json:"message"`
		All     bool     `json:"all,omitempty"`
		Paths   []string `json:"paths,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Message == "" {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "message is required")
	}
	if p.All && len(p.Paths) > 0 {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "all and paths are mutually exclusive")
	}
	ws, err := a.workspace(p.workspaceParam, conn)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	if len(p.Paths) > 0 {
		addArgs := append([]string{"add", "--"}, p.Paths...)
		if out, code, err := a.gitRun(ctx, ws, addArgs...); err != nil {
			return nil, err
		} else if code != 0 {
			return nil, rpc.Errf(rpc.CodeInternal, "git add failed (%d): %s", code, out)
		}
	}
	args := []string{"commit", "-m", p.Message}
	if p.All {
		args = append(args, "-a")
	}
	out, code, err := a.gitRun(ctx, ws, args...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"exitCode": code, "output": out, "workspace": ws.Root}, nil
}

// GitAdd stages paths (git add -- <paths>).
func (a *API) GitAdd(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Paths []string `json:"paths"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if len(p.Paths) == 0 {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "paths is required")
	}
	args := append([]string{"add", "--"}, p.Paths...)
	return a.gitCallArgs(ctx, conn, p.workspaceParam, args...)
}

// GitFetch runs git fetch [--prune] [remote].
func (a *API) GitFetch(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Remote string `json:"remote,omitempty"`
		Prune  bool   `json:"prune,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	args := []string{"fetch"}
	if p.Prune {
		args = append(args, "--prune")
	}
	if p.Remote != "" {
		args = append(args, p.Remote)
	}
	return a.gitCallArgs(ctx, conn, p.workspaceParam, args...)
}

// GitPull runs git pull [--rebase].
func (a *API) GitPull(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Rebase bool `json:"rebase,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	args := []string{"pull"}
	if p.Rebase {
		args = append(args, "--rebase")
	}
	return a.gitCallArgs(ctx, conn, p.workspaceParam, args...)
}

// GitPush runs git push [--set-upstream|--force] [remote] [branch].
// Publishes work to a remote — callers should confirm first.
func (a *API) GitPush(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Remote      string `json:"remote,omitempty"`
		Branch      string `json:"branch,omitempty"`
		SetUpstream bool   `json:"setUpstream,omitempty"`
		Force       bool   `json:"force,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	args := []string{"push"}
	if p.Force {
		args = append(args, "--force-with-lease")
	}
	if p.SetUpstream {
		args = append(args, "--set-upstream")
	}
	if p.Remote != "" {
		args = append(args, p.Remote)
	}
	if p.Branch != "" {
		args = append(args, p.Branch)
	}
	return a.gitCallArgs(ctx, conn, p.workspaceParam, args...)
}

// GitBranch lists branches (no name) or creates one (name, optionally
// from startPoint); delete=true removes the named branch.
func (a *API) GitBranch(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Name       string `json:"name,omitempty"`
		StartPoint string `json:"startPoint,omitempty"`
		Delete     bool   `json:"delete,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	args := []string{"branch"}
	if p.Delete {
		if p.Name == "" {
			return nil, rpc.Errf(rpc.CodeInvalidParams, "name is required with delete")
		}
		args = append(args, "-d", "--", p.Name)
	} else if p.Name != "" {
		args = append(args, p.Name)
		if p.StartPoint != "" {
			args = append(args, p.StartPoint)
		}
	} else {
		args = append(args, "--list", "-vv")
	}
	return a.gitCallArgs(ctx, conn, p.workspaceParam, args...)
}

// GitStash runs stash operations: list (default), push, pop, apply, drop.
func (a *API) GitStash(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Action  string `json:"action,omitempty"`
		Message string `json:"message,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	args := []string{"stash"}
	switch p.Action {
	case "", "list":
		args = append(args, "list")
	case "push":
		args = append(args, "push")
		if p.Message != "" {
			args = append(args, "-m", p.Message)
		}
	case "pop", "apply", "drop":
		args = append(args, p.Action)
	default:
		return nil, rpc.Errf(rpc.CodeInvalidParams, "action must be list|push|pop|apply|drop")
	}
	return a.gitCallArgs(ctx, conn, p.workspaceParam, args...)
}

// GitShow runs git show <ref> or git show <ref>:<path> (default HEAD).
func (a *API) GitShow(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Ref  string `json:"ref,omitempty"`
		Path string `json:"path,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	ref := p.Ref
	if ref == "" {
		ref = "HEAD"
	}
	spec := ref
	if p.Path != "" {
		spec = ref + ":" + p.Path
	}
	return a.gitCallArgs(ctx, conn, p.workspaceParam, "show", spec)
}

// GitCheckout checks out a branch/ref (destructive — caller confirms).
func (a *API) GitCheckout(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Target string `json:"target"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Target == "" {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "target is required")
	}
	return a.gitCallArgs(ctx, conn, p.workspaceParam, "checkout", "--", p.Target)
}

// GitReset runs `git reset [--soft|--mixed|--hard] [target]`.
func (a *API) GitReset(ctx context.Context, conn *rpc.Conn, params json.RawMessage) (any, error) {
	var p struct {
		workspaceParam
		Mode   string `json:"mode,omitempty"`
		Target string `json:"target,omitempty"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	args := []string{"reset"}
	switch p.Mode {
	case "":
	case "soft", "mixed", "hard":
		args = append(args, "--"+p.Mode)
	default:
		return nil, rpc.Errf(rpc.CodeInvalidParams, "mode must be soft|mixed|hard")
	}
	if p.Target != "" {
		args = append(args, "--", p.Target)
	}
	return a.gitCallArgs(ctx, conn, p.workspaceParam, args...)
}

func (a *API) gitCall(ctx context.Context, conn *rpc.Conn, params json.RawMessage, args ...string) (any, error) {
	var p workspaceParam
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	return a.gitCallArgs(ctx, conn, p, args...)
}

func (a *API) gitCallArgs(ctx context.Context, conn *rpc.Conn, p workspaceParam, args ...string) (any, error) {
	ws, err := a.workspace(p, conn)
	if err != nil {
		return nil, rpc.Errf(rpc.CodeInvalidParams, "%v", err)
	}
	out, code, err := a.gitRun(ctx, ws, args...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"exitCode": code, "output": out, "workspace": ws.Root}, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
