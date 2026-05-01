package fs

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// silentLogger returns a slog.Logger that discards everything. Used in
// tests so per-skill diagnostic logs don't pollute test output.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeSkill creates a skill directory under root with the given
// SKILL.md contents.
func writeSkill(t *testing.T, root, name, skillMD string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStore_DiscoversSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "pdf-processing", "---\nname: pdf-processing\ndescription: Handle PDFs.\n---\n\nBody here.\n")
	writeSkill(t, root, "data-analysis", "---\nname: data-analysis\ndescription: Analyze datasets.\n---\n")

	s, err := New(root, silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	skills := s.List()
	if len(skills) != 2 {
		t.Fatalf("got %d skills, want 2", len(skills))
	}
	if skills[0].Name != "data-analysis" || skills[1].Name != "pdf-processing" {
		t.Errorf("skills not sorted by name: %v", []string{skills[0].Name, skills[1].Name})
	}
}

func TestStore_SkipsMalformedSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "good", "---\nname: good\ndescription: Works.\n---\nbody\n")
	writeSkill(t, root, "no-desc", "---\nname: no-desc\n---\nbody\n")
	writeSkill(t, root, "no-fence", "no frontmatter at all\n")
	// dir without SKILL.md
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	// dotfile dir
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0755); err != nil {
		t.Fatal(err)
	}

	s, err := New(root, silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	skills := s.List()
	if len(skills) != 1 || skills[0].Name != "good" {
		t.Errorf("expected only 'good' to survive, got %v", skills)
	}
}

func TestStore_MissingRootIsNotError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	s, err := New(root, silentLogger())
	if err != nil {
		t.Fatalf("New on missing root: %v", err)
	}
	if got := s.List(); len(got) != 0 {
		t.Errorf("expected empty catalog, got %d skills", len(got))
	}
}

func TestStore_NameMismatchUsesDirectoryNameWithDiagnostic(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "pdf-processing", "---\nname: something-else\ndescription: PDFs.\n---\n")

	s, err := New(root, silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("pdf-processing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "pdf-processing" {
		t.Errorf("expected directory name to win, got %q", got.Name)
	}
	if len(got.Diagnostics) == 0 {
		t.Error("expected a diagnostic about name mismatch")
	}
}

func TestStore_GetNotFound(t *testing.T) {
	s, _ := New(t.TempDir(), silentLogger())
	if _, err := s.Get("nope"); err == nil {
		t.Error("Get(nope) returned nil error")
	}
}

func TestStore_ReadResource(t *testing.T) {
	root := t.TempDir()
	dir := writeSkill(t, root, "with-resources", "---\nname: with-resources\ndescription: Has scripts.\n---\n")
	scriptsDir := filepath.Join(dir, "scripts")
	_ = os.MkdirAll(scriptsDir, 0755)
	_ = os.WriteFile(filepath.Join(scriptsDir, "extract.py"), []byte("print('hi')\n"), 0644)

	s, _ := New(root, silentLogger())
	got, err := s.Read("with-resources", "scripts/extract.py")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "print('hi')\n" {
		t.Errorf("unexpected content: %q", got)
	}
}

func TestStore_ReadRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "my-skill", "---\nname: my-skill\ndescription: x.\n---\n")

	s, _ := New(root, silentLogger())
	for _, bad := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"scripts/../../escape.txt",
	} {
		if _, err := s.Read("my-skill", bad); err == nil {
			t.Errorf("Read(%q) accepted path escape", bad)
		}
	}
}

func TestStore_ResourcesEnumeration(t *testing.T) {
	root := t.TempDir()
	dir := writeSkill(t, root, "skill-x", "---\nname: skill-x\ndescription: x.\n---\n")
	for _, p := range []struct {
		path    string
		content string
	}{
		{"scripts/run.py", "x"},
		{"scripts/helper.py", "x"},
		{"references/README.md", "x"},
		{"assets/template.txt", "x"},
		{"scripts/.hidden.py", "x"}, // dotfile, should be excluded
		{"random.txt", "x"},         // outside conventional dirs, should be excluded
	} {
		full := filepath.Join(dir, p.path)
		_ = os.MkdirAll(filepath.Dir(full), 0755)
		_ = os.WriteFile(full, []byte(p.content), 0644)
	}

	s, _ := New(root, silentLogger())
	res, truncated, err := s.Resources("skill-x")
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	if truncated {
		t.Error("did not expect truncation for 4 entries")
	}
	want := []string{"assets/template.txt", "references/README.md", "scripts/helper.py", "scripts/run.py"}
	if len(res) != len(want) {
		t.Fatalf("got %d entries, want %d (%v)", len(res), len(want), res)
	}
	for i, w := range want {
		if res[i].Path != w {
			t.Errorf("entry %d: got %q want %q", i, res[i].Path, w)
		}
	}
}

func TestStore_Reload_PicksUpNewSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "first", "---\nname: first\ndescription: x.\n---\n")
	s, _ := New(root, silentLogger())
	if got := s.List(); len(got) != 1 {
		t.Fatalf("initial: got %d", len(got))
	}

	writeSkill(t, root, "second", "---\nname: second\ndescription: y.\n---\n")
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := s.List(); len(got) != 2 {
		t.Errorf("after reload: got %d", len(got))
	}
}
