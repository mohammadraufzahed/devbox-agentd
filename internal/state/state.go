// Package state holds the daemon's in-memory model: workspaces, exec
// buffers, services, and pty sessions.
package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Workspace is a bound project root.
type Workspace struct {
	Root     string    `json:"root"`
	Name     string    `json:"name"`
	OpenedAt time.Time `json:"openedAt"`
	Devbox   bool      `json:"devbox"` // devbox.json present at root

	openedSeq uint64
}

// Exec is a finished or running exec.run invocation.
type Exec struct {
	ID        string    `json:"id"`
	Workspace string    `json:"workspace"`
	Command   string    `json:"command"`
	StartedAt time.Time `json:"startedAt"`
	ExitCode  *int      `json:"exitCode,omitempty"`
	Done      bool      `json:"done"`
	Output    *Ring     `json:"-"`
}

// Follow is a service-logs follower registered by the api layer.
type Follow struct {
	C     chan string
	Close func()
}

// Service is a managed long-running process.
type Service struct {
	Name      string    `json:"name"`
	Workspace string    `json:"workspace"`
	Command   string    `json:"command"`
	Running   bool      `json:"running"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	ExitCode  *int      `json:"exitCode,omitempty"`
	Logs      *Ring     `json:"-"`

	mu      sync.Mutex
	proc    *os.Process
	follows map[*Follow]struct{}
}

// Pty is a pseudo-session backed by pipes (no real PTY).
type Pty struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Command   string `json:"command"`
	Running   bool   `json:"running"`

	mu     sync.Mutex
	stdin  *os.File
	proc   *os.Process
	output *Ring
}

// Attach records the pty's process handles and marks it running.
func (p *Pty) Attach(stdin *os.File, proc *os.Process, out *Ring) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stdin, p.proc, p.output = stdin, proc, out
	p.Running = true
}

// WriteStdin writes to the pty's stdin pipe.
func (p *Pty) WriteStdin(b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return fmt.Errorf("pty %s is not running", p.ID)
	}
	_, err := p.stdin.Write(b)
	return err
}

// Output returns the pty output ring buffer.
func (p *Pty) Output() *Ring { return p.output }

// Kill terminates the pty process. Safe to call multiple times.
func (p *Pty) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.proc != nil {
		_ = p.proc.Kill()
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
		p.stdin = nil
	}
	p.Running = false
}

// Attach records the service's process and marks it running.
func (svc *Service) Attach(proc *os.Process) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.proc = proc
	svc.Running = true
	svc.PID = proc.Pid
	svc.StartedAt = time.Now()
	svc.ExitCode = nil
}

// Exited records the service's exit code.
func (svc *Service) Exited(code int) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.Running = false
	svc.ExitCode = &code
}

// AddFollow registers a log follower; returns an unregister func.
func (svc *Service) AddFollow(f *Follow) func() {
	svc.mu.Lock()
	if svc.follows == nil {
		svc.follows = map[*Follow]struct{}{}
	}
	svc.follows[f] = struct{}{}
	svc.mu.Unlock()
	return func() {
		svc.mu.Lock()
		delete(svc.follows, f)
		svc.mu.Unlock()
	}
}

// PushLog appends output and fans it out to followers.
func (svc *Service) PushLog(chunk string) {
	_, _ = svc.Logs.Write([]byte(chunk))
	svc.mu.Lock()
	follows := make([]*Follow, 0, len(svc.follows))
	for f := range svc.follows {
		follows = append(follows, f)
	}
	svc.mu.Unlock()
	for _, f := range follows {
		select {
		case f.C <- chunk:
		default: // drop rather than block the service on a slow follower
		}
	}
}

// Kill terminates the service process and marks it stopped. Safe to call
// multiple times.
func (svc *Service) Kill() {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.proc != nil {
		_ = svc.proc.Kill()
	}
	svc.Running = false
}

// Ring is a bounded tail buffer.
type Ring struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

// NewRing returns a Ring keeping at most cap bytes.
func NewRing(cap int) *Ring { return &Ring{cap: cap} }

// Write appends p, keeping only the tail.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		r.buf = append([]byte(nil), r.buf[len(r.buf)-r.cap:]...)
	}
	return len(p), nil
}

// Bytes returns the buffered tail.
func (r *Ring) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf...)
}

// Store is the daemon state container.
type Store struct {
	mu         sync.Mutex
	workspaces map[string]*Workspace // keyed by root
	execs      map[string]*Exec
	services   map[string]*Service // keyed root+"\x00"+name
	ptys       map[string]*Pty
	seq        atomic.Uint64
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{
		workspaces: map[string]*Workspace{},
		execs:      map[string]*Exec{},
		services:   map[string]*Service{},
		ptys:       map[string]*Pty{},
	}
}

// OpenWorkspace registers root (creating it if missing) and returns the
// workspace. Idempotent.
func (s *Store) OpenWorkspace(root string) (*Workspace, error) {
	if root == "" {
		return nil, fmt.Errorf("workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("workspace root %q is not a directory", abs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ws, ok := s.workspaces[abs]; ok {
		return ws, nil
	}
	ws := &Workspace{
		Root:      abs,
		Name:      filepath.Base(abs),
		OpenedAt:  time.Now(),
		Devbox:    fileExists(filepath.Join(abs, "devbox.json")),
		openedSeq: s.seq.Add(1),
	}
	s.workspaces[abs] = ws
	return ws, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ResolveWorkspace returns the workspace for root, auto-opening it. When
// root is empty the most recently opened workspace (or an error) is
// returned.
func (s *Store) ResolveWorkspace(root string) (*Workspace, error) {
	if root != "" {
		return s.OpenWorkspace(root)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *Workspace
	for _, ws := range s.workspaces {
		if latest == nil || ws.openedSeq > latest.openedSeq {
			latest = ws
		}
	}
	if latest == nil {
		return nil, fmt.Errorf("no workspace open; pass workspace (root path) or call workspace.open first")
	}
	return latest, nil
}

// ListWorkspaces returns all open workspaces.
func (s *Store) ListWorkspaces() []*Workspace {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Workspace, 0, len(s.workspaces))
	for _, ws := range s.workspaces {
		out = append(out, ws)
	}
	return out
}

// DestroyWorkspace removes the workspace and returns a cleanup func that
// stops its services/pty sessions (invoked after lock release).
func (s *Store) DestroyWorkspace(root string) (func(), error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.workspaces[abs]; !ok {
		return nil, fmt.Errorf("workspace %q is not open", abs)
	}
	delete(s.workspaces, abs)
	var svcs []*Service
	var ptys []*Pty
	for k, svc := range s.services {
		if svc.Workspace == abs {
			delete(s.services, k)
			svcs = append(svcs, svc)
		}
	}
	for k, p := range s.ptys {
		if p.Workspace == abs {
			delete(s.ptys, k)
			ptys = append(ptys, p)
		}
	}
	return func() {
		for _, svc := range svcs {
			svc.Kill()
		}
		for _, p := range ptys {
			p.Kill()
		}
	}, nil
}

// NewID returns a short unique id with the given prefix.
func (s *Store) NewID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, s.seq.Add(1))
}

func serviceKey(root, name string) string { return root + "\x00" + name }

// PutExec stores e.
func (s *Store) PutExec(e *Exec) { s.mu.Lock(); s.execs[e.ID] = e; s.mu.Unlock() }

// GetExec returns the exec by id.
func (s *Store) GetExec(id string) (*Exec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.execs[id]
	return e, ok
}

// PutService stores svc under its workspace+name key.
func (s *Store) PutService(root string, svc *Service) {
	s.mu.Lock()
	s.services[serviceKey(root, svc.Name)] = svc
	s.mu.Unlock()
}

// GetService returns the service for root+name.
func (s *Store) GetService(root, name string) (*Service, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.services[serviceKey(root, name)]
	return svc, ok
}

// DeleteService removes the service for root+name.
func (s *Store) DeleteService(root, name string) {
	s.mu.Lock()
	delete(s.services, serviceKey(root, name))
	s.mu.Unlock()
}

// ListServices returns all services, or those of one workspace.
func (s *Store) ListServices(root string) []*Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Service
	for _, svc := range s.services {
		if root == "" || svc.Workspace == root {
			out = append(out, svc)
		}
	}
	return out
}

// PutPty stores p.
func (s *Store) PutPty(p *Pty) { s.mu.Lock(); s.ptys[p.ID] = p; s.mu.Unlock() }

// GetPty returns the pty by id.
func (s *Store) GetPty(id string) (*Pty, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.ptys[id]
	return p, ok
}

// DeletePty removes the pty.
func (s *Store) DeletePty(id string) { s.mu.Lock(); delete(s.ptys, id); s.mu.Unlock() }
