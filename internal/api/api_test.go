package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"devbox-agentd/internal/api"
	"devbox-agentd/internal/rpc"
	"devbox-agentd/internal/state"
	"log/slog"
	"os"
)

type client struct {
	t    *testing.T
	conn net.Conn
	sc   *bufio.Scanner
	id   int
	notifications []map[string]any
}

func dial(t *testing.T, sock string) *client {
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cl := &client{t: t, conn: c, sc: bufio.NewScanner(c)}
	cl.sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	return cl
}

// call sends a request and waits for the matching response, collecting
// notifications along the way.
func (c *client) call(method string, params any) map[string]any {
	c.id++
	id := c.id
	p, _ := json.Marshal(params)
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": json.RawMessage(p)}
	if err := json.NewEncoder(c.conn).Encode(req); err != nil {
		c.t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_ = c.conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		if !c.sc.Scan() {
			c.t.Fatalf("connection closed waiting for %s", method)
		}
		var m map[string]any
		if err := json.Unmarshal(c.sc.Bytes(), &m); err != nil {
			c.t.Fatalf("bad json: %v", err)
		}
		if m["method"] != nil {
			c.notifications = append(c.notifications, m)
			continue
		}
		if int(m["id"].(float64)) != id {
			continue
		}
		if e, ok := m["error"]; ok && e != nil {
			c.t.Fatalf("%s error: %v", method, e)
		}
		return m["result"].(map[string]any)
	}
	c.t.Fatalf("timeout waiting for %s", method)
	return nil
}

func notify(c *client, method string, params any) {
	p, _ := json.Marshal(params)
	_ = json.NewEncoder(c.conn).Encode(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": json.RawMessage(p),
	})
}

func setup(t *testing.T) (sock string, root string) {
	dir := t.TempDir()
	sock = filepath.Join(dir, "agent.sock")
	root = t.TempDir()
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := rpc.NewServer(log, nil)
	st := state.NewStore()
	api.New(srv, st, sock)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx, l) }()
	t.Cleanup(func() { cancel(); _ = l.Close() })
	return sock, root
}

func TestWorkspaceAndExec(t *testing.T) {
	sock, root := setup(t)
	c := dial(t, sock)
	defer func() { _ = c.conn.Close() }()

	ws := c.call("workspace.open", map[string]any{"root": root})
	if ws["root"] != root {
		t.Fatalf("workspace root = %v", ws["root"])
	}

	res := c.call("exec.run", map[string]any{"command": "echo hi"})
	if res["exitCode"].(float64) != 0 {
		t.Fatalf("exitCode = %v", res["exitCode"])
	}
	if got := res["output"].(string); got != "hi\n" {
		t.Fatalf("output = %q", got)
	}
	// exec.output notifications should have streamed too.
	if len(c.notifications) == 0 {
		t.Fatal("expected exec.output notifications")
	}

	// exec.output returns the buffered output.
	out := c.call("exec.output", map[string]any{"id": res["id"]})
	if out["output"].(string) != "hi\n" || out["done"] != true {
		t.Fatalf("exec.output = %v", out)
	}
}

func TestCancel(t *testing.T) {
	sock, root := setup(t)
	c := dial(t, sock)
	defer func() { _ = c.conn.Close() }()
	c.call("workspace.open", map[string]any{"root": root})

	// Start a long exec in a goroutine, then cancel by request id.
	done := make(chan map[string]any, 1)
	id := 42
	go func() {
		p, _ := json.Marshal(map[string]any{"command": "sleep 30"})
		_ = json.NewEncoder(c.conn).Encode(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": "exec.run", "params": json.RawMessage(p),
		})
		for c.sc.Scan() {
			var m map[string]any
			if json.Unmarshal(c.sc.Bytes(), &m) != nil {
				continue
			}
			if m["method"] == nil {
				done <- m
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	notify(c, "$/cancel", map[string]any{"id": id})

	select {
	case resp := <-done:
		e, _ := resp["error"].(map[string]any)
		if e == nil {
			t.Fatalf("expected cancel error, got %v", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec.run was not cancelled within 5s")
	}
}

func TestServices(t *testing.T) {
	sock, root := setup(t)
	c := dial(t, sock)
	defer func() { _ = c.conn.Close() }()
	c.call("workspace.open", map[string]any{"root": root})

	svc := c.call("service.start", map[string]any{"name": "sleeper", "command": "sleep 60"})
	if svc["running"] != true {
		t.Fatalf("service not running: %v", svc)
	}
	list := c.call("service.list", map[string]any{"workspace": root})
	if len(list["services"].([]any)) != 1 {
		t.Fatalf("service.list = %v", list)
	}
	stopped := c.call("service.stop", map[string]any{"name": "sleeper"})
	_ = stopped
	list = c.call("service.list", map[string]any{"workspace": root})
	svc = list["services"].([]any)[0].(map[string]any)
	if svc["running"] == true {
		t.Fatal("service still running after stop")
	}
}
