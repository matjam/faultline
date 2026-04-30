package llm

// ChatRequest is the public input to a ChatModel.Chat() call. The Model
// field is filled in by the adapter from its configured value, so callers
// don't set it.
//
// Sampler fields use plain (non-pointer) numeric types with omitempty
// semantics: a zero value means "don't send this field, let the server
// decide." Seed is the exception: it uses *int because 0 is a meaningful
// seed value, and the agent's config sentinel of 0 is treated as "unset"
// higher up the stack.
type ChatRequest struct {
	Messages []Message
	Tools    []Tool

	// OpenAI-spec sampler parameters.
	Temperature      float32
	TopP             float32
	PresencePenalty  float32
	FrequencyPenalty float32
	Seed             int // 0 = unset (passed through agent config)
	MaxTokens        int

	// Vendor extensions accepted by KoboldCpp / llama.cpp / vLLM on the
	// /v1/chat/completions endpoint. Not part of the OpenAI spec; sent
	// as additional JSON fields when non-zero. Most servers silently
	// ignore unknown fields, so passing these to an endpoint that
	// doesn't understand them is harmless.
	TopK              int
	MinP              float32
	RepetitionPenalty float32
}

// EstimateTokens provides a rough token count for a string.
// Uses the approximation of ~4 characters per token for English text.
// This is the heuristic fallback used when a real tokenizer (e.g.
// KoboldCpp's /api/extra/tokencount) isn't available.
func EstimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	return len(text) / 4
}

// EstimateMessagesTokens estimates total tokens across all messages,
// including a small per-message overhead and per-tool-call overhead to
// approximate chat-template scaffolding the heuristic can't see.
func EstimateMessagesTokens(messages []Message) int {
	total := 0
	for _, m := range messages {
		total += EstimateTokens(m.Content)
		// Account for role and message overhead
		total += 4
		for _, tc := range m.ToolCalls {
			total += EstimateTokens(tc.Function.Name)
			total += EstimateTokens(tc.Function.Arguments)
			total += 4
		}
	}
	return total
}
