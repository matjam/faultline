package adminhttp

import "net/http"

// handleConfigRestart is the small action the Configuration page's
// post-save "Restart agent now" button posts to. It has its own
// route (rather than being folded into /admin/configuration/save)
// so the operator can choose to save without restarting and trigger
// the restart later from the same page.
//
// CSRF is enforced by the surrounding requireAuth wrapper, so this
// handler only has to fire the shutdown closure and re-render.
func (s *Server) handleConfigRestart(w http.ResponseWriter, r *http.Request) {
	if s.deps.Config == nil {
		http.Error(w, "config store not wired", http.StatusServiceUnavailable)
		return
	}
	s.deps.Logger.Info("admin: restart requested via configuration page")
	s.deps.Config.Restart()

	// Re-render the configuration page so the operator gets a
	// confirmation flash (the agent is shutting down behind the
	// scenes; the response body will likely render before the
	// process exits).
	data := s.buildConfigurationPage(r, nil, false)
	data.FlashOK = "Restart signal sent. The agent is shutting down; the supervisor (or configured restart_mode) will bring the new process up."
	s.render(w, "configuration.html", data)
}
