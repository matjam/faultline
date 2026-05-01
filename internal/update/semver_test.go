package update

import "testing"

func TestIsNewer(t *testing.T) {
	cases := []struct {
		candidate string
		current   string
		want      bool
	}{
		{"v1.2.3", "v1.2.0", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.0", "v1.2.3", false},
		{"v2.0.0", "v1.99.99", true},
		// no v prefix tolerated
		{"1.2.3", "1.2.0", true},
		{"1.2.0", "v1.2.3", false},
		// dev / non-semver current means anything wins
		{"v1.0.0", "dev", true},
		{"v0.1.0", "dev", true},
		// dev candidate is never newer
		{"dev", "v1.0.0", false},
		{"dev", "dev", false},
		// prerelease compares correctly
		{"v1.0.0", "v1.0.0-rc.1", true},
		{"v1.0.0-rc.2", "v1.0.0-rc.1", true},
	}
	for _, tc := range cases {
		got := IsNewer(tc.candidate, tc.current)
		if got != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v",
				tc.candidate, tc.current, got, tc.want)
		}
	}
}

func TestIsPrerelease(t *testing.T) {
	cases := []struct {
		tag  string
		want bool
	}{
		{"v1.0.0", false},
		{"v1.0.0-rc.1", true},
		{"v1.0.0-alpha", true},
		{"v1.0.0-beta.3", true},
		{"1.0.0", false},
		{"1.0.0-rc.1", true},
	}
	for _, tc := range cases {
		got := IsPrerelease(tc.tag)
		if got != tc.want {
			t.Errorf("IsPrerelease(%q) = %v, want %v", tc.tag, got, tc.want)
		}
	}
}
