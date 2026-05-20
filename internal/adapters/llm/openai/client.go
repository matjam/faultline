// Package openai is the OpenAI-compatible /v1/chat/completions adapter.
// It implements the agent's ChatModel port over plain HTTP and JSON.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/matjam/faultline/internal/llm"
)

// Client is a minimal, hand-rolled client for the OpenAI-compatible
// /v1/chat/completions endpoint. It deliberately avoids any external SDK
// so we can pass vendor-specific sampler fields (top_k, min_p,
// repetition_penalty, etc.) that the OpenAI spec doesn't define but that
// most local backends (KoboldCpp, llama.cpp, vLLM) accept on the same
// endpoint. Streaming, embeddings, image generation, etc. are not
// implemented because the agent doesn't use them.
type Client struct {
	apiURL  string // base URL, e.g. http://host:5001/v1 (no trailing slash)
	apiKey  string // optional; sent as Bearer token when non-empty
	model   string
	http    *http.Client
	logger  *slog.Logger
	chatLog *ChatLogger // optional; nil-safe

	// lastLoggedAt is the index of the first message that has not yet been
	// debug-logged. On each Chat() call we only log messages with index >=
	// lastLoggedAt, then advance it to len(messages). This avoids
	// re-logging the entire conversation on every turn (the message list
	// grows monotonically within a single context lifetime).
	//
	// Assumption: the message slice grows append-only between calls. When
	// the agent rebuilds context (compaction, restart) the new slice is
	// shorter than lastLoggedAt; we detect that and reset to log the full
	// new context once. Any other shrinkage will also trigger a full
	// re-log on the next call, which is cosmetic noise but not incorrect.
	lastLoggedAt int

	// chatLogged tracks the chat-transcript log separately from slog.
	// Two key differences from lastLoggedAt:
	//   1. After LogResponse, this is bumped to len(messages)+1 so the
	//      response is not re-logged on the next turn (the agent appends
	//      the response to the message slice between Chat() calls; the
	//      next call's "incoming" range would otherwise include it).
	//   2. On context rebuild (len(messages) < chatLogged), we emit a
	//      marker and skip the rebuilt content rather than re-dumping
	//      the system prompt + summary -- those aren't part of the
	//      "conversation" from a human reader's perspective; the turns
	//      that produced the summary were already transcribed.
	chatLogged int
}

// New creates a Client configured for the given OpenAI-compatible
// endpoint. apiURL must include the version prefix (e.g. "/v1"); we append
// "/chat/completions" to it. apiKey may be empty for endpoints that don't
// require auth (most local servers).
func New(apiURL, apiKey, model string, logger *slog.Logger) *Client {
	return &Client{
		apiURL: strings.TrimRight(apiURL, "/"),
		apiKey: apiKey,
		model:  model,
		// No global timeout: a long generation can legitimately take
		// many minutes on a CPU backend. Cancellation is driven by the
		// caller's context. Connect-level timeouts are inherited from
		// http.DefaultTransport, which is enough to fail fast on a dead
		// host without prematurely killing in-flight generations.
		http:   &http.Client{},
		logger: logger,
	}
}

// SetChatLog attaches a chat logger that will receive a human-readable
// transcript of every request/response. Pass nil to disable. The Client
// does not take ownership of the lifecycle; the caller is responsible for
// calling Close() on the chat log at shutdown.
func (l *Client) SetChatLog(c *ChatLogger) {
	l.chatLog = c
}

// wireMessage mirrors llm.Message but uses a pointer for Content so the
// marshaller can distinguish "field intentionally absent" from "field
// explicitly empty string". This matters because backends disagree on
// what an assistant message with tool_calls and no text must look like:
//
//   - OpenAI / KoboldCpp / vLLM: accept content omitted entirely.
//   - Venice: rejects with HTTP 400 "list object has no element 0"
//     unless content is present as an explicit empty string (or a
//     non-empty string). `"content": null` also fails. Empirically
//     confirmed against the live API, May 2026.
//
// Our shared llm.Message uses `Content string` + `omitempty`, so it
// cannot express "present but empty". Rather than mutating the shared
// type (which is also the on-disk state-file shape and the API the rest
// of the codebase reasons about), the wire shape is private here and
// the toWireMessages converter decides per-role whether to force the
// field on.
type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content,omitempty"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// emptyContent is the canonical "" pointer used when a message needs to
// emit `"content": ""` on the wire. Sharing one pointer avoids 270+ tiny
// allocations per request on a long resumed conversation.
var emptyContent = func() *string { s := ""; return &s }()

// toWireMessages converts the agent's message slice into the per-message
// wire shape Venice (and presumably other strict OpenAI-compatible
// backends) will accept. Rules:
//
//   - assistant: content is ALWAYS emitted. If the source message has
//     non-empty content, that's what we send. If it has tool_calls but
//     no content, we send "". If it has neither (legacy state files
//     resumed from a prior run on a more permissive backend), we send
//     "" — the on-disk shape is fixed at append time by
//     agent.coerceAssistantMessage going forward, but the wire layer
//     handles pre-existing entries too.
//   - tool: content is ALWAYS emitted. Venice's schema marks it
//     `required`. We have never produced an empty-content tool message
//     in practice (the wrapper adds a timestamp prefix), but explicit
//     emission costs nothing and removes one class of future surprise.
//   - user / system: content emitted when non-empty. These roles
//     legitimately may have empty content in odd edge cases (e.g. a
//     blank shutdown prompt); the `omitempty` behavior is preserved.
//
// The input slice is never mutated.
func toWireMessages(msgs []llm.Message) []wireMessage {
	out := make([]wireMessage, len(msgs))
	for i, m := range msgs {
		w := wireMessage{
			Role:       m.Role,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
		}
		switch m.Role {
		case llm.RoleAssistant, llm.RoleTool:
			// Always emit content for these roles. Backends that
			// accept omitted content are equally happy with an
			// explicit empty string; backends that reject omitted
			// content need it. Force it on.
			if m.Content == "" {
				w.Content = emptyContent
			} else {
				c := m.Content
				w.Content = &c
			}
		default:
			// system / user / unknown: preserve omitempty semantics.
			// Empty content here would be a caller bug; we don't
			// want to mask it with an empty string on the wire.
			if m.Content != "" {
				c := m.Content
				w.Content = &c
			}
		}
		out[i] = w
	}
	return out
}

// chatRequestWire is the JSON shape we POST. Kept separate from llm.ChatRequest
// so we control exactly which keys appear on the wire (and with what
// `omitempty` behavior). Fields here are pointers/zero-omitted where
// needed to avoid sending defaults that override server-side configuration.
type chatRequestWire struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []llm.Tool    `json:"tools,omitempty"`

	// Sampler params (OpenAI-spec). All `omitempty`: if the agent sets
	// 0, the field is omitted and the server uses its default.
	Temperature      float32 `json:"temperature,omitempty"`
	TopP             float32 `json:"top_p,omitempty"`
	PresencePenalty  float32 `json:"presence_penalty,omitempty"`
	FrequencyPenalty float32 `json:"frequency_penalty,omitempty"`
	Seed             *int    `json:"seed,omitempty"`
	MaxTokens        int     `json:"max_tokens,omitempty"`

	// Vendor extensions. Same omit-when-zero rule.
	TopK              int     `json:"top_k,omitempty"`
	MinP              float32 `json:"min_p,omitempty"`
	RepetitionPenalty float32 `json:"repetition_penalty,omitempty"`
}

// apiError is a tolerant decoder for the assorted error-envelope shapes
// emitted by "OpenAI-compatible" backends on non-2xx responses. Two
// known forms in the wild:
//
//   - OpenAI / KoboldCpp / vLLM (the spec shape):
//     {"error": {"message": "...", "type": "...", "code": "..."}}
//   - Venice (and presumably others):
//     {"error": "string message", "request_id": "..."}
//
// We capture `error` as a raw message and try the object form first, the
// string form second. The Message() helper returns whichever decoded
// successfully, or "" if neither did (caller falls back to raw body).
type apiError struct {
	Error     json.RawMessage `json:"error"`
	RequestID string          `json:"request_id,omitempty"`
}

// Message extracts a human-readable error string from whichever envelope
// the server returned. Empty return means the body did not match either
// known shape and the caller should fall back to the raw body.
func (e *apiError) Message() string {
	if len(e.Error) == 0 {
		return ""
	}
	var obj struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(e.Error, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	var s string
	if json.Unmarshal(e.Error, &s) == nil && s != "" {
		return s
	}
	return ""
}

// Chat sends a chat completion request and returns the parsed response.
// The HTTP request is bound to ctx; canceling ctx aborts the in-flight
// HTTP call (the server-side generation may still continue until the
// backend notices, which is what kobold.Client.Abort is for).
func (l *Client) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	wire := chatRequestWire{
		Model:             l.model,
		Messages:          toWireMessages(req.Messages),
		Tools:             req.Tools,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		PresencePenalty:   req.PresencePenalty,
		FrequencyPenalty:  req.FrequencyPenalty,
		MaxTokens:         req.MaxTokens,
		TopK:              req.TopK,
		MinP:              req.MinP,
		RepetitionPenalty: req.RepetitionPenalty,
	}
	// Seed=0 in config means "unset". Use a pointer so we can distinguish
	// "not configured" from "configured to 0" if we ever care to.
	if req.Seed != 0 {
		s := req.Seed
		wire.Seed = &s
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	l.logger.Debug("sending chat request",
		"messages", len(req.Messages),
		"tools", len(req.Tools),
		"model", l.model,
		"body_bytes", len(body),
	)

	// Only log NEW messages since the last request. See the doc comment
	// on lastLoggedAt for the invariant being maintained here.
	start := l.lastLoggedAt
	if len(req.Messages) < start {
		// Message list shrank: context was rebuilt (compaction or fresh
		// run). Log the entire new context once.
		start = 0
	}
	for i := start; i < len(req.Messages); i++ {
		m := req.Messages[i]
		l.logger.Debug(">>> message",
			"index", i,
			"role", m.Role,
			"content", m.Content,
			"tool_call_id", m.ToolCallID,
		)
		for _, tc := range m.ToolCalls {
			l.logger.Debug(">>> tool_call",
				"index", i,
				"id", tc.ID,
				"function", tc.Function.Name,
				"arguments", tc.Function.Arguments,
			)
		}
	}
	l.lastLoggedAt = len(req.Messages)

	// Chat-transcript log uses a separate counter (see field comment on
	// chatLogged). On rebuild we emit a marker and skip the rebuilt
	// content; otherwise we log only the delta since the previous turn.
	if len(req.Messages) < l.chatLogged {
		l.chatLog.LogContextRebuild()
		l.chatLogged = len(req.Messages)
	} else {
		l.chatLog.LogIncoming(req.Messages, l.chatLogged)
		l.chatLogged = len(req.Messages)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		l.apiURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build chat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if l.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+l.apiKey)
	}

	resp, err := l.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("chat completion: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a bounded amount of body so an enormous error page
		// can't blow up our log line. 4KB is plenty for any sensible
		// error envelope.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var ae apiError
		if json.Unmarshal(raw, &ae) == nil {
			if msg := ae.Message(); msg != "" {
				if ae.RequestID != "" {
					return nil, fmt.Errorf("chat completion: HTTP %d: %s (request_id=%s)",
						resp.StatusCode, msg, ae.RequestID)
				}
				return nil, fmt.Errorf("chat completion: HTTP %d: %s",
					resp.StatusCode, msg)
			}
		}
		return nil, fmt.Errorf("chat completion: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out llm.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode chat response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	msg := out.Choices[0].Message
	l.logger.Debug("<<< response",
		"finish_reason", out.Choices[0].FinishReason,
		"content", msg.Content,
	)
	for _, tc := range msg.ToolCalls {
		l.logger.Debug("<<< tool_call",
			"id", tc.ID,
			"function", tc.Function.Name,
			"arguments", tc.Function.Arguments,
		)
	}

	// Chat-transcript log: write the response and bump our counter past
	// it. The agent will append this exact message to its slice between
	// Chat() calls; on the next call we'd otherwise re-log it through
	// LogIncoming. The +1 prevents that.
	l.chatLog.LogResponse(msg, out.Choices[0].FinishReason)
	l.chatLogged++

	return &out, nil
}
