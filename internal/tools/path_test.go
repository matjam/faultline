package tools

import (
	"strings"
	"testing"
)

func TestValidateMemoryPath_AcceptsLowercaseDashesDots(t *testing.T) {
	cases := []string{
		"a.md",
		"meta/long-term-memory.md",
		"dreams/2026-04-30.md",
		"notes/sub/deep-file.md",
		"abc-123.md",
	}
	for _, p := range cases {
		if err := validateMemoryPath(p); err != nil {
			t.Errorf("%q should be valid, got error: %v", p, err)
		}
	}
}

func TestValidateMemoryPath_RejectsUnderscores(t *testing.T) {
	err := validateMemoryPath("notes/long_term_memory.md")
	if err == nil {
		t.Fatal("underscore should be rejected")
	}
	if !strings.Contains(err.Error(), "underscore") {
		t.Errorf("error should mention underscores, got: %v", err)
	}
	if !strings.Contains(err.Error(), "dashes") {
		t.Errorf("error should suggest dashes, got: %v", err)
	}
}

func TestValidateMemoryPath_RejectsSpaces(t *testing.T) {
	if err := validateMemoryPath("notes/my file.md"); err == nil {
		t.Error("spaces should be rejected")
	}
}

func TestValidateMemoryPath_RejectsUppercase(t *testing.T) {
	if err := validateMemoryPath("Notes/file.md"); err == nil {
		t.Error("uppercase directory should be rejected")
	}
	if err := validateMemoryPath("notes/MyFile.md"); err == nil {
		t.Error("uppercase filename should be rejected")
	}
}

func TestValidateMemoryPath_RejectsOtherPunctuation(t *testing.T) {
	if err := validateMemoryPath("notes/file with @special!.md"); err == nil {
		t.Error("special chars should be rejected")
	}
}
