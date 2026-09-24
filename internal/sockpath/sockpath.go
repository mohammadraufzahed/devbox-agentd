// Package sockpath resolves the daemon's runtime directory and socket paths.
package sockpath

import (
	"fmt"
	"os"
	"path/filepath"
)

const dirName = "devbox-agentd"

// Dir returns the daemon runtime directory:
// $XDG_RUNTIME_DIR/devbox-agentd, falling back to
// ~/.local/state/devbox-agentd when XDG_RUNTIME_DIR is unset.
func Dir() (string, error) {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, dirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", dirName), nil
}

// EnsureDir creates the runtime directory (0700) if needed and returns it.
func EnsureDir() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("create runtime dir: %w", err)
	}
	return d, nil
}

// AgentSock returns the JSON-RPC API socket path.
func AgentSock() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "agent.sock"), nil
}

// MCPSock returns the MCP adapter socket path.
func MCPSock() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "mcp.sock"), nil
}

// LockFile returns the path used to serialize concurrent `ensure` calls.
func LockFile() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "ensure.lock"), nil
}

// PIDFile returns the path of the daemon PID file.
func PIDFile() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "agent.pid"), nil
}
