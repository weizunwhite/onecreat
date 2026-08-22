// Package runtime gives OneCreat's resources an explicit lifetime.
//
// # Why
//
// Today every resource a run needs — MCP plugin subprocesses, the CodeGraph
// daemon, LSP servers, background jobs — is created inside boot.Build and
// chained into a single `cleanup` closure that Controller.Close() runs. There is
// exactly one lifetime, and it is the *session's*. That is wrong for anything
// shared: two desktop tabs open on the same project each build their own
// runtime, so closing the first tab stops the CodeGraph daemon the second tab is
// still using. Nothing in the type system says which resources may be shared,
// who owns them, or in what order they must be released — so the answer has to
// be re-derived by reading boot.Build every time.
//
// This package makes that answer explicit. Four scopes, nested:
//
//	Process      one per running OneCreat
//	 └ Workspace one per project directory — SHARED by every session in it
//	    └ Session one conversation (a desktop tab, a CLI run, an ACP session)
//	       └ Turn  one user turn: the model request plus its tool calls
//
// # The five questions, answered once
//
//	          created by        held by        closed by            shared?
//	Process   the frontend      the frontend   the frontend         n/a
//	Workspace Process.Open…     Process        last Release, or     YES, by root
//	                                           Process.Close
//	Session   Workspace.New…    Workspace      Session.Close, or    never
//	                                           its Workspace
//	Turn      Session.Begin…    Session        Turn.End/Cancel, or  never
//	                                           its Session
//
// What must never cross a boundary: a resource registered on a Turn must not
// outlive that turn, and a Session must never register into its Workspace's
// scope. The API enforces this by construction — Defer is a method on the scope
// you hold, so you can only attach a resource to a lifetime you actually have.
//
// # Close ordering
//
// Closing a scope closes its children first (a parent's resources may still be
// needed while a child unwinds), then its own resources in reverse registration
// order (later resources may depend on earlier ones). Close is idempotent, and
// a panicking closer does not prevent the rest from running.
//
// # Status
//
// Plan 03 establishes the ownership model and this skeleton. Wiring the real
// resources onto it is Plan 04 (a single composition root) and Plan 05
// (MCP / CodeGraph / LSP / jobs lifetimes). Until then boot.Build's behaviour is
// unchanged — see docs/架构重构执行总计划.md.
package runtime

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"reasonix/internal/workspace"
)

// closer is one registered resource: a name (for diagnostics and ordering
// tests) and the function that releases it.
type closer struct {
	name string
	fn   func()
}

// scope is the shared machinery of every level: a cancellable context, an
// ordered list of resources, and the child scopes it owns.
//
// It is embedded, not exported: callers hold a *Process / *Workspace /
// *Session / *Turn, which is what makes "you can only attach a resource to a
// lifetime you hold" a compile-time property rather than a convention.
type scope struct {
	name   string
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closers  []closer
	children []*scope
	closed   bool
}

func newScope(parent context.Context, name string) *scope {
	ctx, cancel := context.WithCancel(parent)
	return &scope{name: name, ctx: ctx, cancel: cancel}
}

// Context is this scope's context. It is cancelled when the scope closes, and
// inherits cancellation from every enclosing scope — so cancelling a Session
// cancels its Turns, but never the other way round.
func (s *scope) Context() context.Context { return s.ctx }

// Name identifies the scope in diagnostics.
func (s *scope) Name() string { return s.name }

// Closed reports whether this scope has been closed.
func (s *scope) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Defer registers a resource to release when this scope closes. Registration
// order matters: resources are released in reverse, so a resource may depend on
// anything registered before it.
//
// Registering on an already-closed scope releases immediately rather than
// leaking — a resource that arrives after its lifetime ended has no owner left.
func (s *scope) Defer(name string, fn func()) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		safeRun(fn)
		return
	}
	s.closers = append(s.closers, closer{name: name, fn: fn})
	s.mu.Unlock()
}

// adopt makes child a dependent of s. A closed parent closes the child at once,
// so a scope opened while its parent is shutting down cannot outlive it.
func (s *scope) adopt(child *scope) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		child.close()
		return false
	}
	s.children = append(s.children, child)
	s.mu.Unlock()
	return true
}

// forget drops child from s. Used when a child closes on its own, so a
// long-lived parent does not accumulate dead children.
func (s *scope) forget(child *scope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.children {
		if c == child {
			s.children = append(s.children[:i], s.children[i+1:]...)
			return
		}
	}
}

// close releases this scope: cancel the context, close children (newest first),
// then own resources in reverse registration order. Idempotent.
func (s *scope) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	children := s.children
	closers := s.closers
	s.children, s.closers = nil, nil
	s.mu.Unlock()

	// Cancel first so in-flight work starts unwinding while we release.
	s.cancel()

	for i := len(children) - 1; i >= 0; i-- {
		children[i].close()
	}
	for i := len(closers) - 1; i >= 0; i-- {
		safeRun(closers[i].fn)
	}
}

// safeRun runs one closer, containing a panic so a single misbehaving resource
// cannot abort the rest of the shutdown.
func safeRun(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// --- Process ---------------------------------------------------------------

// Process is the outermost scope: one per running OneCreat. It owns everything
// that is genuinely global — account credentials, shared HTTP clients, the
// updater — and the open workspaces.
type Process struct {
	*scope

	mu         sync.Mutex
	workspaces map[string]*Workspace
}

// NewProcess opens the process scope. ctx is the process's root context; closing
// the Process cancels it for every scope beneath.
func NewProcess(ctx context.Context) *Process {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Process{
		scope:      newScope(ctx, "process"),
		workspaces: map[string]*Workspace{},
	}
}

// OpenWorkspace returns the workspace scope for ws, creating it on first use.
//
// Workspaces are **shared**: opening the same root twice hands back the same
// scope with one more holder, so two sessions on one project share its
// CodeGraph, LSP servers and skill index instead of each starting a copy. Every
// OpenWorkspace must be matched by a Release; the workspace closes when the last
// holder releases it, or when the Process closes — whichever comes first.
//
// The zero workspace.Context (no explicit project) is a valid, distinct root.
func (p *Process) OpenWorkspace(ws workspace.Context) *Workspace {
	key := ws.Root()
	p.mu.Lock()
	if w, ok := p.workspaces[key]; ok {
		w.mu.Lock()
		w.holders++
		w.mu.Unlock()
		p.mu.Unlock()
		return w
	}
	w := &Workspace{
		scope:   newScope(p.scope.ctx, "workspace("+describeRoot(key)+")"),
		ws:      ws,
		proc:    p,
		key:     key,
		holders: 1,
	}
	p.workspaces[key] = w
	p.mu.Unlock()

	if !p.scope.adopt(w.scope) {
		// Process already closing: the workspace was closed by adopt; drop it
		// from the registry so a later OpenWorkspace does not hand out a corpse.
		p.mu.Lock()
		if p.workspaces[key] == w {
			delete(p.workspaces, key)
		}
		p.mu.Unlock()
	}
	return w
}

// Workspaces lists the currently open workspace roots, sorted — for diagnostics
// and tests.
func (p *Process) Workspaces() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.workspaces))
	for k := range p.workspaces {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Close releases the whole tree: every workspace, their sessions, their turns.
func (p *Process) Close() {
	p.scope.close()
	p.mu.Lock()
	p.workspaces = map[string]*Workspace{}
	p.mu.Unlock()
}

// --- Workspace -------------------------------------------------------------

// Workspace is one project directory's scope, shared by every session open on
// it. It owns what belongs to the project rather than to a conversation: the
// resolved config, the skill and memory indexes, CodeGraph, LSP servers, and
// workspace-level MCP servers.
type Workspace struct {
	*scope

	ws   workspace.Context
	proc *Process
	key  string

	mu       sync.Mutex
	holders  int
	sessions int
	seq      int
}

// Workspace is the project root this scope belongs to.
func (w *Workspace) Workspace() workspace.Context { return w.ws }

// NewSession opens a session scope under this workspace. id names it in
// diagnostics; an empty id gets a generated one.
//
// A session never owns workspace resources — it holds the workspace, it does not
// consume it. Closing the session leaves everything workspace-scoped running for
// the other sessions.
func (w *Workspace) NewSession(id string) *Session {
	w.mu.Lock()
	if id == "" {
		w.seq++
		id = fmt.Sprintf("session%d", w.seq)
	}
	w.sessions++
	w.mu.Unlock()

	s := &Session{
		scope: newScope(w.scope.ctx, "session("+id+")"),
		id:    id,
		ws:    w,
	}
	if !w.scope.adopt(s.scope) {
		w.mu.Lock()
		w.sessions--
		w.mu.Unlock()
	}
	return s
}

// Sessions is the number of live sessions on this workspace.
func (w *Workspace) Sessions() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sessions
}

// Holders is the number of outstanding OpenWorkspace calls not yet released.
func (w *Workspace) Holders() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.holders
}

// Release gives up one hold on this workspace. The workspace closes — releasing
// its shared resources — only when the last holder lets go. It reports whether
// this call was the one that closed it.
//
// Sessions are *not* holders: a caller that opened the workspace keeps it alive
// until it releases, regardless of how many sessions came and went.
func (w *Workspace) Release() bool {
	w.mu.Lock()
	if w.holders == 0 {
		// Already fully released. An over-release must not report a second
		// close — a caller acting on "true" would tear down resources a later
		// OpenWorkspace has since handed to somebody else.
		w.mu.Unlock()
		return false
	}
	w.holders--
	last := w.holders == 0
	w.mu.Unlock()
	if !last {
		return false
	}
	w.closeAndDeregister()
	return true
}

func (w *Workspace) closeAndDeregister() {
	if w.proc != nil {
		w.proc.mu.Lock()
		if w.proc.workspaces[w.key] == w {
			delete(w.proc.workspaces, w.key)
		}
		w.proc.mu.Unlock()
		w.proc.scope.forget(w.scope)
	}
	w.scope.close()
}

// --- Session ---------------------------------------------------------------

// Session is one conversation's scope: the message log, its checkpoint journal,
// approval state, session-level MCP overlays, and the plan/coach state. A
// desktop tab, a `reasonix chat` run and an ACP session are each one Session.
type Session struct {
	*scope

	id string
	ws *Workspace

	mu   sync.Mutex
	turn *Turn
	seq  int
}

// ID names this session.
func (s *Session) ID() string { return s.id }

// WorkspaceScope is the workspace this session runs in.
func (s *Session) WorkspaceScope() *Workspace { return s.ws }

// BeginTurn opens a turn scope. A session runs one turn at a time: an already
// running turn is ended first, so a caller cannot leak the previous turn's
// context by starting another.
func (s *Session) BeginTurn() *Turn {
	s.mu.Lock()
	if prev := s.turn; prev != nil {
		s.mu.Unlock()
		prev.End()
		s.mu.Lock()
	}
	s.seq++
	t := &Turn{
		scope: newScope(s.scope.ctx, fmt.Sprintf("turn(%s#%d)", s.id, s.seq)),
		sess:  s,
	}
	s.turn = t
	s.mu.Unlock()

	s.scope.adopt(t.scope)
	return t
}

// CurrentTurn is the running turn, or nil.
func (s *Session) CurrentTurn() *Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

// Close ends this session: its turn, then its own resources. Workspace
// resources are untouched — they belong to the workspace, which may still have
// other sessions on it.
func (s *Session) Close() {
	if s.ws != nil {
		s.ws.mu.Lock()
		if s.ws.sessions > 0 {
			s.ws.sessions--
		}
		s.ws.mu.Unlock()
		s.ws.scope.forget(s.scope)
	}
	s.scope.close()
}

// --- Turn ------------------------------------------------------------------

// Turn is one user turn's scope: the request context and cancel handle, the tool
// batch, usage, and any per-turn temporary state. Cancelling a turn stops that
// turn and nothing else — the session survives and can start another.
type Turn struct {
	*scope
	sess *Session
}

// End closes the turn normally.
func (t *Turn) End() { t.finish() }

// Cancel stops the turn. It is End by another name: a cancelled turn releases
// exactly what a completed one does, and never touches its session.
func (t *Turn) Cancel() { t.finish() }

func (t *Turn) finish() {
	if t.sess != nil {
		t.sess.mu.Lock()
		if t.sess.turn == t {
			t.sess.turn = nil
		}
		t.sess.mu.Unlock()
		t.sess.scope.forget(t.scope)
	}
	t.scope.close()
}

func describeRoot(root string) string {
	if root == "" {
		return "process-cwd"
	}
	return root
}
