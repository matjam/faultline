package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// LLMClient is a minimal, hand-rolled client for the OpenAI-compatible
// /v1/chat/completions endpoint. It deliberately avoids any external SDK
// so we can pass vendor-specific sampler fields (top_k, min_p,
// repetition_penalty, etc.) that the OpenAI spec doesn't define but that
// most local backends (KoboldCpp, llama.cpp, vLLM) accept on the same
// endpoint. Streaming, embeddings, image generation, etc. are not
// implemented because the agent doesn't use them.
type LLMClient struct {
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

// NewLLMClient creates a client configured for the given OpenAI-compatible
// endpoint. apiURL must include the version prefix (e.g. "/v1"); we append
// "/chat/completions" to it. apiKey may be empty for endpoints that don't
// require auth (most local servers).
func NewLLMClient(apiURL, apiKey, model string, logger *slog.Logger) *LLMClient {
	return &LLMClient{
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
// transcript of every request/response. Pass nil to disable. The LLMClient
// does not take ownership of the lifecycle; the caller is responsible for
// calling Close() on the chat log at shutdown.
func (l *LLMClient) SetChatLog(c *ChatLogger) {
	l.chatLog = c
}

// ChatRequest is the public input to Chat(). The Model field is filled in
// by the client from its configured value, so callers don't set it.
//
// Sampler fields use plain (non-pointer) numeric types with omitempty
// semantics: a zero value means "don't send this field, let the server
// decide." This matches the previous go-openai behavior. Seed is the
// exception: it uses *int because 0 is a meaningful seed value, and we
// treat the config sentinel of 0 as "unset" higher up the stack.
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

// chatRequestWire is the JSON shape we POST. Kept separate from the public
// ChatRequest so we control exactly which keys appear on the wire (and
// with what `omitempty` behavior). Fields here are pointers/zero-omitted
// where needed to avoid sending defaults that override server-side
// configuration.
type chatRequestWire struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`

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

// apiError is the OpenAI-style error envelope: {"error": {"message": ...,
// "type": ..., "code": ...}}. We try to decode this for non-2xx responses
// to surface a useful message, and fall back to the raw body otherwise.
type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Chat sends a chat completion request and returns the parsed response.
// The HTTP request is bound to ctx; cancelling ctx aborts the in-flight
// HTTP call (the server-side generation may still continue until the
// backend notices, which is what KoboldExtras.Abort is for).
func (l *LLMClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	wire := chatRequestWire{
		Model:             l.model,
		Messages:          req.Messages,
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
		if json.Unmarshal(raw, &ae) == nil && ae.Error.Message != "" {
			return nil, fmt.Errorf("chat completion: HTTP %d: %s",
				resp.StatusCode, ae.Error.Message)
		}
		return nil, fmt.Errorf("chat completion: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out ChatResponse
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

// EstimateTokens provides a rough token count for a string.
// Uses the approximation of ~4 characters per token for English text.
// This is the heuristic fallback used when KoboldCpp's real tokenizer
// isn't available; see kobold.go for the accurate path.
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
