package update

import (
	"runtime"
	"strings"
	"testing"
)

func TestAssetName_MatchesGoreleaserConvention(t *testing.T) {
	got := AssetName("1.2.3")
	if !strings.HasPrefix(got, "faultline_1.2.3_") {
		t.Errorf("missing version prefix in %q", got)
	}
	if !strings.HasSuffix(got, ".tar.gz") {
		t.Errorf("missing extension in %q", got)
	}
	if !strings.Contains(got, runtime.GOOS) {
		t.Errorf("%q missing os %q", got, runtime.GOOS)
	}
}

func TestArchLabel(t *testing.T) {
	cases := map[string]string{
		"amd64": "x86_64",
		"arm64": "arm64",
		"arm":   "arm",
		"386":   "386",
	}
	for in, want := range cases {
		if got := archLabel(in); got != want {
			t.Errorf("archLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
