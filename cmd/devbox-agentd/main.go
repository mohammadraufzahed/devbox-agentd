// devbox-agentd is a daemon exposing Devbox project environments over a
// Unix-socket JSON-RPC 2.0 API (agent.sock) plus an MCP adapter (mcp.sock).
//
// Usage:
//
//	devbox-agentd serve [--socket PATH] [--mcp-socket PATH] [--idle-exit DUR]
//	devbox-agentd ensure   — connect, or start the daemon detached (idempotent)
//	devbox-agentd version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"devbox-agentd/internal/api"
	"devbox-agentd/internal/mcp"
	"devbox-agentd/internal/rpc"
	"devbox-agentd/internal/sockpath"
	"devbox-agentd/internal/state"
)

const version = "0.1.0"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(log, os.Args[2:])
	case "ensure":
		err = cmdEnsure(log)
	case "version", "--version", "-v":
		fmt.Println("devbox-agentd", version)
	case "help", "--help", "-h":
		fmt.Println("usage: devbox-agentd [serve|ensure|version]")
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "devbox-agentd:", err)
		os.Exit(1)
	}
}

// --- serve ---

func cmdServe(log *slog.Logger, argv []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	sock := fs.String("socket", "", "agent API socket path")
	mcpSock := fs.String("mcp-socket", "", "MCP socket path")
	idleExit := fs.Duration("idle-exit", 0, "exit after this idle duration (default: never)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	var err error
	if *sock == "" {
		if *sock, err = sockpath.AgentSock(); err != nil {
			return err
		}
	}
	if *mcpSock == "" {
		if *mcpSock, err = sockpath.MCPSock(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(*sock), 0o700); err != nil {
		return err
	}

	// idle-exit bookkeeping
	var lastActivity = time.Now()
	var activeConns int32
	_ = activeConns
	onActivity := func() { lastActivity = time.Now() }

	st := state.NewStore()
	srv := rpc.NewServer(log, onActivity)
	a := api.New(srv, st, *sock)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	agentLn, err := listenUnix(*sock)
	if err != nil {
		return err
	}
	mcpLn, err := listenUnix(*mcpSock)
	if err != nil {
		return err
	}
	writePIDFile()

	errCh := make(chan error, 2)
	go func() { errCh <- srv.Serve(ctx, agentLn) }()
	adapter := mcp.NewAdapter(log, st, a.Dispatch)
	go func() { errCh <- adapter.Serve(ctx, mcpLn) }()

	log.Info("devbox-agentd listening", "socket", *sock, "mcp", *mcpSock, "pid", os.Getpid())

	if *idleExit > 0 {
		go func() {
			t := time.NewTicker(*idleExit / 2)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if time.Since(lastActivity) >= *idleExit {
						log.Info("idle timeout, exiting")
						stop()
						return
					}
				}
			}
		}()
	}

	select {
	case <-ctx.Done():
	case e := <-errCh:
		if e != nil {
			return e
		}
	}
	_ = agentLn.Close()
	_ = mcpLn.Close()
	_ = os.Remove(*sock)
	_ = os.Remove(*mcpSock)
	return nil
}

func listenUnix(path string) (net.Listener, error) {
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

func writePIDFile() {
	if p, err := sockpath.PIDFile(); err == nil {
		_ = os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
}

// --- ensure ---

// cmdEnsure connects to the daemon, or starts it detached and waits for
// the socket. Safe to run concurrently — serialized via a lock file.
// Prints the socket path on success.
func cmdEnsure(log *slog.Logger) error {
	sock, err := sockpath.AgentSock()
	if err != nil {
		return err
	}
	if dialOK(sock) {
		fmt.Println(sock)
		return nil
	}
	if _, err := sockpath.EnsureDir(); err != nil {
		return err
	}
	lockPath, err := sockpath.LockFile()
	if err != nil {
		return err
	}
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lf.Close() }()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire ensure lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()

	// Re-check under the lock — a concurrent ensure may have won.
	if dialOK(sock) {
		fmt.Println(sock)
		return nil
	}

	devNull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	defer func() { _ = devNull.Close() }()
	// /proc/self/exe re-executes our own binary — a static path, no user
	// input reaches argv[0].
	cmd := exec.Command("/proc/self/exe", "serve")
	// Detach into a new session so the daemon survives the caller.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.Stdin = devNull
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// Do not Wait — detached child is reaped by init after double-fork
	// semantics from Setsid.
	_ = cmd.Process.Release()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if dialOK(sock) {
			fmt.Println(sock)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("daemon did not become ready within 15s; check `devbox-agentd serve` logs")
}

func dialOK(sock string) bool {
	c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
