package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/matjam/faultline/internal/skills"
)

// stateFileShape is the on-disk shape of skills.toml. Kept simple:
// a single top-level array of disabled skill names. Adding fields
// later is trivial because BurntSushi/toml ignores unknown keys.
type stateFileShape struct {
	Disabled []string `toml:"disabled"`
}

// LoadDisabledFromFile reads a TOML file naming skills the operator
// has marked disabled and applies the result to the in-memory state.
// Disabled skills disappear from List() and Get() — they are
// invisible to the agent — but ListAll() still surfaces them so the
// admin UI can re-enable them.
//
// A missing file is not an error: it means "no skills disabled".
// Parse errors are returned so misconfiguration is loud, not silent.
//
// path is recorded so subsequent SetEnabled calls can rewrite it.
func (s *Store) LoadDisabledFromFile(path string) error {
	s.mu.Lock()
	s.stateFile = path
	if s.disabled == nil {
		s.disabled = make(map[string]bool)
	}
	s.mu.Unlock()

	if path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// Empty disabled set; file will be created on first
		// SetEnabled(name, false).
		return nil
	}
	if err != nil {
		return fmt.Errorf("read skills state file: %w", err)
	}

	var shape stateFileShape
	if err := toml.Unmarshal(data, &shape); err != nil {
		return fmt.Errorf("parse skills state file: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.disabled = make(map[string]bool, len(shape.Disabled))
	for _, name := range shape.Disabled {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		s.disabled[name] = true
	}
	return nil
}

// IsDisabled reports whether the named skill is operator-disabled.
// Always false when no state file has been loaded.
func (s *Store) IsDisabled(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.disabled != nil && s.disabled[name]
}

// SetEnabled toggles the enabled/disabled state of the named skill
// and persists the result to the state file. Returns ErrNotFound if
// the skill is not in the catalog (we want to refuse setting a
// state for a skill that doesn't exist; otherwise stale entries
// could pile up in the file). Idempotent: setting an already-
// enabled skill to enabled is a no-op write.
func (s *Store) SetEnabled(name string, enabled bool) error {
	s.mu.Lock()
	if _, ok := s.catalog[name]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if s.disabled == nil {
		s.disabled = make(map[string]bool)
	}
	if enabled {
		delete(s.disabled, name)
	} else {
		s.disabled[name] = true
	}
	statePath := s.stateFile
	disabled := make([]string, 0, len(s.disabled))
	for n := range s.disabled {
		disabled = append(disabled, n)
	}
	s.mu.Unlock()

	sort.Strings(disabled)
	if statePath == "" {
		// No file path configured: in-memory toggle only.
		return nil
	}
	return writeSkillsStateFile(statePath, disabled)
}

// ListAll returns every skill in the catalog, including operator-
// disabled ones. The returned skills carry the disabled flag in
// AllSkill.Disabled so the admin UI can render them appropriately.
//
// The agent's existing List() filters disabled skills out, so the
// agent never sees them in its system prompt.
func (s *Store) ListAll() []AllSkill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AllSkill, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, AllSkill{
			Skill:    *s.catalog[name],
			Disabled: s.disabled[name],
		})
	}
	return out
}

// AllSkill pairs a Skill with its admin-side disabled flag. Returned
// from ListAll. The Disabled field is true when the operator has
// turned the skill off via skills.toml.
type AllSkill struct {
	skills.Skill
	Disabled bool
}

// writeSkillsStateFile persists the disabled list with a header
// comment so an operator inspecting the file by hand sees what it's
// for and where it's read from. Atomic write via temp + rename.
func writeSkillsStateFile(path string, disabled []string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".skills-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		// Best-effort cleanup if rename below didn't happen.
		_ = os.Remove(tmpPath)
	}()

	var b strings.Builder
	b.WriteString("# Faultline skills enable/disable state.\n")
	b.WriteString("#\n")
	b.WriteString("# Edited via the admin UI's Skills page; safe to inspect by hand\n")
	b.WriteString("# but the file may be rewritten on any toggle. Skills listed in\n")
	b.WriteString("# `disabled` are hidden from the agent's catalog (the agent does\n")
	b.WriteString("# not see them in its system prompt). Skills not listed here are\n")
	b.WriteString("# enabled — there is no allow-list mode.\n")
	b.WriteString("#\n")
	fmt.Fprintf(&b, "# Last written: %s\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("\n")
	b.WriteString("disabled = [")
	if len(disabled) > 0 {
		b.WriteString("\n")
		for _, name := range disabled {
			fmt.Fprintf(&b, "  %q,\n", name)
		}
	}
	b.WriteString("]\n")

	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
