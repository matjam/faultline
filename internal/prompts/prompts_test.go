package prompts

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// memStore is a minimal in-memory Store for tests. The prompts package
// doesn't import the real MemoryStore (would create a dependency cycle
// once memory is extracted into its own package), so we ship a tiny fake
// with only the methods the Store interface requires.
type memStore struct {
	data map[string]string
}

func newMemStore() *memStore {
	return &memStore{data: map[string]string{}}
}

func (m *memStore) Read(path string) (string, error) {
	v, ok := m.data[path]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (m *memStore) Write(path, content string) error {
	m.data[path] = content
	return nil
}

// Move and Delete satisfy the Migrator interface so tests can exercise
// prompts.Migrate without dragging in the real fs.Store.
func (m *memStore) Move(src, dst string) error {
	v, ok := m.data[src]
	if !ok {
		return errors.New("not found")
	}
	if _, exists := m.data[dst]; exists {
		return errors.New("destination exists")
	}
	m.data[dst] = v
	delete(m.data, src)
	return nil
}

func (m *memStore) Delete(path string) error {
	if _, ok := m.data[path]; !ok {
		return errors.New("not found")
	}
	delete(m.data, path)
	return nil
}

func TestMigrate_RenamesLegacyCycleStart(t *testing.T) {
	m := newMemStore()
	const content = "rich agent-written cycle-start prompt"
	if err := m.Write("prompts/cycle_start.md", content); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(m); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if _, err := m.Read("prompts/cycle_start.md"); err == nil {
		t.Error("legacy file should have been removed after migration")
	}
	got, err := m.Read("prompts/cycle-start.md")
	if err != nil {
		t.Fatalf("new file should exist: %v", err)
	}
	if got != content {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}
}

func TestMigrate_NoOpWhenOnlyNewExists(t *testing.T) {
	m := newMemStore()
	if err := m.Write("prompts/cycle-start.md", "current"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(m); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got, _ := m.Read("prompts/cycle-start.md")
	if got != "current" {
		t.Errorf("new file changed unexpectedly: %q", got)
	}
}

func TestMigrate_ErrorWhenBothExist(t *testing.T) {
	m := newMemStore()
	_ = m.Write("prompts/cycle_start.md", "old content")
	_ = m.Write("prompts/cycle-start.md", "new content")

	err := Migrate(m)
	if err == nil {
		t.Fatal("expected error when both files exist")
	}
	// Both files should still be present (no destructive action).
	if _, err := m.Read("prompts/cycle_start.md"); err != nil {
		t.Error("old file should still exist after conflict")
	}
	if _, err := m.Read("prompts/cycle-start.md"); err != nil {
		t.Error("new file should still exist after conflict")
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	m := newMemStore()
	_ = m.Write("prompts/cycle_start.md", "x")
	if err := Migrate(m); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(m); err != nil {
		t.Fatalf("second Migrate should be a no-op, got: %v", err)
	}
}

func TestRender(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 30, 0, 0, time.UTC)
	tpl := "Hello, the time is {{TIME}}. Goodbye."
	got := Render(tpl, now)
	want := "Hello, the time is " + now.Format(time.RFC1123) + ". Goodbye."
	if got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

func TestRender_NoPlaceholder(t *testing.T) {
	tpl := "no placeholders here"
	if got := Render(tpl, time.Now()); got != tpl {
		t.Errorf("expected unchanged template, got %q", got)
	}
}

func TestRender_MultipleOccurrences(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tpl := "{{TIME}} - {{TIME}}"
	got := Render(tpl, now)
	stamp := now.Format(time.RFC1123)
	want := stamp + " - " + stamp
	if got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

func TestLoad_SeedsDefault(t *testing.T) {
	m := newMemStore()

	got, err := Load(m, "system")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != defaultSystem {
		t.Error("Load should return embedded default on first load")
	}

	// File should now exist in the store
	stored, err := m.Read("prompts/system.md")
	if err != nil {
		t.Fatalf("expected seeded file: %v", err)
	}
	if stored != defaultSystem {
		t.Error("seeded file content does not match embedded default")
	}
}

func TestDefaultSystemPromptIncludesMCPGuidance(t *testing.T) {
	for _, want := range []string{
		"mcp_discover_tools",
		"allow_tools",
		"collaborator approval",
		"mcp/<server>",
		"stdio MCP",
		"runtime_notes",
		"/output",
		"/mcp/<server>",
		"npm install --prefix /node",
		"/node/node_modules/.bin",
		"git-diff-style Markdown code block",
	} {
		if !strings.Contains(defaultSystem, want) {
			t.Fatalf("default system prompt missing %q", want)
		}
	}
}

// TestDefaultSystemPromptIncludesAutonomyConventions guards the
// identity-vs-operating split and the changelog convention added in the
// autonomy-prompts-v1 work. The matching migration (001) gates its
// system.md edits on the presence of these substrings, so accidentally
// removing them from the default would silently re-apply the migration
// on every fresh deployment.
func TestDefaultSystemPromptIncludesAutonomyConventions(t *testing.T) {
	for _, want := range []string{
		"identity/core.md",
		"prompts/changelog.md",
		"meta/state-summary.md",
	} {
		if !strings.Contains(defaultSystem, want) {
			t.Fatalf("default system prompt missing %q", want)
		}
	}
}

func TestLoad_PreservesUserEdits(t *testing.T) {
	m := newMemStore()
	custom := "MY CUSTOM SYSTEM PROMPT"
	if err := m.Write("prompts/system.md", custom); err != nil {
		t.Fatal(err)
	}

	got, err := Load(m, "system")
	if err != nil {
		t.Fatal(err)
	}
	if got != custom {
		t.Errorf("Load returned %q, want %q (should not overwrite existing)", got, custom)
	}
}

func TestLoad_UnknownName(t *testing.T) {
	m := newMemStore()
	if _, err := Load(m, "no-such-prompt"); err == nil {
		t.Error("expected error for unknown prompt name")
	}
}

func TestLoadAll(t *testing.T) {
	m := newMemStore()
	prompts, err := LoadAll(m)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"system", "compaction", "cycle-start", "continue", "shutdown", "identity-core", "changelog"} {
		if _, ok := prompts[want]; !ok {
			t.Errorf("LoadAll missing prompt %q", want)
		}
	}
}

// TestLoadAllSeedsAutonomyConventions verifies that loading the
// identity-core and changelog prompts seeds them at the documented
// memory paths (identity/core.md and prompts/changelog.md), not at the
// default prompts/<name>.md locations. Matters because the migration
// idempotency check and the system.md text both reference those exact
// paths.
func TestLoadAllSeedsAutonomyConventions(t *testing.T) {
	m := newMemStore()
	if _, err := LoadAll(m); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Read("identity/core.md"); err != nil {
		t.Errorf("identity/core.md not seeded: %v", err)
	}
	if _, err := m.Read("prompts/changelog.md"); err != nil {
		t.Errorf("prompts/changelog.md not seeded: %v", err)
	}
}
