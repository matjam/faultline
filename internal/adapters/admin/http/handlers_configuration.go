package adminhttp

import (
	"bytes"
	"net/http"

	"github.com/BurntSushi/toml"
	"github.com/matjam/faultline/internal/config"
)

// configurationPage backs configuration.html. The form descriptor is
// computed by reflecting over a freshly-loaded *config.Config so the
// UI never drifts from the struct shape — adding a field to
// config.Config wires it into the form automatically.
type configurationPage struct {
	pageData

	HasConfig bool
	Path      string
	ReadErr   string

	Form ConfigForm

	FlashOK  string
	FlashErr string
	Warnings []string
	Saved    bool
}

func (s *Server) handleConfiguration(w http.ResponseWriter, r *http.Request) {
	data := s.buildConfigurationPage(r, nil, false)
	s.render(w, "configuration.html", data)
}

func (s *Server) handleConfigurationSave(w http.ResponseWriter, r *http.Request) {
	if s.deps.Config == nil {
		http.Error(w, "config store not wired", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Load the on-disk config as the baseline so secrets the form
	// didn't repost (and any TOML the form doesn't surface) are
	// preserved.
	cfg, readErr := s.loadConfig()
	if readErr != nil {
		data := s.buildConfigurationPage(r, nil, false)
		data.FlashErr = "Could not read existing config: " + readErr.Error()
		s.render(w, "configuration.html", data)
		return
	}

	warnings := ApplyConfigForm(cfg, r.PostForm)

	// Encode the modified struct back to TOML.
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		data := s.buildConfigurationPage(r, cfg, false)
		data.FlashErr = "Encode failed: " + err.Error()
		data.Warnings = warnings
		s.render(w, "configuration.html", data)
		return
	}

	// Validate + atomic write via the existing port. Validate is
	// run as part of Write, so a bad encode is rejected before
	// touching disk.
	if err := s.deps.Config.Write(buf.Bytes()); err != nil {
		s.deps.Logger.Warn("admin: configuration save failed", "error", err)
		data := s.buildConfigurationPage(r, cfg, false)
		data.FlashErr = "Save failed: " + err.Error()
		data.Warnings = warnings
		s.render(w, "configuration.html", data)
		return
	}

	s.deps.Logger.Info("admin: configuration saved", "path", s.deps.Config.Path())
	data := s.buildConfigurationPage(r, cfg, true)
	data.FlashOK = "Saved. Restart the agent to apply changes."
	data.Warnings = warnings
	s.render(w, "configuration.html", data)
}

// buildConfigurationPage assembles the data the template renders.
// override, when non-nil, supplies the in-memory config to render
// (used after a save so the form reflects the operator's just-applied
// values rather than re-reading the file). When nil we read from disk.
func (s *Server) buildConfigurationPage(r *http.Request, override *config.Config, saved bool) configurationPage {
	data := configurationPage{
		pageData: s.basePageData(r, "configuration"),
		Saved:    saved,
	}
	if s.deps.Config == nil {
		return data
	}
	data.HasConfig = true
	data.Path = s.deps.Config.Path()

	cfg := override
	if cfg == nil {
		var err error
		cfg, err = s.loadConfig()
		if err != nil {
			data.ReadErr = err.Error()
			return data
		}
	}
	data.Form = BuildConfigForm(cfg)
	return data
}

// loadConfig reads the configured TOML file and parses it on top of
// config.Default(). Mirrors config.Load semantics minus the
// nonsense-zero-duration backfills (which we don't want on this read
// path: an operator who just typed "0" should see "0" until they
// hit Save).
func (s *Server) loadConfig() (*config.Config, error) {
	if s.deps.Config == nil {
		return nil, errConfigNotWired
	}
	raw, err := s.deps.Config.Read()
	if err != nil {
		return nil, err
	}
	cfg := config.Default()
	if len(raw) > 0 {
		if err := toml.Unmarshal(raw, cfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// errConfigNotWired is returned when loadConfig is called without a
// store. Stable error for tests.
var errConfigNotWired = configNotWiredError{}

type configNotWiredError struct{}

func (configNotWiredError) Error() string { return "config store not wired" }
