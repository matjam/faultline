package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLogger returns a logger writing to a buffer so tests can both
// silence log spam and assert on log contents. Distinct from quietLogger
// in kobold_test.go (which discards output and returns no buffer).
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, buf
}

func sampleMessages() []Message {
	return []Message{
		{Role: RoleSystem, Content: "you are an agent"},
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi"},
	}
}

func TestSaveAndLoadState_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	msgs := sampleMessages()
	if err := SaveState(path, msgs, 7); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	logger, _ := captureLogger()
	got, idle, err := LoadState(path, logger)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if idle != 7 {
		t.Errorf("idle streak: got %d want 7", idle)
	}
	if len(got) != len(msgs) {
		t.Fatalf("messages len: got %d want %d", len(got), len(msgs))
	}
	for i := range got {
		if got[i].Role != msgs[i].Role || got[i].Content != msgs[i].Content {
			t.Errorf("message %d roundtrip mismatch: got %+v want %+v", i, got[i], msgs[i])
		}
	}
}

func TestSaveState_EmptyPathIsNoOp(t *testing.T) {
	if err := SaveState("", sampleMessages(), 0); err != nil {
		t.Errorf("SaveState with empty path should be no-op, got %v", err)
	}
}

func TestLoadState_EmptyPathIsNoOp(t *testing.T) {
	logger, _ := captureLogger()
	got, idle, err := LoadState("", logger)
	if err != nil {
		t.Errorf("LoadState empty path err = %v", err)
	}
	if got != nil || idle != 0 {
		t.Errorf("LoadState empty path: got messages=%v idle=%d", got, idle)
	}
}

func TestLoadState_MissingFileIsFreshStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-such-file.json")

	logger, _ := captureLogger()
	got, idle, err := LoadState(path, logger)
	if err != nil {
		t.Errorf("LoadState missing file should not error, got %v", err)
	}
	if got != nil || idle != 0 {
		t.Errorf("missing file: got messages=%v idle=%d", got, idle)
	}
}

func TestLoadState_BadJSONIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	logger, logBuf := captureLogger()
	got, _, err := LoadState(path, logger)
	if err != nil {
		t.Fatalf("expected no error for bad file (quarantined), got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil messages on quarantine, got %v", got)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("expected original path to be moved aside, still exists: %v", statErr)
	}
	matches, _ := filepath.Glob(path + ".bad-*")
	if len(matches) == 0 {
		t.Errorf("expected a quarantine sibling file, none found in %s", dir)
	}
	if !strings.Contains(logBuf.String(), "quarantined") {
		t.Errorf("expected quarantine log message, got: %s", logBuf.String())
	}
}

func TestLoadState_VersionMismatchIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	bogus := persistedState{
		Version:  stateFileVersion + 99,
		Messages: sampleMessages(),
	}
	data, _ := json.Marshal(bogus)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	logger, logBuf := captureLogger()
	got, _, err := LoadState(path, logger)
	if err != nil {
		t.Fatalf("LoadState err = %v", err)
	}
	if got != nil {
		t.Errorf("expected nil messages on version mismatch, got %v", got)
	}
	if !strings.Contains(logBuf.String(), "version mismatch") {
		t.Errorf("expected version mismatch reason in log, got: %s", logBuf.String())
	}
}

func TestSaveState_AtomicReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := SaveState(path, sampleMessages(), 1); err != nil {
		t.Fatal(err)
	}
	updated := append(sampleMessages(), Message{
		Role: RoleUser, Content: "second turn",
	})
	if err := SaveState(path, updated, 2); err != nil {
		t.Fatal(err)
	}

	logger, _ := captureLogger()
	got, idle, err := LoadState(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(updated) {
		t.Errorf("expected %d messages after overwrite, got %d", len(updated), len(got))
	}
	if idle != 2 {
		t.Errorf("expected idle=2 after overwrite, got %d", idle)
	}

	// No leftover .tmp files in the directory.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

func TestSanitizeMessages_DropsTrailingUnsatisfiedToolCalls(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "hi"},
		{
			Role: RoleAssistant,
			ToolCalls: []ToolCall{
				{ID: "call_1", Type: ToolTypeFunction, Function: FunctionCall{Name: "x"}},
				{ID: "call_2", Type: ToolTypeFunction, Function: FunctionCall{Name: "y"}},
			},
		},
		{Role: RoleTool, ToolCallID: "call_1", Content: "result1"},
		// call_2 never got a tool response -- crash mid-dispatch.
	}
	got := sanitizeMessages(msgs)
	if len(got) != 2 {
		t.Errorf("expected truncation to 2 messages, got %d: %+v", len(got), got)
	}
}

func TestSanitizeMessages_KeepsCompleteToolCallTurns(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "sys"},
		{
			Role: RoleAssistant,
			ToolCalls: []ToolCall{
				{ID: "call_1", Type: ToolTypeFunction, Function: FunctionCall{Name: "x"}},
			},
		},
		{Role: RoleTool, ToolCallID: "call_1", Content: "result"},
		{Role: RoleAssistant, Content: "thanks"},
	}
	got := sanitizeMessages(msgs)
	if len(got) != len(msgs) {
		t.Errorf("complete log should be untouched, got %d/%d", len(got), len(msgs))
	}
}

func TestSanitizeMessages_NoToolCallsIsNoOp(t *testing.T) {
	msgs := sampleMessages()
	got := sanitizeMessages(msgs)
	if len(got) != len(msgs) {
		t.Errorf("plain messages should be untouched, got %d/%d", len(got), len(msgs))
	}
}

func TestSanitizeMessages_EmptyIsNoOp(t *testing.T) {
	got := sanitizeMessages(nil)
	if got != nil {
		t.Errorf("nil input should return nil, got %v", got)
	}
}
