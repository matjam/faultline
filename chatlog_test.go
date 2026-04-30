package main

import (
	"strings"
	"testing"
)

// TestChatLogger_FormatPreservesPriorityFields exercises formatMessage
// directly. The whole point of the chat log is that prose, code, and
// `<think>` reasoning blocks come through verbatim with newlines intact --
// unlike the slog debug log where they're escaped onto a single line.
func TestChatLogger_FormatPreservesNewlinesAndToolCalls(t *testing.T) {
	msg := Message{
		Role:    RoleAssistant,
		Content: "<think>\nLet me check memory.\n</think>\n\nReading the file now.",
		ToolCalls: []ToolCall{
			{
				ID:       "call_abc",
				Type:     ToolTypeFunction,
				Function: FunctionCall{Name: "memory_read", Arguments: `{"path":"notes"}`},
			},
			{
				ID:       "call_def",
				Type:     ToolTypeFunction,
				Function: FunctionCall{Name: "get_time", Arguments: `{}`},
			},
		},
	}

	var sb strings.Builder
	formatMessage(&sb, msg, "tool_calls")
	out := sb.String()

	if !strings.Contains(out, "| assistant | tool_calls") {
		t.Errorf("expected role+finish in banner, got:\n%s", out)
	}
	if !strings.Contains(out, "<think>\nLet me check memory.\n</think>") {
		t.Errorf("multi-line thinking block not preserved verbatim:\n%s", out)
	}
	if !strings.Contains(out, "Reading the file now.") {
		t.Errorf("post-thinking content missing:\n%s", out)
	}
	if !strings.Contains(out, `[tool_call] call_abc memory_read({"path":"notes"})`) {
		t.Errorf("tool call line missing or malformed:\n%s", out)
	}
	if !strings.Contains(out, `[tool_call] call_def get_time({})`) {
		t.Errorf("second tool call missing:\n%s", out)
	}
}

func TestChatLogger_FormatToolMessageIncludesCallID(t *testing.T) {
	msg := Message{
		Role:       RoleTool,
		Content:    "[memory listing here]\nfile1.md\nfile2.md",
		ToolCallID: "call_abc",
	}

	var sb strings.Builder
	formatMessage(&sb, msg, "")
	out := sb.String()

	if !strings.Contains(out, "| tool | call_abc") {
		t.Errorf("tool message banner should include call ID, got:\n%s", out)
	}
	if !strings.Contains(out, "file1.md\nfile2.md") {
		t.Errorf("multi-line tool content not preserved:\n%s", out)
	}
}

// TestChatLogger_NilReceiverIsSafe verifies that callers can hold a nil
// *ChatLogger without nil-checking at every call site (e.g. when the chat
// log file fails to open at startup, NewAgent sets it to nil and continues).
func TestChatLogger_NilReceiverIsSafe(t *testing.T) {
	var c *ChatLogger
	c.LogIncoming([]Message{{Role: RoleUser, Content: "hi"}}, 0)
	c.LogResponse(Message{Role: RoleAssistant, Content: "hello"}, "stop")
	c.LogContextRebuild()
	if err := c.Close(); err != nil {
		t.Errorf("Close on nil receiver returned error: %v", err)
	}
}
