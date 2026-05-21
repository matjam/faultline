package openai

import (
	"encoding/json"
	"testing"

	"github.com/matjam/faultline/internal/llm"
)

// marshalOne is a tiny helper: convert one llm.Message through the wire
// path and return the marshaled JSON for assertion. Keeps each test
// focused on the per-role rule it cares about.
func marshalOne(t *testing.T, m llm.Message) string {
	t.Helper()
	wire := toWireMessages([]llm.Message{m})
	raw, err := json.Marshal(wire[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// TestWire_AssistantWithToolCallsForcesEmptyContent is the core
// regression. Venice rejects assistant messages with tool_calls when
// `content` is either omitted or null; only an explicit empty string (or
// non-empty string) is accepted. Confirmed empirically against
// api.venice.ai with /v1/chat/completions returning HTTP 400 "list
// object has no element 0" on the omitted-content payload.
func TestWire_AssistantWithToolCallsForcesEmptyContent(t *testing.T) {
	in := llm.Message{
		Role: llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{
			{ID: "call_1", Type: llm.ToolTypeFunction, Function: llm.FunctionCall{Name: "sleep", Arguments: `{"seconds":1}`}},
		},
	}
	got := marshalOne(t, in)
	want := `{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"sleep","arguments":"{\"seconds\":1}"}}]}`
	if got != want {
		t.Fatalf("assistant+tool_calls wire shape mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestWire_AssistantBareForcesEmptyContent covers the legacy state-file
// case: bare `{"role":"assistant"}` entries from sessions that pre-date
// the agent-side coercer. The wire layer still must emit an explicit
// empty string so Venice accepts the request. (Going forward,
// agent.coerceAssistantMessage substitutes a placeholder at append
// time, so newly-recorded bare entries become e.g. "(no response)" on
// disk — but the wire layer is the last line of defense.)
func TestWire_AssistantBareForcesEmptyContent(t *testing.T) {
	in := llm.Message{Role: llm.RoleAssistant}
	got := marshalOne(t, in)
	want := `{"role":"assistant","content":""}`
	if got != want {
		t.Fatalf("bare assistant wire shape mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestWire_AssistantWithContentPreserved verifies normal assistant
// turns with real text content marshal unchanged.
func TestWire_AssistantWithContentPreserved(t *testing.T) {
	in := llm.Message{Role: llm.RoleAssistant, Content: "hello"}
	got := marshalOne(t, in)
	want := `{"role":"assistant","content":"hello"}`
	if got != want {
		t.Fatalf("assistant content wire shape mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestWire_ToolMessageContentAlwaysEmitted covers Venice's required-
// content rule on tool messages. We have never produced an empty-content
// tool message in practice (the wrapper prepends a timestamp), but the
// wire layer enforces the rule defensively.
func TestWire_ToolMessageContentAlwaysEmitted(t *testing.T) {
	got := marshalOne(t, llm.Message{Role: llm.RoleTool, Content: "result", ToolCallID: "call_1"})
	want := `{"role":"tool","content":"result","tool_call_id":"call_1"}`
	if got != want {
		t.Fatalf("tool message wire shape mismatch:\n  got:  %s\n  want: %s", got, want)
	}

	got = marshalOne(t, llm.Message{Role: llm.RoleTool, ToolCallID: "call_2"})
	want = `{"role":"tool","content":"","tool_call_id":"call_2"}`
	if got != want {
		t.Fatalf("empty-content tool message wire shape mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestWire_UserSystemPreserveOmitempty makes sure the converter doesn't
// over-reach: empty content on user/system messages is a caller bug we
// don't want to silently paper over by emitting a meaningless empty
// string. The omitempty behavior is preserved for these roles.
func TestWire_UserSystemPreserveOmitempty(t *testing.T) {
	got := marshalOne(t, llm.Message{Role: llm.RoleUser})
	if got != `{"role":"user"}` {
		t.Fatalf("empty user wire shape unexpected: %s", got)
	}
	got = marshalOne(t, llm.Message{Role: llm.RoleSystem})
	if got != `{"role":"system"}` {
		t.Fatalf("empty system wire shape unexpected: %s", got)
	}
	got = marshalOne(t, llm.Message{Role: llm.RoleUser, Content: "hi"})
	if got != `{"role":"user","content":"hi"}` {
		t.Fatalf("normal user wire shape unexpected: %s", got)
	}
}

// TestWire_InputNotMutated guards the no-mutation contract — the
// converter must not write through to the caller's llm.Message slice.
func TestWire_InputNotMutated(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "x"}}},
		{Role: llm.RoleTool, ToolCallID: "x"},
	}
	_ = toWireMessages(in)
	if in[0].Content != "" {
		t.Fatalf("toWireMessages mutated input[0].Content: %q", in[0].Content)
	}
	if in[1].Content != "" {
		t.Fatalf("toWireMessages mutated input[1].Content: %q", in[1].Content)
	}
}

// TestAPIError_OpenAINested is the original envelope shape from
// OpenAI / KoboldCpp / vLLM. Must continue to decode.
func TestAPIError_OpenAINested(t *testing.T) {
	body := []byte(`{"error":{"message":"context length exceeded","type":"invalid_request","code":"context_length_exceeded"}}`)
	var ae apiError
	if err := json.Unmarshal(body, &ae); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, want := ae.Message(), "context length exceeded"; got != want {
		t.Fatalf("Message() = %q, want %q", got, want)
	}
	if ae.RequestID != "" {
		t.Fatalf("RequestID should be empty, got %q", ae.RequestID)
	}
}

// TestAPIError_VeniceFlat is the new shape we discovered when switching
// to Venice. Top-level `error` is a string, with a sibling `request_id`.
// Both must be extracted without confusing the OpenAI-shape decoder.
func TestAPIError_VeniceFlat(t *testing.T) {
	body := []byte(`{"error":"list object has no element 0","request_id":"bUdIJBuzMNo4MHfEyEr0R"}`)
	var ae apiError
	if err := json.Unmarshal(body, &ae); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, want := ae.Message(), "list object has no element 0"; got != want {
		t.Fatalf("Message() = %q, want %q", got, want)
	}
	if got, want := ae.RequestID, "bUdIJBuzMNo4MHfEyEr0R"; got != want {
		t.Fatalf("RequestID = %q, want %q", got, want)
	}
}

// TestAPIError_Unknown verifies that a body matching neither shape
// returns "" from Message() so the caller falls back to raw body.
func TestAPIError_Unknown(t *testing.T) {
	body := []byte(`{"something":"else"}`)
	var ae apiError
	if err := json.Unmarshal(body, &ae); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ae.Message() != "" {
		t.Fatalf("expected empty Message() for unknown shape, got %q", ae.Message())
	}
}
