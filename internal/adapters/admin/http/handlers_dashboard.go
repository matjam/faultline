package adminhttp

import (
	"html/template"
	"net/http"
	"time"

	"github.com/matjam/faultline/internal/agent"
	"github.com/matjam/faultline/internal/subagent"
	"github.com/matjam/faultline/internal/tools"
)

// templateFuncs returns the FuncMap shared by every template the
// admin server renders. Keeping these in one place makes it obvious
// to a reader which helpers are available, and keeps the dashboard
// templates terse.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"formatDuration": FormatDuration,
		"formatRelative": FormatRelative,
		"sinceShort": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return FormatDuration(time.Since(t))
		},
		"percent": func(part, whole int) int {
			if whole <= 0 {
				return 0
			}
			p := part * 100 / whole
			if p < 0 {
				return 0
			}
			if p > 100 {
				return 100
			}
			return p
		},
		"phaseClass": func(p agent.Phase) string {
			switch p {
			case agent.PhaseGenerating:
				return "badge-info"
			case agent.PhaseExecutingTool:
				return "badge-secondary"
			case agent.PhaseCompacting:
				return "badge-warning"
			case agent.PhaseSaving:
				return "badge-warning"
			case agent.PhaseStopped:
				return "badge-error"
			case agent.PhaseInitializing:
				return "badge-ghost"
			default:
				return "badge-success"
			}
		},
		"errorClass": func(err string) string {
			if err == "" {
				return "badge-success"
			}
			return "badge-error"
		},
	}
}

// dashboardPage is the data passed to dashboard.html on first load.
// The fragments below carry their own narrower view models.
type dashboardPage struct {
	pageData
	SessionCount int
}

// fragStatusData backs frag_status.html — the agent status card.
type fragStatusData struct {
	HasAgent bool
	Snapshot agent.AgentSnapshot

	Uptime            string
	PhaseSince        string
	LastChatRelative  string
	LastErrorRelative string

	TokenPercent      int
	CompactionPercent int
}

// fragToolsData backs frag_tools.html — the recent tool calls table.
type fragToolsData struct {
	Events []tools.ToolCallEvent
	Newest []tools.ToolCallEvent
	Total  int
	Cap    int
}

// fragSubagentsData backs frag_subagents.html — the live children
// table.
type fragSubagentsData struct {
	HasManager bool
	Active     []subagent.ActiveStatus
	Profiles   []subagent.Profile
}

// handleDashboard renders the dashboard shell. The dynamic fragments
// (status, tools) load via HTMX after first paint.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := dashboardPage{
		pageData:     s.basePageData(r, "dashboard"),
		SessionCount: s.deps.Sessions.Count(),
	}
	s.render(w, "dashboard.html", data)
}

// handleSubagentsPage renders the subagent section.
func (s *Server) handleSubagentsPage(w http.ResponseWriter, r *http.Request) {
	data := struct {
		pageData
	}{pageData: s.basePageData(r, "subagents")}
	s.render(w, "subagents.html", data)
}

// handleSkillsPage renders the skills section.
func (s *Server) handleSkillsPage(w http.ResponseWriter, r *http.Request) {
	data := struct {
		pageData
	}{pageData: s.basePageData(r, "skills")}
	s.render(w, "skills.html", data)
}

// handleVersionPage renders the version & updates section.
func (s *Server) handleVersionPage(w http.ResponseWriter, r *http.Request) {
	data := struct {
		pageData
	}{pageData: s.basePageData(r, "version")}
	s.render(w, "version.html", data)
}

// handleFragStatus renders the live agent status fragment.
func (s *Server) handleFragStatus(w http.ResponseWriter, _ *http.Request) {
	data := fragStatusData{}
	if s.deps.Agent != nil {
		snap := s.deps.Agent.Snapshot()
		data.HasAgent = true
		data.Snapshot = snap
		data.Uptime = FormatDuration(time.Since(snap.StartedAt))
		data.PhaseSince = FormatDuration(time.Since(snap.PhaseSince))
		data.LastChatRelative = FormatRelative(snap.LastChatAt)
		data.LastErrorRelative = FormatRelative(snap.LastErrorAt)

		if snap.MaxTokens > 0 {
			data.TokenPercent = snap.TokenEstimate * 100 / snap.MaxTokens
		}
		if snap.CompactionThreshold > 0 {
			p := snap.TokenEstimate * 100 / snap.CompactionThreshold
			if p > 100 {
				p = 100
			}
			data.CompactionPercent = p
		}
	}
	s.renderFragment(w, "frag_status.html", data)
}

// handleFragTools renders the recent tool-call feed.
func (s *Server) handleFragTools(w http.ResponseWriter, _ *http.Request) {
	data := fragToolsData{}
	if s.deps.Tools != nil {
		data.Newest = reverseEvents(s.deps.Tools.SnapshotRecent(50))
		data.Events = data.Newest
		data.Total = s.deps.Tools.Len()
		data.Cap = s.deps.Tools.Cap()
	}
	s.renderFragment(w, "frag_tools.html", data)
}

// handleFragSubagents renders the subagent panel.
func (s *Server) handleFragSubagents(w http.ResponseWriter, _ *http.Request) {
	data := fragSubagentsData{}
	if s.deps.Subagents != nil {
		data.HasManager = true
		data.Active = s.deps.Subagents.Status()
		data.Profiles = s.deps.Subagents.Profiles()
	}
	s.renderFragment(w, "frag_subagents.html", data)
}

// renderFragment executes a fragment template (no layout). Any
// rendering error is logged; partial writes can't be unwound by
// http.Error so we don't try.
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	t, ok := s.fragments[name]
	if !ok {
		s.deps.Logger.Error("renderFragment: unknown template", "name", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := t.Execute(w, data); err != nil {
		s.deps.Logger.Error("renderFragment: execute",
			"template", name, "error", err)
	}
}

// reverseEvents returns a copy of events with the order flipped.
func reverseEvents(events []tools.ToolCallEvent) []tools.ToolCallEvent {
	out := make([]tools.ToolCallEvent, len(events))
	for i, e := range events {
		out[len(events)-1-i] = e
	}
	return out
}
