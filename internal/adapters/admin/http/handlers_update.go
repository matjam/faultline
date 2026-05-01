package adminhttp

import (
	"net/http"
	"time"

	"github.com/matjam/faultline/internal/update"
)

// fragUpdateData backs frag_update.html. The fields all derive from
// the cached update.State plus a couple of presentation-side
// helpers; we deliberately do not call Check() from the dashboard
// poll path (that would hit GitHub on every refresh).
type fragUpdateData struct {
	HasUpdater bool

	Enabled         bool
	CurrentVersion  string
	LatestVersion   string
	UpdateAvailable bool
	LastChecked     string // formatted relative
	Note            string
	Err             string

	// FlashOK / FlashErr surface the result of an Apply.
	FlashOK  string
	FlashErr string

	CSRFToken string
}

func (s *Server) handleFragUpdate(w http.ResponseWriter, r *http.Request) {
	data := s.buildUpdateFragmentData(r)
	s.renderFragment(w, "frag_update.html", data)
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if s.deps.Update == nil {
		http.Error(w, "updater not wired", http.StatusServiceUnavailable)
		return
	}
	if !s.deps.Update.Enabled() {
		// Re-render the fragment with an explanatory flash.
		data := s.buildUpdateFragmentData(r)
		data.FlashErr = "Self-update is disabled in [update]; cannot apply."
		s.renderFragment(w, "frag_update.html", data)
		return
	}

	// Apply blocks while it downloads + swaps + records history,
	// then triggers shutdown. We give it a generous deadline so a
	// slow tarball download doesn't cut the process off.
	ctx, cancel := contextWithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	res, err := s.deps.Update.Apply(ctx)
	data := s.buildUpdateFragmentData(r)
	if err != nil {
		s.deps.Logger.Warn("admin: update apply failed", "error", err)
		data.FlashErr = "Update failed: " + err.Error()
	} else {
		s.deps.Logger.Info("admin: update applied; agent will shut down",
			"from", res.FromVersion, "to", res.ToVersion)
		data.FlashOK = "Updated " + res.FromVersion + " → " + res.ToVersion +
			". The agent is shutting down for the new binary to take over."
		// Reflect the current binary as the new one in the UI;
		// the old State still shows the pre-update value because
		// no Check has run since.
		data.CurrentVersion = res.ToVersion
		data.UpdateAvailable = false
	}
	s.renderFragment(w, "frag_update.html", data)
}

func (s *Server) buildUpdateFragmentData(r *http.Request) fragUpdateData {
	data := fragUpdateData{}
	if s.deps.Update == nil {
		return data
	}
	data.HasUpdater = true
	data.Enabled = s.deps.Update.Enabled()
	data.CurrentVersion = s.deps.Update.CurrentVersion()
	if sess := sessionFromContext(r.Context()); sess != nil {
		data.CSRFToken = sess.CSRFToken
	}

	st := s.deps.Update.State()
	data.LatestVersion = st.LatestVersion
	data.UpdateAvailable = st.UpdateAvailable
	if !st.LastChecked.IsZero() {
		data.LastChecked = FormatRelative(st.LastChecked)
	} else {
		data.LastChecked = "—"
	}
	data.Note = st.Note
	if st.Err != nil {
		data.Err = st.Err.Error()
	}
	return data
}

// stateString is a small helper used by tests to make assertions
// easier; not used at runtime.
//
//nolint:unused // referenced in tests via package-internal access
func stateString(st update.State) string {
	if st.UpdateAvailable {
		return st.CurrentVersion + " -> " + st.LatestVersion
	}
	return st.CurrentVersion + " (current)"
}
