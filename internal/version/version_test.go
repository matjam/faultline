package version

import (
	"strings"
	"testing"
)

func TestString_VersionOnly(t *testing.T) {
	defer restore(Version, Commit, BuildTime)
	Version = "dev"
	Commit = ""
	BuildTime = ""

	got := String()
	// vcsRevision may pick up a commit from build info during `go test`,
	// so allow either "dev" or "dev (somehash)".
	if !strings.HasPrefix(got, "dev") {
		t.Errorf("expected output to start with 'dev', got %q", got)
	}
}

func TestString_AllFields(t *testing.T) {
	defer restore(Version, Commit, BuildTime)
	Version = "v1.2.3"
	Commit = "abcdef1234567890"
	BuildTime = "2026-05-01T10:23:45Z"

	got := String()
	want := "v1.2.3 (abcdef1) built 2026-05-01T10:23:45Z"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestString_ShortCommitNotShortened(t *testing.T) {
	defer restore(Version, Commit, BuildTime)
	Version = "v1.0.0"
	Commit = "abc"
	BuildTime = ""

	got := String()
	want := "v1.0.0 (abc)"
	if got != want {
		t.Errorf("String() = %q, want %q (short commit should pass through)", got, want)
	}
}

// restore resets the package-level version vars. Used as
// `defer restore(Version, Commit, BuildTime)` at the top of each test;
// defer evaluates the arguments at defer time so the originals are
// captured before the test mutates them.
func restore(v, c, b string) {
	Version = v
	Commit = c
	BuildTime = b
}
