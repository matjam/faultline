package openai

import (
	"encoding/json"
	"testing"

	"github.com/matjam/faultline/internal/llm"
)

// TestSanitizeForWire_LegacyBareAssistant covers the regression that
// triggered this code: state files resumed from a previous run can
// contain assistant messages with neither content nor tool_calls. With
// `omitempty` on both struct fields they serialize over the wire as
// literally `{"role":"assistant"}`, which Venice (and presumably other
// strict OpenAI-compatible backends) rejects with HTTP 400 "list object
// has no element 0". The sanitizer must inject placeholder content so
// the wire shape satisfies the assistant content-or-tool_calls rule.
func TestSanitizeForWire_LegacyBareAssistant(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleSystem, Content: "system"},
		{Role: llm.RoleUser, Content: "hello"},
		{Role: llm.RoleAssistant}, // the offending shape
		{Role: llm.RoleUser, Content: "?"},
	}
	out := sanitizeForWire(in)

	// Input slice must not be mutated. Other adapters / loggers /
	// inspectors share the same Messages slice through the agent loop;
	// rewriting it under them would be a footgun.
	if in[2].Content != "" {
		t.Fatalf("sanitizeForWire mutated caller's slice: in[2].Content=%q", in[2].Content)
	}
	if out[2].Content != emptyAssistantPlaceholder {
		t.Fatalf("expected placeholder %q, got %q", emptyAssistantPlaceholder, out[2].Content)
	}

	// Marshaling the sanitized message must produce a JSON object with
	// a `content` field — the whole point of the fix.
	raw, err := json.Marshal(out[2])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(raw), `{"role":"assistant","content":"(no response)"}`; got != want {
		t.Fatalf("wire shape mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestSanitizeForWire_NoOp confirms that messages already satisfying the
// content-or-tool_calls rule are returned untouched (and that the slice
// is the exact same backing array — we promised no copy when no fix is
// needed, which matters because the message log can be large).
func TestSanitizeForWire_NoOp(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleSystem, Content: "system"},
		{Role: llm.RoleAssistant, Content: "i have content"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "x", Type: llm.ToolTypeFunction}}},
		{Role: llm.RoleTool, Content: "result", ToolCallID: "x"},
	}
	out := sanitizeForWire(in)
	if &out[0] != &in[0] {
		t.Fatal("sanitizeForWire allocated a copy when none was needed")
	}
}

// TestSanitizeForWire_NonAssistantUntouched guards against the sanitizer
// over-reaching. A user / tool / system message with no content is the
// caller's problem (and rare); we only know what the assistant rule
// requires.
func TestSanitizeForWire_NonAssistantUntouched(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleUser},
		{Role: llm.RoleTool, ToolCallID: "x"},
	}
	out := sanitizeForWire(in)
	for i := range in {
		if out[i].Content != "" {
			t.Fatalf("sanitizer touched non-assistant message %d: %+v", i, out[i])
		}
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
