// Package rpc implements a newline-delimited JSON-RPC 2.0 server over a
// Unix socket. Each accepted connection is an independent session; requests
// run on their own goroutine and may emit notifications back on the
// connection. A `$/cancel` notification carrying {"id": <request id>}
// cancels the in-flight request's context.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
)

// Version is the JSON-RPC protocol version string used on the wire.
const Version = "2.0"

// Error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeCancelled      = -32800
)

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Errf builds an *Error with a formatted message.
func Errf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Request is an incoming JSON-RPC message.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the request carries no id.
func (r *Request) IsNotification() bool { return len(r.ID) == 0 }

// response is an outgoing JSON-RPC message (result, error, or notification).
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
}

// Handler processes one request. Params is decoded by the caller into the
// handler's expected shape via DecodeParams.
type Handler func(ctx context.Context, conn *Conn, params json.RawMessage) (any, error)

// DecodeParams unmarshals request params into v.
func DecodeParams(params json.RawMessage, v any) error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, v); err != nil {
		return Errf(CodeInvalidParams, "invalid params: %v", err)
	}
	return nil
}

// Server dispatches JSON-RPC methods to registered handlers.
type Server struct {
	handlers map[string]Handler
	log      *slog.Logger

	// onActivity is invoked for every accepted connection and every
	// request; used by the daemon's idle-exit timer.
	onActivity func()
}

// NewServer returns a Server. onActivity may be nil.
func NewServer(log *slog.Logger, onActivity func()) *Server {
	return &Server{handlers: map[string]Handler{}, log: log, onActivity: onActivity}
}

// Handle registers a method handler.
func (s *Server) Handle(method string, h Handler) { s.handlers[method] = h }

// Serve accepts connections on l until l is closed or ctx is done.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if s.onActivity != nil {
			s.onActivity()
		}
		conn := newConn(c, s)
		go conn.serve()
	}
}

// Conn is a single client connection.
type Conn struct {
	c   net.Conn
	s   *Server
	wmu sync.Mutex
	bw  *bufio.Writer

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func newConn(c net.Conn, s *Server) *Conn {
	return &Conn{c: c, s: s, bw: bufio.NewWriter(c), cancels: map[string]context.CancelFunc{}}
}

// Notify writes a JSON-RPC notification (no id) on the connection.
// It is a no-op on a NoopConn (in-process callers without streaming).
func (conn *Conn) Notify(method string, params any) error {
	if conn == nil || conn.c == nil {
		return nil
	}
	return conn.write(response{JSONRPC: Version, Method: method, Params: params})
}

// NewNoopConn returns a *Conn whose Notify silently succeeds — for
// in-process dispatch (e.g. the MCP adapter) where there is no socket.
func NewNoopConn() *Conn { return &Conn{} }

// write serializes and flushes one message.
func (conn *Conn) write(m response) error {
	conn.wmu.Lock()
	defer conn.wmu.Unlock()
	if err := json.NewEncoder(conn.bw).Encode(m); err != nil {
		return err
	}
	return conn.bw.Flush()
}

func (conn *Conn) serve() {
	defer func() {
		conn.mu.Lock()
		for _, cancel := range conn.cancels {
			cancel()
		}
		conn.mu.Unlock()
		_ = conn.c.Close()
	}()

	sc := bufio.NewScanner(conn.c)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if conn.s.onActivity != nil {
			conn.s.onActivity()
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = conn.write(response{JSONRPC: Version, Error: Errf(CodeParseError, "parse error: %v", err)})
			continue
		}
		if req.JSONRPC != Version || req.Method == "" {
			conn.respond(req.ID, nil, Errf(CodeInvalidRequest, "invalid JSON-RPC 2.0 request"))
			continue
		}
		if req.IsNotification() {
			conn.handleNotification(&req)
			continue
		}
		go conn.handleRequest(&req)
	}
}

func (conn *Conn) handleNotification(req *Request) {
	if req.Method == "$/cancel" {
		var p struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(req.Params, &p); err == nil {
			conn.mu.Lock()
			if cancel, ok := conn.cancels[string(p.ID)]; ok {
				cancel()
			}
			conn.mu.Unlock()
		}
		return
	}
	// Other notifications (e.g. notifications/initialized on MCP) are ignored.
}

// reqIDKey carries the JSON-RPC request id through the handler context so
// streamed notifications can be correlated by the client.
type reqIDKey struct{}

// ReqID returns the raw JSON-RPC id of the in-flight request.
func ReqID(ctx context.Context) json.RawMessage {
	if v, ok := ctx.Value(reqIDKey{}).(json.RawMessage); ok {
		return v
	}
	return nil
}

func (conn *Conn) handleRequest(req *Request) {
	idKey := string(req.ID)
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, reqIDKey{}, req.ID)
	conn.mu.Lock()
	conn.cancels[idKey] = cancel
	conn.mu.Unlock()
	defer func() {
		conn.mu.Lock()
		delete(conn.cancels, idKey)
		conn.mu.Unlock()
		cancel()
	}()

	h, ok := conn.s.handlers[req.Method]
	if !ok {
		conn.respond(req.ID, nil, Errf(CodeMethodNotFound, "method not found: %s", req.Method))
		return
	}
	res, err := h(ctx, conn, req.Params)
	switch {
	case err == nil:
		conn.respond(req.ID, res, nil)
	case errors.Is(err, context.Canceled):
		conn.respond(req.ID, nil, Errf(CodeCancelled, "request cancelled"))
	default:
		var re *Error
		if errors.As(err, &re) {
			conn.respond(req.ID, nil, re)
		} else {
			conn.respond(req.ID, nil, Errf(CodeInternal, "%v", err))
		}
	}
}

func (conn *Conn) respond(id json.RawMessage, result any, e *Error) {
	m := response{JSONRPC: Version, ID: id}
	if e != nil {
		m.Error = e
	} else {
		m.Result = result
	}
	if err := conn.write(m); err != nil && !errors.Is(err, io.EOF) {
		conn.s.log.Debug("rpc write failed", "err", err)
	}
}
