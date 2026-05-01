package tools

import (
	"strings"
	"testing"
)

func TestSplitIntoUnits_Empty(t *testing.T) {
	cases := []string{"", "   ", "\n\n", "\t \n  \n"}
	for _, c := range cases {
		if got := splitIntoUnits(c); got != nil {
			t.Errorf("splitIntoUnits(%q) = %v, want nil", c, got)
		}
	}
}

func TestSplitIntoUnits_Single(t *testing.T) {
	got := splitIntoUnits("just one paragraph, no blank lines")
	if len(got) != 1 {
		t.Fatalf("want 1 unit, got %d", len(got))
	}
	if got[0] != "just one paragraph, no blank lines" {
		t.Errorf("unit content: %q", got[0])
	}
}

func TestSplitIntoUnits_Multiple(t *testing.T) {
	body := "first paragraph here\n\nsecond paragraph here\n\nthird"
	got := splitIntoUnits(body)
	if len(got) != 3 {
		t.Fatalf("want 3 units, got %d: %v", len(got), got)
	}
	for i, want := range []string{"first paragraph here", "second paragraph here", "third"} {
		if got[i] != want {
			t.Errorf("unit[%d] = %q, want %q", i, got[i], want)
		}
	}
}

func TestSplitIntoUnits_AdjacentBlankLines(t *testing.T) {
	body := "alpha\n\n\n\nbeta"
	got := splitIntoUnits(body)
	if len(got) != 2 {
		t.Fatalf("multiple blank lines should collapse to one separator; got %d units: %v", len(got), got)
	}
}

func TestSplitIntoUnits_BlankLinesWithWhitespace(t *testing.T) {
	body := "alpha\n   \nbeta\n\t\ngamma"
	got := splitIntoUnits(body)
	if len(got) != 3 {
		t.Fatalf("blank lines containing whitespace should still separate; got %d units: %v", len(got), got)
	}
}

func TestSplitIntoUnits_CRLF(t *testing.T) {
	body := "line one\r\n\r\nline two\r\n\r\nline three"
	got := splitIntoUnits(body)
	if len(got) != 3 {
		t.Fatalf("CRLF should normalize and split; got %d units: %v", len(got), got)
	}
	for i, u := range got {
		if strings.Contains(u, "\r") {
			t.Errorf("unit[%d] still contains CR: %q", i, u)
		}
	}
}

func TestSplitIntoUnits_LeadingTrailingWhitespace(t *testing.T) {
	body := "\n\n\nalpha\n\nbeta\n\n\n"
	got := splitIntoUnits(body)
	if len(got) != 2 {
		t.Fatalf("leading/trailing blank lines should be ignored; got %d: %v", len(got), got)
	}
}

func TestSplitIntoUnits_OversizedParagraph(t *testing.T) {
	huge := strings.Repeat("x", maxParagraphBytes*2+500)
	body := "small first\n\n" + huge + "\n\nsmall last"
	got := splitIntoUnits(body)
	// Expect: small first | huge[0:3000] | huge[3000:6000] | huge[6000:6500] | small last
	if len(got) != 5 {
		t.Fatalf("oversized paragraph should byte-cut into 3 pieces; got %d units: %v", len(got), len(got))
	}
	if got[0] != "small first" || got[len(got)-1] != "small last" {
		t.Errorf("non-oversized neighbors should be preserved: first=%q last=%q", got[0], got[len(got)-1])
	}
	for i := 1; i <= 3; i++ {
		if len(got[i]) > maxParagraphBytes {
			t.Errorf("byte-cut unit[%d] exceeds cap: %d bytes", i, len(got[i]))
		}
	}
	// Byte-cut pieces should reassemble to the original.
	rejoined := got[1] + got[2] + got[3]
	if rejoined != huge {
		t.Errorf("byte-cut pieces don't reassemble (got %d bytes, want %d)", len(rejoined), len(huge))
	}
}

func TestSplitIntoUnits_PreservesIndentation(t *testing.T) {
	// Markdown indentation (lists, code blocks) is meaningful;
	// splitIntoUnits trims trailing whitespace per paragraph but
	// must preserve leading indentation.
	body := "  - bullet one\n  - bullet two\n\nplain paragraph"
	got := splitIntoUnits(body)
	if len(got) != 2 {
		t.Fatalf("got %d units: %v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "  - bullet one") {
		t.Errorf("indentation lost: %q", got[0])
	}
}

func TestUnitKey_Single(t *testing.T) {
	if got := unitKey("notes/foo.md", 0, 1); got != "notes/foo.md" {
		t.Errorf("single-unit file should use bare path; got %q", got)
	}
}

func TestUnitKey_Multi(t *testing.T) {
	if got := unitKey("notes/foo.md", 0, 5); got != "notes/foo.md#0" {
		t.Errorf("multi-unit key: %q", got)
	}
	if got := unitKey("notes/foo.md", 4, 5); got != "notes/foo.md#4" {
		t.Errorf("multi-unit last key: %q", got)
	}
}

func TestPathFromUnitKey(t *testing.T) {
	cases := []struct{ key, want string }{
		{"notes/foo.md", "notes/foo.md"},
		{"notes/foo.md#0", "notes/foo.md"},
		{"notes/foo.md#42", "notes/foo.md"},
		{"a/b#3", "a/b"},
		// Defensive: trailing # alone or non-numeric suffix isn't a chunk separator.
		{"oddpath#", "oddpath#"},
		{"oddpath#abc", "oddpath#abc"},
	}
	for _, c := range cases {
		if got := pathFromUnitKey(c.key); got != c.want {
			t.Errorf("pathFromUnitKey(%q) = %q, want %q", c.key, got, c.want)
		}
	}
}
