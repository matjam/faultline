package main

import (
	"strings"
	"testing"
	"time"
)

func TestBuildCycleContext_NoMemories(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 30, 0, 0, time.UTC)
	got := BuildCycleContext("SYSTEM PROMPT", nil, now, 2000)

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
	got := BuildCycleContext("SYS", mems, now, 2000)

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
	got := BuildCycleContext("SYS", mems, time.Now(), 2000)

	if !strings.Contains(got, "[truncated") {
		t.Error("expected truncation marker for long memory")
	}
	// Body should not contain the full 3000 x's
	if strings.Count(got, "x") >= 3000 {
		t.Errorf("memory was not truncated; got %d x's", strings.Count(got, "x"))
	}
	// Hint must mention the tool the agent should call to read the rest.
	if !strings.Contains(got, "memory_read") {
		t.Error("expected truncation hint to reference memory_read")
	}
	// Hint must mention the file path so the agent doesn't have to guess.
	if !strings.Contains(got, "long.md") {
		t.Error("expected truncation hint to reference the file path")
	}
}

func TestBuildCycleContext_NoLimitKeepsFullContent(t *testing.T) {
	long := strings.Repeat("x", 3000)
	mems := []SearchResult{{Path: "long.md", Content: long}}
	got := BuildCycleContext("SYS", mems, time.Now(), 0)

	if strings.Contains(got, "[truncated") {
		t.Error("did not expect truncation marker when limit is disabled")
	}
	if strings.Count(got, "x") < 3000 {
		t.Errorf("expected full 3000 x's when limit disabled; got %d", strings.Count(got, "x"))
	}
}
