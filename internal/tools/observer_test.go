package tools

import (
	"strings"
	"testing"
)

func TestSummarizeToolField(t *testing.T) {
	cases := []struct {
		in     string
		maxLen int
		want   string
	}{
		{"hello world", 100, "hello world"},
		{"line1\nline2\nline3", 100, "line1 line2 line3"},
		{"  multi   space  ", 100, "multi space"},
		{strings.Repeat("a", 30), 10, "aaaaaaaaaa…"},
		{"", 100, ""},
	}
	for _, tc := range cases {
		got := summarizeToolField(tc.in, tc.maxLen)
		if got != tc.want {
			t.Errorf("summarizeToolField(%q, %d) = %q, want %q",
				tc.in, tc.maxLen, got, tc.want)
		}
	}
}

func TestSummarizeToolField_RuneSafe(t *testing.T) {
	// Ensure UTF-8 multi-byte sequences aren't split mid-codepoint
	// when the byte budget would otherwise land in the middle.
	emoji := strings.Repeat("⚡", 50) // 3 bytes per glyph
	got := summarizeToolField(emoji, 5)
	// Want exactly 5 lightning bolts plus the ellipsis.
	want := strings.Repeat("⚡", 5) + "…"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestClassifyToolError(t *testing.T) {
	cases := []struct {
		in       string
		hasError bool
	}{
		{"Error: file not found", true},
		{"Failed: network timeout", true},
		{"Tool \"foo\" is not available to subagents.", true},
		{"OK", false},
		{"42 lines written", false},
		{"", false},
	}
	for _, tc := range cases {
		got := classifyToolError(tc.in)
		if (got != "") != tc.hasError {
			t.Errorf("classifyToolError(%q) = %q, hasError=%v want %v",
				tc.in, got, got != "", tc.hasError)
		}
	}
}
