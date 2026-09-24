package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"devbox-agentd/internal/api"
	"devbox-agentd/internal/rpc"
	"devbox-agentd/internal/state"
	"log/slog"
	"os"
)

type client struct {
	t             *testing.T
	conn          net.Conn
	sc            *bufio.Scanner
	id            int
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

// callErr sends a request expecting an error response; returns the error
// object. Fails the test if the call succeeds.
func (c *client) callErr(method string, params any) map[string]any {
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
			return e.(map[string]any)
		}
		c.t.Fatalf("%s unexpectedly succeeded: %v", method, m["result"])
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

func TestExecOptions(t *testing.T) {
	sock, root := setup(t)
	c := dial(t, sock)
	defer func() { _ = c.conn.Close() }()
	c.call("workspace.open", map[string]any{"root": root})

	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	res := c.call("exec.run", map[string]any{
		"command": "cat; pwd",
		"cwd":     "sub",
		"env":     map[string]any{"FOO": "bar"},
		"stdin":   "hello\n",
	})
	if !strings.Contains(res["output"].(string), "hello") || !strings.Contains(res["output"].(string), sub) {
		t.Fatalf("stdin/cwd not applied: %v", res["output"])
	}
	res = c.call("exec.run", map[string]any{
		"command": "echo $FOO",
		"env":     map[string]any{"FOO": "bar"},
	})
	if res["output"].(string) != "bar\n" {
		t.Fatalf("env not applied: %v", res["output"])
	}

	res = c.call("exec.run", map[string]any{"command": "sleep 30", "timeoutSec": 1})
	if res["timedOut"] != true {
		t.Fatalf("expected timedOut: %v", res)
	}
	if res["durationMs"].(float64) <= 0 {
		t.Fatalf("missing durationMs: %v", res)
	}

	errMap := c.callErr("exec.run", map[string]any{"command": "true", "cwd": "../"})
	if !strings.Contains(errMap["message"].(string), "escapes") {
		t.Fatalf("unexpected error: %v", errMap)
	}
}

func TestBackgroundExecAndWait(t *testing.T) {
	sock, root := setup(t)
	c := dial(t, sock)
	defer func() { _ = c.conn.Close() }()
	c.call("workspace.open", map[string]any{"root": root})

	res := c.call("exec.run", map[string]any{
		"command": "sleep 0.3; echo done", "background": true,
	})
	id := res["id"].(string)
	if res["background"] != true {
		t.Fatalf("expected background: %v", res)
	}
	list := c.call("exec.list", map[string]any{"workspace": root})
	if len(list["execs"].([]any)) == 0 {
		t.Fatal("exec.list empty")
	}
	w := c.call("exec.wait", map[string]any{"id": id, "timeoutSec": 10})
	if w["exitCode"].(float64) != 0 || !strings.Contains(w["output"].(string), "done") {
		t.Fatalf("exec.wait = %v", w)
	}
}

func TestExecCancel(t *testing.T) {
	sock, root := setup(t)
	c := dial(t, sock)
	defer func() { _ = c.conn.Close() }()
	c.call("workspace.open", map[string]any{"root": root})

	res := c.call("exec.run", map[string]any{
		"command": "sleep 30", "background": true,
	})
	id := res["id"].(string)
	time.Sleep(200 * time.Millisecond)
	cr := c.call("exec.cancel", map[string]any{"id": id})
	if cr["cancelled"] != true {
		t.Fatalf("exec.cancel = %v", cr)
	}
	w := c.call("exec.wait", map[string]any{"id": id, "timeoutSec": 10})
	if w["done"] != true {
		t.Fatalf("exec did not finish after cancel: %v", w)
	}
}

func TestFsOps(t *testing.T) {
	sock, root := setup(t)
	c := dial(t, sock)
	defer func() { _ = c.conn.Close() }()
	c.call("workspace.open", map[string]any{"root": root})

	c.call("fs.write", map[string]any{"path": "a/b.txt", "content": "hello world"})
	r := c.call("fs.read", map[string]any{"path": "a/b.txt"})
	if r["content"].(string) != "hello world" {
		t.Fatalf("fs.read = %v", r)
	}
	c.call("fs.patch", map[string]any{
		"path":  "a/b.txt",
		"edits": []map[string]any{{"old": "world", "new": "there"}},
	})
	r = c.call("fs.read", map[string]any{"path": "a/b.txt", "offset": 6})
	if r["content"].(string) != "there" {
		t.Fatalf("patched content = %v", r["content"])
	}
	errMap := c.callErr("fs.write", map[string]any{"path": "../evil", "content": "x"})
	if !strings.Contains(errMap["message"].(string), "escapes") {
		t.Fatalf("unexpected error: %v", errMap)
	}
}

func TestEventsAndMeta(t *testing.T) {
	sock, root := setup(t)
	sub := dial(t, sock)
	defer func() { _ = sub.conn.Close() }()
	c := dial(t, sock)
	defer func() { _ = c.conn.Close() }()
	c.call("workspace.open", map[string]any{"root": root})

	subID := 77
	go func() {
		p, _ := json.Marshal(map[string]any{"types": []string{"exec.done"}})
		_ = json.NewEncoder(sub.conn).Encode(map[string]any{
			"jsonrpc": "2.0", "id": subID, "method": "events.subscribe", "params": json.RawMessage(p),
		})
	}()
	time.Sleep(200 * time.Millisecond)

	c.call("exec.run", map[string]any{"command": "true", "background": true})

	deadline := time.Now().Add(10 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		_ = sub.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if !sub.sc.Scan() {
			t.Fatal("subscription connection closed")
		}
		var m map[string]any
		if json.Unmarshal(sub.sc.Bytes(), &m) != nil {
			continue
		}
		if m["method"] == "event" {
			p := m["params"].(map[string]any)
			if p["type"] == "exec.done" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no exec.done event received")
	}

	meta := c.call("meta.info", nil)
	if meta["version"] != "0.2.0" {
		t.Fatalf("meta.info = %v", meta)
	}
	methods := meta["methods"].([]any)
	if len(methods) < 10 {
		t.Fatalf("few methods: %v", methods)
	}
}

func TestWorkspaceBind(t *testing.T) {
	sock, root := setup(t)
	other := t.TempDir()
	c := dial(t, sock)
	defer func() { _ = c.conn.Close() }()
	c.call("workspace.open", map[string]any{"root": root})
	c.call("workspace.open", map[string]any{"root": other})

	// Bound conn resolves its bound root even though another workspace
	// was opened more recently.
	bound := c.call("workspace.bind", map[string]any{"root": root})
	if bound["root"] != root {
		t.Fatalf("bind = %v", bound)
	}
	res := c.call("exec.run", map[string]any{"command": "pwd"})
	if !strings.Contains(res["output"].(string), root) {
		t.Fatalf("bound exec ran elsewhere: %v", res["output"])
	}
	// Explicit workspace still wins over the binding.
	res = c.call("exec.run", map[string]any{"command": "pwd", "workspace": other})
	if !strings.Contains(res["output"].(string), other) {
		t.Fatalf("explicit workspace ignored: %v", res["output"])
	}
	// Unbind falls back to most-recently-opened (other).
	c.call("workspace.unbind", nil)
	res = c.call("exec.run", map[string]any{"command": "pwd"})
	if !strings.Contains(res["output"].(string), other) {
		t.Fatalf("unbound exec resolved wrong ws: %v", res["output"])
	}
}

func TestServicePortEnv(t *testing.T) {
	sock, root := setup(t)
	c := dial(t, sock)
	defer func() { _ = c.conn.Close() }()
	c.call("workspace.open", map[string]any{"root": root})

	svc := c.call("service.start", map[string]any{
		"name": "websvc", "command": "echo port=$MYPORT; sleep 60",
		"portEnv": "MYPORT",
	})
	port := svc["port"].(float64)
	if port <= 0 {
		t.Fatalf("no port assigned: %v", svc)
	}
	time.Sleep(300 * time.Millisecond)
	logs := c.call("service.logs", map[string]any{"name": "websvc"})
	if !strings.Contains(logs["logs"].(string), "port=") {
		t.Fatalf("port env not injected: %v", logs)
	}
	list := c.call("service.list", map[string]any{"workspace": root})
	s := list["services"].([]any)[0].(map[string]any)
	if s["port"].(float64) != port {
		t.Fatalf("snapshot port = %v", s)
	}
	c.call("service.stop", map[string]any{"name": "websvc"})
}

func TestServiceWait(t *testing.T) {
	sock, root := setup(t)
	c := dial(t, sock)
	defer func() { _ = c.conn.Close() }()
	c.call("workspace.open", map[string]any{"root": root})

	c.call("service.start", map[string]any{"name": "svc", "command": "sleep 60"})
	w := c.call("service.wait", map[string]any{"name": "svc", "timeoutSec": 5})
	if w["ready"] != true || w["running"] != true {
		t.Fatalf("service.wait = %v", w)
	}
	c.call("service.stop", map[string]any{"name": "svc"})
	time.Sleep(200 * time.Millisecond)
	errMap := c.callErr("service.wait", map[string]any{"name": "svc", "timeoutSec": 1})
	if !strings.Contains(errMap["message"].(string), "exited") {
		t.Fatalf("unexpected error: %v", errMap)
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
