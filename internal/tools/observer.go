package tools

import (
	"strings"
	"time"
)

// Observer is an optional hook the composition root can attach to an
// Executor to receive a record of every tool call after it completes.
// Implemented by the admin UI's ring buffer; nil-safe — when no
// observer is wired, Execute is unchanged.
//
// OnToolCall is called synchronously after each dispatch returns.
// Implementations must not block (a slow observer slows down every
// tool call); the ring-buffer implementation copies into a fixed
// slot under a brief mutex.
type Observer interface {
	OnToolCall(ev ToolCallEvent)
}

// ToolCallEvent is the per-call record passed to Observer.OnToolCall.
// Args/Result are pre-truncated; the implementation in executor.go
// uses summarizeToolField to keep the record compact.
type ToolCallEvent struct {
	// Mode is "primary" or "subagent" depending on the Executor
	// instance that ran the call.
	Mode string

	// Name is the tool's registered name (e.g. "memory_write").
	Name string

	// CallID is the OpenAI tool_call_id from the LLM. Useful for
	// correlating with the chat transcript log.
	CallID string

	// StartedAt is the wall-clock at the start of dispatch.
	StartedAt time.Time

	// Duration is the wall-clock elapsed time of the dispatch.
	Duration time.Duration

	// ArgsSummary is the raw JSON arguments string truncated to a
	// fixed budget. Useful for showing "what was this call about"
	// without dumping a 50KB memory_write into the UI.
	ArgsSummary string

	// ArgsBytes is the original argument string length, before
	// truncation, so the UI can show "(truncated, N bytes)".
	ArgsBytes int

	// ResultSummary is the dispatch result string truncated to a
	// fixed budget.
	ResultSummary string

	// ResultBytes is the original result length, before truncation.
	ResultBytes int

	// Error, when non-empty, indicates a tool result that begins
	// with "Error:" or another error-shaped prefix. We don't have
	// a typed error channel from Execute (it returns string), so
	// we lift error-shaped results here for UI styling. Empty for
	// successful results.
	Error string
}

// summarizeToolField truncates a tool argument or result to at most
// maxLen runes (not bytes), appending an ellipsis when truncation
// happens. Newlines are collapsed to spaces so the UI can render
// each event on a single line.
//
// Operates on runes rather than bytes so a multi-byte UTF-8 sequence
// at the boundary doesn't get split mid-codepoint.
func summarizeToolField(s string, maxLen int) string {
	// Cheap collapse of internal whitespace so the UI line stays
	// one line. We don't want a memory_write payload's body to
	// stretch the table.
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	// Collapse runs of spaces.
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	s = strings.TrimSpace(s)

	if maxLen <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "…"
}

// classifyToolError lifts error-shaped tool results into the Error
// field of the event. The tools layer historically returns errors as
// strings prefixed with "Error:" or similar; we don't reclassify the
// result, just surface a UI hint.
func classifyToolError(result string) string {
	trimmed := strings.TrimSpace(result)
	switch {
	case strings.HasPrefix(trimmed, "Error:"),
		strings.HasPrefix(trimmed, "ERROR:"),
		strings.HasPrefix(trimmed, "error:"),
		strings.HasPrefix(trimmed, "Failed:"),
		strings.HasPrefix(trimmed, "Tool"):
		// Most tool helpers prepend "Error:" or "Failed:" on
		// failure. "Tool ..." catches the "Tool X is not
		// available" subagent-rejection path. False-positives
		// here only affect UI styling, not behavior.
		// Truncate to a one-liner.
		return summarizeToolField(trimmed, 200)
	}
	return ""
}
