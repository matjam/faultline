package adminhttp

import (
	"net/http"
	"runtime"
	"time"

	"github.com/matjam/faultline/internal/version"
)

// pageData is the common set of fields every full-page render needs:
// auth state for the layout, navigation metadata for the sidebar +
// navbar, and a few global stats the layout footer surfaces. Every
// concrete page-data struct embeds this so the layout can reach the
// fields uniformly.
type pageData struct {
	Title         string
	Authenticated bool
	Username      string
	CSRFToken     string

	// Section is the current sidebar section's stable key
	// ("dashboard", "configuration", ...). Used by the layout to
	// mark the active nav row.
	Section string
	// SectionLabel is the human-facing title shown in the navbar
	// for the current page.
	SectionLabel string

	// Nav is the list of sidebar items, with .Active set on the
	// current page.
	Nav []navItem

	// Common stats surfaced in the layout footer / navbar.
	Version   string
	GoVersion string
	Uptime    string
	StartedAt string

	UI       string
	Theme    string
	IsModern bool
	IsMatrix bool
}

type navItem struct {
	Key    string
	Href   string
	Label  string
	Active bool
}

// navItems is the canonical sidebar order. Adding a new section is
// a one-line change here plus a new content template + route.
var navItems = []navItem{
	{Key: "dashboard", Href: "/admin", Label: "dashboard"},
	{Key: "configuration", Href: "/admin/configuration", Label: "configuration"},
	{Key: "subagents", Href: "/admin/subagents", Label: "subagent"},
	{Key: "skills", Href: "/admin/skills", Label: "skills"},
	{Key: "version", Href: "/admin/version", Label: "version"},
	{Key: "logs", Href: "/admin/logs", Label: "logs"},
}

// sectionLabels maps section keys to the navbar title.
var sectionLabels = map[string]string{
	"dashboard":     "dashboard",
	"configuration": "configuration",
	"subagents":     "subagent stream",
	"skills":        "skills catalog",
	"version":       "version & updates",
	"logs":          "log stream",
}

// basePageData fills the boilerplate every authenticated page needs.
// Section must be a key present in navItems; an unknown key still
// renders but no row is highlighted.
func (s *Server) basePageData(r *http.Request, section string) pageData {
	sess := sessionFromContext(r.Context())

	uptime := time.Since(s.deps.StartedAt).Round(time.Second)

	nav := make([]navItem, len(navItems))
	for i, it := range navItems {
		it.Active = it.Key == section
		nav[i] = it
	}

	label, ok := sectionLabels[section]
	if !ok {
		label = section
	}

	pd := pageData{
		Title:         label,
		Authenticated: true,
		Section:       section,
		SectionLabel:  label,
		Nav:           nav,
		Version:       version.String(),
		GoVersion:     runtime.Version(),
		Uptime:        uptime.String(),
		StartedAt:     s.deps.StartedAt.UTC().Format(time.RFC3339),
		UI:            s.deps.UI,
		Theme:         themeForUI(s.deps.UI),
		IsModern:      s.deps.UI == "modern",
		IsMatrix:      s.deps.UI == "matrix",
	}
	if sess != nil {
		pd.Username = sess.Username
		pd.CSRFToken = sess.CSRFToken
	}
	return pd
}

func themeForUI(ui string) string {
	if ui == "modern" {
		return "light"
	}
	return "matrix"
}
