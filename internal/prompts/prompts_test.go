package prompts

import (
	"errors"
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

	for _, want := range []string{"system", "compaction", "cycle_start", "continue", "shutdown"} {
		if _, ok := prompts[want]; !ok {
			t.Errorf("LoadAll missing prompt %q", want)
		}
	}
}
