// Package adminhttp implements the embedded HTTP admin UI driving
// adapter for Faultline. It listens on a loopback address (no TLS
// in v1; reverse-proxy TLS termination is the documented path) and
// serves an HTMX + DaisyUI front-end backed by html/template
// rendering.
//
// In stage 2 this package only carries the skeleton: login, logout,
// session and CSRF middleware, embedded asset serving, and a stub
// dashboard. Live status, config editing, skill toggling, and
// statistics land in subsequent stages.
//
// Auth is delegated to internal/adapters/auth/users (argon2id
// password hashing, in-memory sessions). The admin server has no
// view of the agent's domain ports yet — those will be added as
// AgentInspector / SubagentInspector / ToolObserver / ConfigStore
// in stage 3+.
package adminhttp

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/matjam/faultline/internal/adapters/auth/users"
)

// Deps bundles the dependencies the composition root injects.
type Deps struct {
	// Bind is the address:port to listen on (e.g. "127.0.0.1:8742").
	Bind string

	// Users is the loaded user store.
	Users *users.Store

	// Sessions is the in-memory session store. The Server does not
	// own its lifecycle — the composition root does, so the same
	// store survives across server restarts (e.g. on config edit
	// triggering a graceful restart in a later stage).
	Sessions *users.SessionStore

	// StartedAt is the wall-clock time the agent process started;
	// surfaced on the dashboard.
	StartedAt time.Time

	// Logger is the shared slog logger.
	Logger *slog.Logger

	// Agent is the live primary agent inspector. nil-allowed: when
	// not wired the dashboard renders empty placeholders. Stage 3
	// onward expects this to be set.
	Agent AgentInspector

	// Subagents is the live subagent.Manager inspector. nil-allowed
	// when [subagent] is disabled.
	Subagents SubagentInspector

	// Tools is the in-memory tool-call ring buffer. The same
	// instance is wired into the primary's tools.Executor as the
	// Observer, so the admin UI sees every dispatch. nil-allowed.
	Tools *ToolBuffer

	// Skills is the read+write port for the Skills page.
	// nil-allowed (skills feature off, or stage 5 not yet wired).
	Skills SkillsAdmin
}

// Server is the HTTP admin UI server. Construct with New, run with
// Run; Shutdown is idempotent.
type Server struct {
	deps      Deps
	templates map[string]*template.Template // contentName -> layout+content combined
	fragments map[string]*template.Template // fragName -> standalone template
	staticSub fs.FS

	srv      *http.Server
	stopOnce sync.Once
}

// contentTemplates is the fixed set of content templates we ship.
// Each entry combines layout.html with that content file at parse
// time, producing a template whose root entry point is "layout".
//
// We pre-parse all combinations at startup rather than parsing per
// request, but each entry is a separate *template.Template so the
// {{define "content"}} blocks across content files don't collide
// (which they would in a single ParseFS-parsed set, with the
// last-parsed win).
var contentTemplates = []string{
	"login.html",
	"dashboard.html",
}

// fragmentTemplates are stand-alone HTMX-fragment templates rendered
// without a surrounding layout. Each file defines a single named
// template (matching the file's basename minus .html) which the
// fragment handlers execute.
var fragmentTemplates = []string{
	"frag_status.html",
	"frag_tools.html",
	"frag_subagents.html",
	"frag_skills.html",
}

// New parses templates and prepares the static-file sub-FS. Returns
// an error if the embedded template set fails to parse — that's a
// programmer error and should fail loudly.
func New(deps Deps) (*Server, error) {
	if deps.Bind == "" {
		return nil, errors.New("adminhttp: empty bind address")
	}
	if deps.Users == nil {
		return nil, errors.New("adminhttp: nil user store")
	}
	if deps.Sessions == nil {
		return nil, errors.New("adminhttp: nil session store")
	}
	if deps.Logger == nil {
		return nil, errors.New("adminhttp: nil logger")
	}

	layoutBytes, err := fs.ReadFile(templateFS, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("adminhttp: read layout: %w", err)
	}
	funcs := templateFuncs()

	tmpls := make(map[string]*template.Template, len(contentTemplates))
	for _, name := range contentTemplates {
		contentBytes, err := fs.ReadFile(templateFS, "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("adminhttp: read %s: %w", name, err)
		}
		t, err := template.New(name).Funcs(funcs).Parse(string(layoutBytes) + string(contentBytes))
		if err != nil {
			return nil, fmt.Errorf("adminhttp: parse %s: %w", name, err)
		}
		tmpls[name] = t
	}

	frags := make(map[string]*template.Template, len(fragmentTemplates))
	for _, name := range fragmentTemplates {
		fragBytes, err := fs.ReadFile(templateFS, "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("adminhttp: read %s: %w", name, err)
		}
		t, err := template.New(name).Funcs(funcs).Parse(string(fragBytes))
		if err != nil {
			return nil, fmt.Errorf("adminhttp: parse %s: %w", name, err)
		}
		frags[name] = t
	}

	staticSub, err := fs.Sub(staticFS, "assets")
	if err != nil {
		return nil, fmt.Errorf("adminhttp: sub-fs assets: %w", err)
	}

	return &Server{deps: deps, templates: tmpls, fragments: frags, staticSub: staticSub}, nil
}

// Run binds the listener and serves until ctx is canceled or
// Shutdown is called. Returns nil on graceful shutdown, or a non-nil
// error for bind failures or other unrecoverable conditions.
//
// The server registers its handlers on a fresh ServeMux; nothing in
// this package mutates DefaultServeMux.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	s.routes(mux)

	s.srv = &http.Server{
		Addr:              s.deps.Bind,
		Handler:           s.requestLogger(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Shut the server down when the parent context fires.
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		s.shutdown()
		close(shutdownDone)
	}()

	s.deps.Logger.Info("admin server listening", "bind", s.deps.Bind)
	err := s.srv.ListenAndServe()
	<-shutdownDone

	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server. Idempotent; safe to call
// after Run has already returned.
func (s *Server) Shutdown() {
	s.shutdown()
}

// SetInspectors swaps in the live agent + subagent inspectors after
// construction. Used by the composition root because the agent
// constructor in turn depends on the tools.Executor (which we feed
// the admin's ToolBuffer), so the order has to be:
//
//  1. build admin (owns the tool buffer)
//  2. build tools.Executor with admin's tool buffer as Observer
//  3. build agent on top of the executor
//  4. attach inspectors back to admin
//
// Either argument may be nil; nil disables that section of the
// dashboard rather than producing an error.
func (s *Server) SetInspectors(agent AgentInspector, subs SubagentInspector) {
	s.deps.Agent = agent
	s.deps.Subagents = subs
}

// SetSkillsAdmin wires the Skills toggle port. Separate from
// SetInspectors because the skills store is constructed before the
// agent (no ordering dependency on the tool buffer), but kept on
// its own setter for clarity.
func (s *Server) SetSkillsAdmin(sk SkillsAdmin) {
	s.deps.Skills = sk
}

func (s *Server) shutdown() {
	s.stopOnce.Do(func() {
		if s.srv == nil {
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			s.deps.Logger.Warn("admin server shutdown error", "error", err)
		}
	})
}

// routes wires every handler. Stage 2 surface area: static, login,
// logout, dashboard, healthz.
func (s *Server) routes(mux *http.ServeMux) {
	staticHandler := http.StripPrefix("/admin/static/", http.FileServer(http.FS(s.staticSub)))

	mux.Handle("GET /admin/static/", staticHandler)

	mux.HandleFunc("GET /admin/healthz", s.handleHealthz)
	mux.HandleFunc("GET /admin/login", s.handleLoginGet)
	mux.HandleFunc("POST /admin/login", s.handleLoginPost)
	mux.HandleFunc("POST /admin/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("GET /admin/{$}", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("GET /admin", s.requireAuth(s.handleDashboard))

	// Live HTMX fragments. Polled by the dashboard at low frequency
	// (1–2s) so the operator sees fresh state without the cost of
	// SSE plumbing. All require auth.
	mux.HandleFunc("GET /admin/fragments/status", s.requireAuth(s.handleFragStatus))
	mux.HandleFunc("GET /admin/fragments/tools", s.requireAuth(s.handleFragTools))
	mux.HandleFunc("GET /admin/fragments/subagents", s.requireAuth(s.handleFragSubagents))
	mux.HandleFunc("GET /admin/fragments/skills", s.requireAuth(s.handleFragSkills))

	// Skills toggle action. Re-renders the skills fragment so
	// HTMX can hx-swap the updated card without a full page load.
	mux.HandleFunc("POST /admin/skills/toggle", s.requireAuth(s.handleSkillsToggle))

	// Anything not under /admin gets 404. We intentionally don't
	// take over /; the agent doesn't expose anything else on this
	// port, and forwarding "/" to "/admin" would trip up reverse
	// proxies that expect the prefix to be explicit.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// render executes the layout template for the named content. Each
// content name has been pre-parsed at startup combined with the
// layout, so there's no per-request parsing or cloning.
func (s *Server) render(w http.ResponseWriter, contentName string, data any) {
	t, ok := s.templates[contentName]
	if !ok {
		s.deps.Logger.Error("render: unknown content template", "name", contentName)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		// Headers are likely already flushed by the time Execute
		// returns; logging is the only useful action.
		s.deps.Logger.Error("render: execute", "template", contentName, "error", err)
	}
}
