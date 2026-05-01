// Package version exposes build-time metadata about the running binary.
//
// During release builds, goreleaser injects values via -X ldflags. For
// non-release local builds (`go build ./cmd/faultline`) the defaults
// below are used, which is enough to identify a development binary
// without requiring any extra build flags.
package version

import (
	"runtime/debug"
	"strings"
)

var (
	// Version is the semver tag this binary was built from, e.g. "v1.2.3".
	// "dev" indicates a local non-release build.
	Version = "dev"

	// Commit is the short git commit SHA the binary was built from. Set
	// by goreleaser; falls back to runtime/debug.BuildInfo's vcs.revision
	// for non-release builds (which Go records automatically when there
	// is no -ldflags override).
	Commit = ""

	// BuildTime is the RFC3339 timestamp of the build. Set by goreleaser;
	// empty for non-release builds.
	BuildTime = ""
)

// String returns a human-readable one-line summary suitable for logging
// or printing under -version. Output looks like:
//
//	v1.2.3 (abc1234) built 2026-05-01T10:23:45Z
//	dev (abc1234)
//	dev
//
// Only fields with non-empty values are included.
func String() string {
	commit := Commit
	if commit == "" {
		commit = vcsRevision()
	}

	var b strings.Builder
	b.WriteString(Version)
	if commit != "" {
		b.WriteString(" (")
		b.WriteString(shorten(commit))
		b.WriteString(")")
	}
	if BuildTime != "" {
		b.WriteString(" built ")
		b.WriteString(BuildTime)
	}
	return b.String()
}

// vcsRevision pulls the commit SHA from the embedded BuildInfo. Returns
// empty when running outside a git checkout or under a Go version that
// doesn't record VCS info.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}

// shorten trims a full SHA to the conventional 7-character prefix. Any
// shorter input is returned unchanged.
func shorten(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
