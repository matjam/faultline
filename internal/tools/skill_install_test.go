package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	skillsfs "github.com/matjam/faultline/internal/adapters/skills/fs"
)

// silentTestLogger discards all log output so tests don't pollute the run.
func silentTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDetectSourceKind(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		override string
		wantKind string
		wantErr  bool
	}{
		{"tar.gz", "https://example.com/skill.tar.gz", "", "tarball", false},
		{"tgz", "https://example.com/skill.tgz", "", "tarball", false},
		{"plain tar", "https://example.com/skill.tar", "", "tarball", false},
		{"zip", "https://example.com/skill.zip", "", "zip", false},
		{".git suffix", "https://github.com/owner/repo.git", "", "git", false},
		{"git@", "git@github.com:owner/repo.git", "", "git", false},
		{"git+https", "git+https://github.com/owner/repo.git", "", "git", false},
		{"plain github repo", "https://github.com/owner/repo", "", "git", false},
		{"plain github with trailing slash", "https://github.com/owner/repo/", "", "git", false},
		{"github tree url not normalised", "https://github.com/owner/repo/tree/main", "", "", true},

		{"override tarball", "https://example.com/anything", "tarball", "tarball", false},
		{"override zip", "https://example.com/anything", "zip", "zip", false},
		{"override git", "https://example.com/anything", "git", "git", false},
		{"override unknown", "https://example.com/anything", "what", "", true},

		{"unrecognized", "https://example.com/no-extension", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, _, err := detectSourceKind(c.source, c.override)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got kind=%q", c.source, kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if kind != c.wantKind {
				t.Errorf("kind=%q want %q", kind, c.wantKind)
			}
		})
	}
}

func TestInferSkillName(t *testing.T) {
	cases := []struct {
		source  string
		subpath string
		want    string
	}{
		{"https://example.com/skill.tar.gz", "", "skill"},
		{"https://example.com/pdf-processing.tgz", "", "pdf-processing"},
		{"https://github.com/owner/repo.git", "", "repo"},
		{"https://github.com/owner/repo", "", "repo"},
		{"https://example.com/dir/some_skill.tar.gz", "", "some-skill"},
		// subpath wins
		{"https://github.com/owner/monorepo.git", "skills/pdf-processing", "pdf-processing"},
	}
	for _, c := range cases {
		got := inferSkillName(c.source, c.subpath)
		if got != c.want {
			t.Errorf("inferSkillName(%q, %q) = %q, want %q", c.source, c.subpath, got, c.want)
		}
	}
}

func TestSanitiseName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"PDF Processing", "pdf-processing"},
		{"some_skill", "some-skill"},
		{"alpha--beta", "alpha-beta"},
		{"-leading", "leading"},
		{"trailing-", "trailing"},
		{"good.name", "good-name"},
	}
	for _, c := range cases {
		got := sanitiseName(c.in)
		if got != c.want {
			t.Errorf("sanitiseName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSafeJoin(t *testing.T) {
	parent := "/tmp/parent"
	good, err := safeJoin(parent, "subdir/file.txt")
	if err != nil || !strings.HasPrefix(good, parent) {
		t.Errorf("safeJoin(good) failed: %v %q", err, good)
	}

	for _, bad := range []string{
		"../escape.txt",
		"subdir/../../etc/passwd",
		"/etc/passwd",
	} {
		if _, err := safeJoin(parent, bad); err == nil {
			t.Errorf("safeJoin(%q) accepted path escape", bad)
		}
	}
}

// buildTarGz constructs a .tar.gz byte slice in memory containing the
// supplied file map. Used by TestSkillInstall_Tarball below to feed a
// fake httptest server without any disk I/O.
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestSkillInstall_Tarball is the end-to-end happy path: serve a
// minimal tar.gz over httptest, drive skill_install, expect the skill
// to land in the configured root and be visible via the catalog.
func TestSkillInstall_Tarball(t *testing.T) {
	skillsRoot := t.TempDir()

	// GitHub-archive convention: contents wrapped in a single
	// top-level directory. resolveSkillRoot should unwrap it.
	tarData := buildTarGz(t, map[string]string{
		"my-skill-main/SKILL.md":          "---\nname: my-skill\ndescription: A test skill.\n---\n\nBody.\n",
		"my-skill-main/scripts/run.py":    "print('ok')\n",
		"my-skill-main/references/foo.md": "ref\n",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tarData)
	}))
	defer srv.Close()

	// Construct a real Store + Executor against the temp skills root.
	store, err := skillsfs.New(skillsRoot, silentTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	te := New(Deps{
		Skills:              store,
		SkillInstallEnabled: true,
		Logger:              silentTestLogger(),
	})

	args := `{"source":"` + srv.URL + `/my-skill.tar.gz","name":"my-skill"}`
	got := te.skillInstall(context.Background(), args)
	if !strings.Contains(got, "Installed skill") {
		t.Fatalf("install failed: %s", got)
	}

	// The skill should now exist on disk and in the catalog.
	if _, err := os.Stat(filepath.Join(skillsRoot, "my-skill", "SKILL.md")); err != nil {
		t.Errorf("SKILL.md not on disk: %v", err)
	}
	if _, err := store.Get("my-skill"); err != nil {
		t.Errorf("skill not in catalog after install: %v", err)
	}
}

func TestSkillInstall_RejectsExisting(t *testing.T) {
	skillsRoot := t.TempDir()
	_ = os.MkdirAll(filepath.Join(skillsRoot, "duplicate"), 0o755)
	_ = os.WriteFile(filepath.Join(skillsRoot, "duplicate", "SKILL.md"),
		[]byte("---\nname: duplicate\ndescription: x.\n---\n"), 0o644)

	store, _ := skillsfs.New(skillsRoot, silentTestLogger())
	te := New(Deps{
		Skills:              store,
		SkillInstallEnabled: true,
		Logger:              silentTestLogger(),
	})

	args := `{"source":"https://example.com/duplicate.tar.gz","name":"duplicate"}`
	got := te.skillInstall(context.Background(), args)
	if !strings.Contains(got, "already exists") {
		t.Errorf("expected refusal on existing skill, got: %s", got)
	}
}

func TestSkillInstall_DisabledByDefault(t *testing.T) {
	skillsRoot := t.TempDir()
	store, _ := skillsfs.New(skillsRoot, silentTestLogger())
	// install_enabled = false
	te := New(Deps{
		Skills:              store,
		SkillInstallEnabled: false,
		Logger:              silentTestLogger(),
	})

	args := `{"source":"https://example.com/x.tar.gz"}`
	got := te.skillInstall(context.Background(), args)
	if !strings.Contains(got, "disabled") {
		t.Errorf("expected disabled error, got: %s", got)
	}

	// Tool def should not be advertised.
	for _, def := range te.skillToolDefs() {
		if def.Function != nil && def.Function.Name == "skill_install" {
			t.Errorf("skill_install advertised despite install_enabled=false")
		}
	}
}

func TestSkillInstall_RejectsInvalidName(t *testing.T) {
	skillsRoot := t.TempDir()
	store, _ := skillsfs.New(skillsRoot, silentTestLogger())
	te := New(Deps{
		Skills:              store,
		SkillInstallEnabled: true,
		Logger:              silentTestLogger(),
	})

	// "Bad_Name" sanitizes to "bad-name" via inferSkillName, which is
	// fine -- we use an explicit name with capitals to force the
	// validation failure.
	args := `{"source":"https://example.com/x.tar.gz","name":"BadName"}`
	got := te.skillInstall(context.Background(), args)
	if !strings.Contains(got, "invalid") {
		t.Errorf("expected invalid-name error, got: %s", got)
	}
}
