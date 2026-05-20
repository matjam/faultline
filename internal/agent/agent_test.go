package agent

import (
	"testing"

	"github.com/matjam/faultline/internal/llm"
)

// TestCoerceAssistantMessage_BareGetsPlaceholder is the agent-side half
// of the Venice fix: when the model returns a stop with no content and
// no tool calls, recording the literal `{"role":"assistant"}` in the
// message log poisons every subsequent request to backends that
// validate the assistant content-or-tool_calls rule. Coercing at append
// time keeps the persisted state file clean too, so a future restart
// against a strict backend doesn't re-hit the wire-side sanitizer for
// turns that were produced under this fix.
func TestCoerceAssistantMessage_BareGetsPlaceholder(t *testing.T) {
	in := llm.Message{Role: llm.RoleAssistant}
	out := coerceAssistantMessage(in)
	if out.Content != emptyAssistantPlaceholder {
		t.Fatalf("expected content=%q, got %q", emptyAssistantPlaceholder, out.Content)
	}
}

// TestCoerceAssistantMessage_PreservesContent verifies the function is
// a no-op when the message already carries content, tool calls, or
// both — we do not want to overwrite a real response.
func TestCoerceAssistantMessage_PreservesContent(t *testing.T) {
	cases := []llm.Message{
		{Role: llm.RoleAssistant, Content: "real reply"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "x", Type: llm.ToolTypeFunction}}},
		{Role: llm.RoleAssistant, Content: "both", ToolCalls: []llm.ToolCall{{ID: "x"}}},
	}
	for i, c := range cases {
		got := coerceAssistantMessage(c)
		if got.Content != c.Content {
			t.Fatalf("case %d: content changed from %q to %q", i, c.Content, got.Content)
		}
		if len(got.ToolCalls) != len(c.ToolCalls) {
			t.Fatalf("case %d: tool_calls changed", i)
		}
	}
}

// TestCoerceAssistantMessage_OtherRolesUntouched protects against the
// coercer over-reaching into roles whose required-field semantics it
// doesn't know.
func TestCoerceAssistantMessage_OtherRolesUntouched(t *testing.T) {
	for _, role := range []string{llm.RoleSystem, llm.RoleUser, llm.RoleTool} {
		in := llm.Message{Role: role}
		out := coerceAssistantMessage(in)
		if out.Content != "" {
			t.Fatalf("role %q: content set to %q", role, out.Content)
		}
	}
}
