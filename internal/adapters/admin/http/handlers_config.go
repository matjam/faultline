package adminhttp

import (
	"net/http"
	"strings"
)

// fragConfigData backs frag_config.html. Either Raw or Err is
// populated when reading from disk fails; the rest are presentation
// helpers.
type fragConfigData struct {
	HasConfig bool

	Path string
	Raw  string

	// FlashOK / FlashErr surface the result of validate / save /
	// restart. ReadErr is populated when the disk read itself
	// failed; the textarea is then disabled.
	FlashOK  string
	FlashErr string
	ReadErr  string

	// Saved is set after a successful save so the UI can prompt
	// the operator to restart (without losing the just-saved
	// content).
	Saved bool

	CSRFToken string
}

func (s *Server) handleFragConfig(w http.ResponseWriter, r *http.Request) {
	data := s.buildConfigFragmentData(r, "")
	s.renderFragment(w, "frag_config.html", data)
}

func (s *Server) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Config == nil {
		http.Error(w, "config store not wired", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	raw := r.PostFormValue("content")
	data := s.buildConfigFragmentData(r, raw)
	if err := s.deps.Config.Validate([]byte(raw)); err != nil {
		data.FlashErr = "Validation failed: " + err.Error()
	} else {
		data.FlashOK = "Looks good. The config parses cleanly and would load on next start."
	}
	s.renderFragment(w, "frag_config.html", data)
}

func (s *Server) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	if s.deps.Config == nil {
		http.Error(w, "config store not wired", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	raw := r.PostFormValue("content")
	data := s.buildConfigFragmentData(r, raw)
	if err := s.deps.Config.Write([]byte(raw)); err != nil {
		s.deps.Logger.Warn("admin: config save failed", "error", err)
		data.FlashErr = "Save failed: " + err.Error()
	} else {
		s.deps.Logger.Info("admin: config saved", "path", s.deps.Config.Path())
		data.FlashOK = "Saved. Restart the agent to apply changes."
		data.Saved = true
	}
	s.renderFragment(w, "frag_config.html", data)
}

func (s *Server) handleConfigRestart(w http.ResponseWriter, r *http.Request) {
	if s.deps.Config == nil {
		http.Error(w, "config store not wired", http.StatusServiceUnavailable)
		return
	}
	s.deps.Logger.Info("admin: restart requested via config page")
	s.deps.Config.Restart()
	data := s.buildConfigFragmentData(r, "")
	data.FlashOK = "Restart signal sent. The agent is shutting down; the supervisor (or configured restart_mode) will bring the new process up."
	s.renderFragment(w, "frag_config.html", data)
}

func (s *Server) buildConfigFragmentData(r *http.Request, override string) fragConfigData {
	data := fragConfigData{}
	if s.deps.Config == nil {
		return data
	}
	data.HasConfig = true
	data.Path = s.deps.Config.Path()
	if sess := sessionFromContext(r.Context()); sess != nil {
		data.CSRFToken = sess.CSRFToken
	}

	if override != "" {
		// The form posted content; show it back so the operator
		// doesn't lose their edits when re-rendering.
		data.Raw = override
	} else {
		raw, err := s.deps.Config.Read()
		if err != nil {
			data.ReadErr = err.Error()
		} else {
			// Trim trailing whitespace and ensure the textarea
			// shows the file as-is. CRLF stays as written.
			data.Raw = strings.TrimRight(string(raw), "\n") + "\n"
		}
	}
	return data
}
