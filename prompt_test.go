package main

import (
	"strings"
	"testing"
	"time"
)

func TestRenderPrompt(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 30, 0, 0, time.UTC)
	tpl := "Hello, the time is {{TIME}}. Goodbye."
	got := RenderPrompt(tpl, now)
	want := "Hello, the time is " + now.Format(time.RFC1123) + ". Goodbye."
	if got != want {
		t.Errorf("RenderPrompt = %q, want %q", got, want)
	}
}

func TestRenderPrompt_NoPlaceholder(t *testing.T) {
	tpl := "no placeholders here"
	if got := RenderPrompt(tpl, time.Now()); got != tpl {
		t.Errorf("expected unchanged template, got %q", got)
	}
}

func TestRenderPrompt_MultipleOccurrences(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tpl := "{{TIME}} - {{TIME}}"
	got := RenderPrompt(tpl, now)
	stamp := now.Format(time.RFC1123)
	want := stamp + " - " + stamp
	if got != want {
		t.Errorf("RenderPrompt = %q, want %q", got, want)
	}
}

func TestBuildCycleContext_NoMemories(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 30, 0, 0, time.UTC)
	got := BuildCycleContext("SYSTEM PROMPT", nil, now)

	if !strings.Contains(got, "SYSTEM PROMPT") {
		t.Error("output missing system prompt")
	}
	if !strings.Contains(got, now.Format(time.RFC1123)) {
		t.Error("output missing current time")
	}
	if strings.Contains(got, "Recent Memories") {
		t.Error("output should not have Recent Memories section when no memories provided")
	}
}

func TestBuildCycleContext_WithMemories(t *testing.T) {
	now := time.Now()
	mems := []SearchResult{
		{Path: "alpha.md", Content: "alpha content"},
		{Path: "beta.md", Content: "beta content"},
	}
	got := BuildCycleContext("SYS", mems, now)

	if !strings.Contains(got, "Recent Memories") {
		t.Error("missing Recent Memories header")
	}
	if !strings.Contains(got, "### alpha.md") {
		t.Error("missing alpha header")
	}
	if !strings.Contains(got, "### beta.md") {
		t.Error("missing beta header")
	}
	if !strings.Contains(got, "alpha content") {
		t.Error("missing alpha body")
	}
}

func TestBuildCycleContext_TruncatesLongMemory(t *testing.T) {
	long := strings.Repeat("x", 3000)
	mems := []SearchResult{{Path: "long.md", Content: long}}
	got := BuildCycleContext("SYS", mems, time.Now())

	if !strings.Contains(got, "*[truncated]*") {
		t.Error("expected truncation marker for long memory")
	}
	// Body should not contain the full 3000 x's
	if strings.Count(got, "x") >= 3000 {
		t.Errorf("memory was not truncated; got %d x's", strings.Count(got, "x"))
	}
}

func TestLoadPrompt_SeedsDefault(t *testing.T) {
	m := newTestMemory(t)

	got, err := LoadPrompt(m, "system")
	if err != nil {
		t.Fatalf("LoadPrompt: %v", err)
	}
	if got != defaultSystemPrompt {
		t.Error("LoadPrompt should return embedded default on first load")
	}

	// File should now exist on disk
	stored, err := m.Read("prompts/system.md")
	if err != nil {
		t.Fatalf("expected seeded file on disk: %v", err)
	}
	if stored != defaultSystemPrompt {
		t.Error("seeded file content does not match embedded default")
	}
}

func TestLoadPrompt_PreservesUserEdits(t *testing.T) {
	m := newTestMemory(t)
	custom := "MY CUSTOM SYSTEM PROMPT"
	if err := m.Write("prompts/system.md", custom); err != nil {
		t.Fatal(err)
	}

	got, err := LoadPrompt(m, "system")
	if err != nil {
		t.Fatal(err)
	}
	if got != custom {
		t.Errorf("LoadPrompt returned %q, want %q (should not overwrite existing)", got, custom)
	}
}

func TestLoadPrompt_UnknownName(t *testing.T) {
	m := newTestMemory(t)
	if _, err := LoadPrompt(m, "no-such-prompt"); err == nil {
		t.Error("expected error for unknown prompt name")
	}
}

func TestLoadAllPrompts(t *testing.T) {
	m := newTestMemory(t)
	prompts, err := LoadAllPrompts(m)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"system", "compaction", "cycle_start", "continue", "shutdown"} {
		if _, ok := prompts[want]; !ok {
			t.Errorf("LoadAllPrompts missing prompt %q", want)
		}
	}
}
