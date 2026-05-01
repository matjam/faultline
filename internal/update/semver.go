package update

import (
	"strings"

	"golang.org/x/mod/semver"
)

// IsNewer reports whether candidate (e.g. "v1.2.3") is strictly newer
// than current (e.g. "v1.2.0"). Both inputs are normalized so a missing
// "v" prefix is tolerated. Non-semver inputs (e.g. the "dev" placeholder
// from a local build) are treated as the oldest possible version, so any
// real release tag wins -- this is the right behavior because we want
// dev builds to upgrade to real releases when self-update is enabled.
func IsNewer(candidate, current string) bool {
	c := normalize(candidate)
	cur := normalize(current)

	if !semver.IsValid(c) {
		return false
	}
	if !semver.IsValid(cur) {
		return true
	}
	return semver.Compare(c, cur) > 0
}

// IsPrerelease reports whether tag (e.g. "v1.2.3-rc.1") is a prerelease.
func IsPrerelease(tag string) bool {
	return semver.Prerelease(normalize(tag)) != ""
}

// normalize ensures a "v" prefix on tag-shaped strings so semver.* works.
// Empty input is returned as-is and validity-checked by callers.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "v") {
		return "v" + s
	}
	return s
}
