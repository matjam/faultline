// Package adminhttp implements the embedded HTTP admin UI driving
// adapter for Faultline. It listens on a loopback address (no TLS;
// reverse-proxy TLS termination is the documented path) and serves
// an HTMX + DaisyUI front-end backed by html/template rendering.
//
// The UI is a multi-section dashboard with a persistent sidebar +
// navbar shell. Each section is its own page (full-reload navigation)
// with HTMX fragments for live data inside the page. The Matrix
// terminal theme is custom DaisyUI v5 plus the local CSS in
// assets/terminal.css.
//
// Auth is delegated to internal/adapters/auth/users (argon2id
// password hashing, in-memory sessions). The admin server depends on
// the agent's domain ports through the consumer-defined inspector
// interfaces in inspector.go.
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

	// Sessions is the in-memory session store.
	Sessions *users.SessionStore

	// StartedAt is the wall-clock time the agent process started.
	StartedAt time.Time

	// Logger is the shared slog logger. Used for everything except
	// per-request access logs.
	Logger *slog.Logger

	// RequestLogger is the dedicated slog logger for the per-request
	// access log. The dashboard polls a handful of fragments every
	// 1-2 seconds, so this stream is spammy by design — split out
	// of the main log so the operator can ignore it without losing
	// visibility into agent / config / shutdown events. nil-allowed:
	// when not wired, request logs fall back to Logger.
	RequestLogger *slog.Logger

	// LogDir is the directory holding the daily-rotated agent log
	// files. The Logs section reads <LogDir>/YYYY-MM-DD.log to tail
	// today's output. Empty disables the logs page (renders a
	// not-configured placeholder).
	LogDir string

	// Agent is the live primary agent inspector.
	Agent AgentInspector

	// Subagents is the live subagent.Manager inspector.
	Subagents SubagentInspector

	// Tools is the in-memory tool-call ring buffer.
	Tools *ToolBuffer

	// Skills is the read+write port for the Skills section.
	Skills SkillsAdmin

	// Update is the read+write port for the self-update pane.
	Update UpdateInspector

	// Config is the read+write port for the configuration editor.
	Config ConfigStore
}

// Server is the HTTP admin UI server.
type Server struct {
	deps      Deps
	templates map[string]*template.Template
	fragments map[string]*template.Template
	staticSub fs.FS

	srv      *http.Server
	stopOnce sync.Once
}

// contentTemplates is the fixed set of (layout + content) templates
// we ship. Each is parsed once at startup; multiple ParseFS would
// collapse the {{define "content"}} blocks across content files.
var contentTemplates = []string{
	"login.html",
	"dashboard.html",
	"configuration.html",
	"subagents.html",
	"skills.html",
	"version.html",
	"logs.html",
}

// fragmentTemplates are stand-alone HTMX-fragment templates rendered
// without a surrounding layout.
var fragmentTemplates = []string{
	"frag_status.html",
	"frag_tools.html",
	"frag_subagents.html",
	"frag_skills.html",
	"frag_update.html",
	"frag_logs.html",
}

// New parses templates and prepares the static-file sub-FS.
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
// Shutdown is called.
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

// Shutdown gracefully stops the server.
func (s *Server) Shutdown() { s.shutdown() }

// SetInspectors swaps in the live agent + subagent inspectors after
// construction. Both nil-allowed.
func (s *Server) SetInspectors(agent AgentInspector, subs SubagentInspector) {
	s.deps.Agent = agent
	s.deps.Subagents = subs
}

// SetSkillsAdmin wires the Skills toggle port.
func (s *Server) SetSkillsAdmin(sk SkillsAdmin) { s.deps.Skills = sk }

// SetUpdateInspector wires the self-update read+write port.
func (s *Server) SetUpdateInspector(u UpdateInspector) { s.deps.Update = u }

// SetConfigStore wires the read+write configuration port.
func (s *Server) SetConfigStore(c ConfigStore) { s.deps.Config = c }

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

// routes wires every handler.
func (s *Server) routes(mux *http.ServeMux) {
	// Vendored static assets ship inside the binary, so they are
	// effectively immutable for the lifetime of a deployment. Tag
	// them with a far-future cache header so the browser doesn't
	// revalidate fonts / CSS / JS on every navigation — without
	// this, full-page navigations briefly redraw with fallback
	// fonts before the WOFF2 cache hit settles.
	staticHandler := http.StripPrefix("/admin/static/", http.FileServer(http.FS(s.staticSub)))
	mux.Handle("GET /admin/static/", cacheImmutable(staticHandler))

	mux.HandleFunc("GET /admin/healthz", s.handleHealthz)
	mux.HandleFunc("GET /admin/login", s.handleLoginGet)
	mux.HandleFunc("POST /admin/login", s.handleLoginPost)
	mux.HandleFunc("POST /admin/logout", s.requireAuth(s.handleLogout))

	// Pages: each section in the sidebar is its own GET endpoint.
	mux.HandleFunc("GET /admin/{$}", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("GET /admin", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("GET /admin/configuration", s.requireAuth(s.handleConfiguration))
	mux.HandleFunc("GET /admin/subagents", s.requireAuth(s.handleSubagentsPage))
	mux.HandleFunc("GET /admin/skills", s.requireAuth(s.handleSkillsPage))
	mux.HandleFunc("GET /admin/version", s.requireAuth(s.handleVersionPage))
	mux.HandleFunc("GET /admin/logs", s.requireAuth(s.handleLogsPage))

	// Live HTMX fragments. All require auth.
	mux.HandleFunc("GET /admin/fragments/status", s.requireAuth(s.handleFragStatus))
	mux.HandleFunc("GET /admin/fragments/tools", s.requireAuth(s.handleFragTools))
	mux.HandleFunc("GET /admin/fragments/subagents", s.requireAuth(s.handleFragSubagents))
	mux.HandleFunc("GET /admin/fragments/skills", s.requireAuth(s.handleFragSkills))
	mux.HandleFunc("GET /admin/fragments/update", s.requireAuth(s.handleFragUpdate))
	mux.HandleFunc("GET /admin/fragments/logs", s.requireAuth(s.handleFragLogs))

	// Skills toggle action.
	mux.HandleFunc("POST /admin/skills/toggle", s.requireAuth(s.handleSkillsToggle))

	// Update apply action.
	mux.HandleFunc("POST /admin/update/apply", s.requireAuth(s.handleUpdateApply))

	// Configuration save + restart.
	mux.HandleFunc("POST /admin/configuration/save", s.requireAuth(s.handleConfigurationSave))
	mux.HandleFunc("POST /admin/config/restart", s.requireAuth(s.handleConfigRestart))

	// Anything not under /admin gets 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// cacheImmutable wraps a handler so static-asset responses carry the
// "public, max-age=31536000, immutable" cache directive. The vendored
// assets are baked into the binary and never change for the lifetime
// of a deployment; revalidation on every page navigation is wasteful
// and visibly noisy (fonts re-applying mid-paint produces a flicker).
//
// On a binary upgrade the operator restarts the agent; the asset
// paths are unchanged but the contents may differ. That's acceptable
// here — admin-UI assets aren't safety-critical and a forced reload
// (Ctrl+Shift+R) clears the cache.
func cacheImmutable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
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
		s.deps.Logger.Error("render: execute", "template", contentName, "error", err)
	}
}
