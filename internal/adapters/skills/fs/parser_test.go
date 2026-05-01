package fs

import (
	"strings"
	"testing"
)

func TestSplitFrontmatter_Basic(t *testing.T) {
	input := []byte("---\nname: foo\ndescription: bar\n---\n\n# Body\n\nHello.\n")
	fm, body, ok := splitFrontmatter(input)
	if !ok {
		t.Fatal("splitFrontmatter returned ok=false")
	}
	if !strings.Contains(string(fm), "name: foo") {
		t.Errorf("frontmatter missing name: got %q", fm)
	}
	if !strings.HasPrefix(body, "# Body") {
		t.Errorf("body unexpected: got %q", body)
	}
}

func TestSplitFrontmatter_CRLF(t *testing.T) {
	input := []byte("---\r\nname: foo\r\ndescription: bar\r\n---\r\nbody\r\n")
	_, body, ok := splitFrontmatter(input)
	if !ok {
		t.Fatal("splitFrontmatter returned ok=false on CRLF input")
	}
	if !strings.HasPrefix(body, "body") {
		t.Errorf("body unexpected: got %q", body)
	}
}

func TestSplitFrontmatter_BOM(t *testing.T) {
	input := []byte("\xef\xbb\xbf---\nname: foo\ndescription: bar\n---\nbody\n")
	_, _, ok := splitFrontmatter(input)
	if !ok {
		t.Fatal("splitFrontmatter returned ok=false on BOM input")
	}
}

func TestSplitFrontmatter_NoFrontmatter(t *testing.T) {
	input := []byte("# Just markdown\n\nNo frontmatter here.\n")
	_, _, ok := splitFrontmatter(input)
	if ok {
		t.Error("splitFrontmatter returned ok=true for missing frontmatter")
	}
}

func TestSplitFrontmatter_UnclosedFence(t *testing.T) {
	input := []byte("---\nname: foo\ndescription: bar\nthis never closes\n")
	_, _, ok := splitFrontmatter(input)
	if ok {
		t.Error("splitFrontmatter returned ok=true for unclosed frontmatter")
	}
}

func TestParseFrontmatter_Strict(t *testing.T) {
	input := []byte("name: foo\ndescription: bar baz\nlicense: MIT\n")
	raw, err := parseFrontmatter(input)
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}
	if raw.Name != "foo" || raw.Description != "bar baz" || raw.License != "MIT" {
		t.Errorf("unexpected raw: %+v", raw)
	}
}

func TestParseFrontmatter_LenientRetry_UnquotedColonInValue(t *testing.T) {
	// The spec calls out this exact authoring mistake. Strict YAML
	// parse fails because "Use this skill when:" looks like a nested
	// key-value pair. Our lenient retry quotes it.
	input := []byte("name: pdf\ndescription: Use this skill when: the user asks about PDFs\n")
	raw, err := parseFrontmatter(input)
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}
	if !strings.HasPrefix(raw.Description, "Use this skill when:") {
		t.Errorf("description not preserved through lenient retry: %q", raw.Description)
	}
}

func TestParseFrontmatter_TotallyBroken(t *testing.T) {
	// Unbalanced brackets should fail even with lenient retry.
	input := []byte("name: [foo\n  description: bar\n")
	if _, err := parseFrontmatter(input); err == nil {
		t.Error("parseFrontmatter accepted unparseable YAML")
	}
}
