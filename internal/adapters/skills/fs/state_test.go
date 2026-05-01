package fs

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSkillTree creates a minimal valid skill at <root>/<name>/SKILL.md
// for tests that need the catalog to contain a few skills.
func fakeSkillTree(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		skill := "---\nname: " + name + "\ndescription: test skill " + name + "\n---\n\nbody\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skill), 0o644); err != nil {
			t.Fatalf("write SKILL.md: %v", err)
		}
	}
}

func newQuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestLoadDisabledFromFile_MissingFileIsOK(t *testing.T) {
	root := t.TempDir()
	fakeSkillTree(t, root, "alpha", "beta")

	store, err := New(root, newQuietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Path that doesn't exist; LoadDisabledFromFile must not error.
	missing := filepath.Join(t.TempDir(), "skills.toml")
	if err := store.LoadDisabledFromFile(missing); err != nil {
		t.Fatalf("LoadDisabledFromFile(missing): %v", err)
	}
	if got := len(store.List()); got != 2 {
		t.Fatalf("List len = %d, want 2 (no disables)", got)
	}
}

func TestLoadDisabledFromFile_HidesFromList(t *testing.T) {
	root := t.TempDir()
	fakeSkillTree(t, root, "alpha", "beta", "gamma")

	store, err := New(root, newQuietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stateFile := filepath.Join(t.TempDir(), "skills.toml")
	contents := "disabled = [\"alpha\", \"gamma\"]\n"
	if err := os.WriteFile(stateFile, []byte(contents), 0o644); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	if err := store.LoadDisabledFromFile(stateFile); err != nil {
		t.Fatalf("LoadDisabledFromFile: %v", err)
	}

	got := store.List()
	if len(got) != 1 {
		t.Fatalf("List len = %d, want 1", len(got))
	}
	if got[0].Name != "beta" {
		t.Fatalf("List[0] = %q, want beta", got[0].Name)
	}

	// Get on a disabled skill must return ErrNotFound (we
	// deliberately don't distinguish "missing" from "disabled" to
	// the agent).
	if _, err := store.Get("alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(disabled) err = %v, want ErrNotFound", err)
	}

	// ListAll surfaces every skill with the Disabled flag so the
	// admin UI can render toggles.
	all := store.ListAll()
	if len(all) != 3 {
		t.Fatalf("ListAll len = %d, want 3", len(all))
	}
	for _, s := range all {
		want := s.Name == "alpha" || s.Name == "gamma"
		if s.Disabled != want {
			t.Errorf("%s Disabled = %v, want %v", s.Name, s.Disabled, want)
		}
	}
}

func TestSetEnabled_PersistsAndRoundTrips(t *testing.T) {
	root := t.TempDir()
	fakeSkillTree(t, root, "alpha", "beta")

	store, err := New(root, newQuietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stateFile := filepath.Join(t.TempDir(), "skills.toml")
	if err := store.LoadDisabledFromFile(stateFile); err != nil {
		t.Fatalf("LoadDisabledFromFile: %v", err)
	}

	// Disable alpha.
	if err := store.SetEnabled("alpha", false); err != nil {
		t.Fatalf("SetEnabled(alpha,false): %v", err)
	}
	if !store.IsDisabled("alpha") {
		t.Fatal("alpha should be disabled")
	}
	if got := len(store.List()); got != 1 {
		t.Fatalf("List len = %d, want 1", got)
	}

	// File on disk must mention alpha.
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if !strings.Contains(string(data), `"alpha"`) {
		t.Fatalf("state file does not list alpha:\n%s", data)
	}

	// Re-load from disk into a fresh Store and confirm the
	// persisted state survived.
	store2, err := New(root, newQuietLogger())
	if err != nil {
		t.Fatalf("New (second): %v", err)
	}
	if err := store2.LoadDisabledFromFile(stateFile); err != nil {
		t.Fatalf("LoadDisabledFromFile (second): %v", err)
	}
	if !store2.IsDisabled("alpha") {
		t.Fatal("disabled state did not survive reload")
	}
	if store2.IsDisabled("beta") {
		t.Fatal("beta should not be disabled")
	}

	// Re-enable alpha; file must no longer list it.
	if err := store2.SetEnabled("alpha", true); err != nil {
		t.Fatalf("SetEnabled(alpha,true): %v", err)
	}
	data, err = os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read after re-enable: %v", err)
	}
	if strings.Contains(string(data), `"alpha"`) {
		t.Fatalf("state file still lists alpha after re-enable:\n%s", data)
	}
}

func TestSetEnabled_UnknownSkill(t *testing.T) {
	root := t.TempDir()
	fakeSkillTree(t, root, "alpha")

	store, err := New(root, newQuietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.LoadDisabledFromFile(filepath.Join(t.TempDir(), "skills.toml")); err != nil {
		t.Fatalf("LoadDisabledFromFile: %v", err)
	}
	if err := store.SetEnabled("ghost", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown skill, got %v", err)
	}
}

func TestLoadDisabledFromFile_BadTOML(t *testing.T) {
	root := t.TempDir()
	fakeSkillTree(t, root, "alpha")

	store, err := New(root, newQuietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stateFile := filepath.Join(t.TempDir(), "skills.toml")
	if err := os.WriteFile(stateFile, []byte("this is not = valid = toml ====\n"), 0o644); err != nil {
		t.Fatalf("write bad state file: %v", err)
	}
	if err := store.LoadDisabledFromFile(stateFile); err == nil {
		t.Fatal("expected parse error on bad TOML, got nil")
	}
}
