package openai

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/llm"
	"github.com/matjam/faultline/internal/log"
)

// ChatLogger writes a human-readable transcript of every message exchanged
// with the LLM. Unlike the slog debug log -- which escapes newlines and is
// optimized for grepping single-line records -- the chat log preserves
// formatting so you can read prose, code, and `<think>` reasoning traces
// directly without un-escaping anything.
//
// One file per day, named chat-YYYY-MM-DD.log under cfg.Log.Dir. Daily
// rotation and concurrent-write safety are inherited from the underlying
// log.Daily writer.
//
// All methods are nil-safe so callers can omit the chat logger without
// peppering nil checks at every call site.
type ChatLogger struct {
	w *log.Daily
}

// NewChatLogger returns a logger that writes to dir/chat-YYYY-MM-DD.log.
// Directory creation and rotation are handled by the writer.
func NewChatLogger(dir string) (*ChatLogger, error) {
	w, err := log.NewDailyPrefixed(dir, "chat-")
	if err != nil {
		return nil, err
	}
	return &ChatLogger{w: w}, nil
}

// LogIncoming writes messages[start:] to the log. Mirrors the LLM client's
// lastLoggedAt invariant: only messages new since the previous call are
// appended. When context is rebuilt (compaction or fresh start) the caller
// passes start=0 to re-log the full new context.
func (c *ChatLogger) LogIncoming(messages []llm.Message, start int) {
	if c == nil {
		return
	}
	if start < 0 {
		start = 0
	}
	for i := start; i < len(messages); i++ {
		c.writeMessage(messages[i], "")
	}
}

// LogResponse writes the assistant message that came back from the model,
// including the finish reason for context (`stop`, `tool_calls`, `length`,
// etc.). Should be called once per Chat() round-trip.
func (c *ChatLogger) LogResponse(msg llm.Message, finishReason string) {
	if c == nil {
		return
	}
	c.writeMessage(msg, finishReason)
}

// LogContextRebuild writes a separator marking that the context window was
// rebuilt (compaction or restart). The newly-rebuilt context (system
// prompt, summary, recent memories) is intentionally NOT transcribed: the
// turns that produced the summary were already logged as they happened,
// and the rebuilt context is just what the model now sees, not new
// conversation. Subsequent turns continue the transcript naturally.
func (c *ChatLogger) LogContextRebuild() {
	if c == nil {
		return
	}
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	header := fmt.Sprintf("=== %s | [context rebuilt]", ts)
	if pad := bannerWidth - len(header) - 1; pad > 0 {
		header += " " + strings.Repeat("=", pad)
	}
	_, _ = io.WriteString(c.w, header+"\n\n")
}

// Close releases the underlying file.
func (c *ChatLogger) Close() error {
	if c == nil {
		return nil
	}
	return c.w.Close()
}

// bannerWidth is the visual width we pad each banner line to. Wide enough
// for typical terminal viewing without wrapping, narrow enough that a
// header line fits even with a long tool_call_id.
const bannerWidth = 100

// writeMessage formats one message and writes it in a single Write() so
// concurrent callers don't interleave (log.Daily mutexes its Write).
func (c *ChatLogger) writeMessage(m llm.Message, finishReason string) {
	var sb strings.Builder
	formatMessage(&sb, m, finishReason)
	_, _ = io.WriteString(c.w, sb.String())
}

// formatMessage renders one message as banner + content + tool calls into
// the supplied builder. Pulled out as a free function so the format is
// unit-testable without exercising the log.Daily pipeline.
//
// Format:
//
//	=== <ts> | <role>[ | <finish>][ | <tool_call_id>] ====...
//	<content verbatim, newlines preserved>
//	[tool_call] <id> <name>(<json args>)
//	...
//	(blank line separator)
func formatMessage(sb *strings.Builder, m llm.Message, finishReason string) {
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	header := fmt.Sprintf("=== %s | %s", ts, m.Role)
	if finishReason != "" {
		header += " | " + finishReason
	}
	if m.ToolCallID != "" {
		header += " | " + m.ToolCallID
	}
	if pad := bannerWidth - len(header) - 1; pad > 0 {
		header += " " + strings.Repeat("=", pad)
	}
	sb.WriteString(header)
	sb.WriteByte('\n')

	if m.Content != "" {
		sb.WriteString(m.Content)
		if !strings.HasSuffix(m.Content, "\n") {
			sb.WriteByte('\n')
		}
	}

	// Tool calls: one line per call. Arguments are already JSON; we
	// don't try to pretty-print because that would require parsing and
	// the raw form is fine for visual inspection.
	for _, tc := range m.ToolCalls {
		fmt.Fprintf(sb, "[tool_call] %s %s(%s)\n",
			tc.ID, tc.Function.Name, tc.Function.Arguments)
	}

	// Trailing blank line as a separator before the next banner.
	sb.WriteByte('\n')
}
