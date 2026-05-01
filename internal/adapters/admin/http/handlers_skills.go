package adminhttp

import (
	"net/http"
	"strings"

	skillsfs "github.com/matjam/faultline/internal/adapters/skills/fs"
)

// fragSkillsData backs frag_skills.html — the per-skill toggle list.
type fragSkillsData struct {
	HasSkills bool
	Skills    []skillsfs.AllSkill
	CSRFToken string

	// FlashOK / FlashErr surface the result of a recent toggle when
	// the request came from the toggle endpoint and the response
	// re-renders the fragment with hx-swap. Both empty on an
	// ordinary list render.
	FlashOK  string
	FlashErr string
}

// handleFragSkills renders the skills toggle list. Polled by the
// dashboard at low frequency; also re-rendered as the response of
// POST /admin/skills/toggle so the operator sees the new state
// without a page reload.
func (s *Server) handleFragSkills(w http.ResponseWriter, r *http.Request) {
	data := s.buildSkillsFragmentData(r)
	s.renderFragment(w, "frag_skills.html", data)
}

// handleSkillsToggle is the POST endpoint backing the per-skill
// switch. Form fields: name=<skill>, enabled=on|off. After applying
// the change, re-renders the fragment so HTMX can hx-swap the
// updated row in.
//
// Auth + CSRF are enforced by requireAuth in the route registration.
func (s *Server) handleSkillsToggle(w http.ResponseWriter, r *http.Request) {
	if s.deps.Skills == nil {
		http.Error(w, "skills admin not configured", http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		http.Error(w, "missing skill name", http.StatusBadRequest)
		return
	}

	// Form encoding for a checkbox: when checked, the value is
	// posted as "on" (or whatever value attribute we pick); when
	// unchecked, the field is OMITTED. We look at presence rather
	// than value so the standard checkbox encoding works.
	enabled := r.PostForm.Has("enabled")

	data := s.buildSkillsFragmentData(r)
	if err := s.deps.Skills.SetEnabled(name, enabled); err != nil {
		s.deps.Logger.Warn("admin: skills toggle failed",
			"name", name, "enabled", enabled, "error", err)
		data.FlashErr = "Could not update " + name + ": " + err.Error()
	} else {
		state := "enabled"
		if !enabled {
			state = "disabled"
		}
		s.deps.Logger.Info("admin: skill toggled",
			"name", name, "state", state)
		data.FlashOK = name + " is now " + state + "."
		// Refresh the data after the mutation so the new state
		// renders in the response.
		data.Skills = s.deps.Skills.ListAll()
	}
	s.renderFragment(w, "frag_skills.html", data)
}

// buildSkillsFragmentData populates the fragment data. Lifted out of
// both handlers so the toggle handler can mutate Flash fields after
// reading the snapshot.
func (s *Server) buildSkillsFragmentData(r *http.Request) fragSkillsData {
	data := fragSkillsData{}
	if s.deps.Skills == nil {
		return data
	}
	data.HasSkills = true
	data.Skills = s.deps.Skills.ListAll()
	if sess := sessionFromContext(r.Context()); sess != nil {
		data.CSRFToken = sess.CSRFToken
	}
	return data
}
