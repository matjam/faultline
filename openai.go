package main

// This file defines the wire-format types for the OpenAI-compatible chat
// completions API. We previously used github.com/sashabaranov/go-openai,
// but that library does not expose a way to inject vendor-specific extras
// (top_k, min_p, repetition_penalty, etc.) into chat completion requests.
// Since faultline only needs a tiny slice of the API (one POST endpoint,
// no streaming, no embeddings), it was simpler to drop the dependency and
// own the types directly.
//
// JSON tags match what go-openai used to write, so existing state files
// (which contain serialized message logs) continue to load without
// migration. Fields go-openai included that we don't use (Refusal,
// MultiContent, Name, ReasoningContent, FunctionCall) are intentionally
// omitted: encoding/json silently ignores unknown fields on Unmarshal,
// so legacy state files with those keys still parse cleanly.

// Chat message roles understood by the OpenAI-compatible endpoint.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ToolTypeFunction is the only tool type the spec currently defines.
const ToolTypeFunction = "function"

// Message is one entry in the chat conversation log. It serves three
// purposes: input messages we send, assistant responses we receive, and
// the persisted on-disk format. Field tags must stay in sync with what
// go-openai wrote previously to preserve state-file compatibility.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall describes a single function the model wants the agent to run.
// Returned in assistant messages; satisfied with a Role=tool message
// carrying the matching ToolCallID.
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the (name, arguments) payload inside a ToolCall.
// Arguments is the raw JSON string the model produced; the executor
// parses it directly. We never round-trip through a Go map here.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Tool is one entry in the request's `tools` array, advertising a callable
// function to the model.
type Tool struct {
	Type     string       `json:"type"`
	Function *FunctionDef `json:"function,omitempty"`
}

// FunctionDef is a single function declaration. Parameters is intentionally
// `any` so callers can pass a map[string]any literal (which is what we do
// throughout tools.go) without needing a JSON-Schema builder type.
type FunctionDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// ChatResponse is the trimmed-down /v1/chat/completions response shape we
// actually consume. The real response carries usage stats, IDs, etc., but
// we don't use them and decoding ignores unknown fields.
type ChatResponse struct {
	Choices []Choice `json:"choices"`
}

// Choice is one generation option in a ChatResponse. We always read the
// first one; multi-choice (`n > 1`) is not used.
type Choice struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}
