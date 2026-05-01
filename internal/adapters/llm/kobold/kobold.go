// Package kobold is the KoboldCpp-extras adapter: real tokenization,
// generation aborts, and backend perf metrics that sit alongside the
// OpenAI compatibility layer at the same base URL.
package kobold

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/llm"
)

// Client provides access to KoboldCpp-specific endpoints that sit
// alongside the OpenAI compatibility layer. These give us:
//
//   - Real tokenization via /api/extra/tokencount (vs. our 4-chars-per-token
//     heuristic, which under-counts code/JSON heavily).
//   - Generation aborts via /api/extra/abort, so a forced shutdown actually
//     stops in-flight inference instead of leaving the GPU/CPU busy.
//   - Backend performance metrics via /api/extra/perf for diagnostics.
//
// Detection is best-effort. If the configured API URL is not actually a
// KoboldCpp server (e.g. real OpenAI, vLLM, llama.cpp's openai endpoint),
// Detect() returns an error and the rest of the agent falls back to
// heuristics. The agent never depends on this client succeeding.
type Client struct {
	baseURL string // e.g. "http://localhost:5001" (no /v1, no trailing slash)
	http    *http.Client
	logger  *slog.Logger

	detected bool   // true after a successful Detect()
	version  string // KoboldCpp version reported by /api/extra/version
}

// chatTemplateOverhead is added to each message's real token count to
// approximate the chat-template scaffolding that the OpenAI-compat endpoint
// inserts but that /api/extra/tokencount can't see (role tags like
// "<|im_start|>user\n", end-of-turn markers, etc.). Empirical estimate; varies
// per template (ChatML adds ~7, Llama-3 ~10, Mistral ~5).
const chatTemplateOverhead = 10

// New constructs a Client. apiURL is the OpenAI-compat URL from config
// (e.g. "http://localhost:5001/v1"); we derive the kcpp root from it by
// trimming the /v1 suffix.
func New(apiURL string, logger *slog.Logger) *Client {
	base := strings.TrimRight(apiURL, "/")
	base = strings.TrimSuffix(base, "/v1")
	return &Client{
		baseURL: base,
		http:    &http.Client{Timeout: 5 * time.Second},
		logger:  logger,
	}
}

// Detect probes /api/extra/version. Returns nil and marks the client as
// detected on success; returns an error and leaves the client unusable
// otherwise. Callers should treat detection failure as "this is not a
// KoboldCpp endpoint" and proceed without the extras.
func (k *Client) Detect(ctx context.Context) error {
	var resp struct {
		Result  string `json:"result"`
		Version string `json:"version"`
	}
	if err := k.getJSON(ctx, "/api/extra/version", &resp); err != nil {
		return fmt.Errorf("kobold extras detect: %w", err)
	}
	if resp.Version == "" {
		return fmt.Errorf("kobold extras detect: empty version response")
	}
	k.detected = true
	k.version = resp.Version
	k.logger.Info("kobold extras detected",
		"version", resp.Version, "url", k.baseURL)
	return nil
}

// Detected reports whether Detect() succeeded.
func (k *Client) Detected() bool { return k != nil && k.detected }

// Version returns the KoboldCpp version reported during detection.
func (k *Client) Version() string {
	if k == nil {
		return ""
	}
	return k.version
}

// CountString returns the real token count for a string using the loaded
// model's tokenizer. Uses a 5s timeout. On any error the heuristic estimate
// is returned and the failure is logged at debug level.
func (k *Client) CountString(ctx context.Context, s string) int {
	if !k.Detected() {
		return llm.EstimateTokens(s)
	}
	if s == "" {
		return 0
	}
	var resp struct {
		Value int `json:"value"`
	}
	body := map[string]string{"prompt": s}
	if err := k.postJSON(ctx, "/api/extra/tokencount", body, &resp); err != nil {
		k.logger.Debug("tokencount failed, falling back to heuristic", "error", err)
		return llm.EstimateTokens(s)
	}
	return resp.Value
}

// CountMessages returns the token count for an entire chat message log.
// Concatenates message contents and tool-call payloads, tokenizes once
// against the live model tokenizer, then adds a per-message overhead
// constant to approximate chat-template scaffolding that the tokenizer
// endpoint doesn't see.
//
// On any error, falls back to llm.EstimateMessagesTokens.
func (k *Client) CountMessages(ctx context.Context, messages []llm.Message) int {
	if !k.Detected() || len(messages) == 0 {
		return llm.EstimateMessagesTokens(messages)
	}

	// Build a single prompt-shaped blob covering everything the model
	// actually has to ingest: each message's content plus the JSON payload
	// of any tool calls. We don't try to mimic the chat template exactly;
	// the per-message overhead constant absorbs that.
	var sb strings.Builder
	for _, m := range messages {
		sb.WriteString(m.Content)
		sb.WriteByte('\n')
		for _, tc := range m.ToolCalls {
			sb.WriteString(tc.Function.Name)
			sb.WriteByte('\n')
			sb.WriteString(tc.Function.Arguments)
			sb.WriteByte('\n')
		}
	}

	var resp struct {
		Value int `json:"value"`
	}
	body := map[string]string{"prompt": sb.String()}
	if err := k.postJSON(ctx, "/api/extra/tokencount", body, &resp); err != nil {
		k.logger.Debug("tokencount failed, falling back to heuristic",
			"error", err, "messages", len(messages))
		return llm.EstimateMessagesTokens(messages)
	}
	return resp.Value + len(messages)*chatTemplateOverhead
}

// Abort tells the backend to stop the currently running generation. This is
// best-effort: it's safe to call even when no generation is in flight, and
// errors are logged but not propagated. Used during forced shutdown so the
// model doesn't keep eating GPU after we exit.
func (k *Client) Abort(ctx context.Context) {
	if !k.Detected() {
		return
	}
	var resp struct {
		Success bool `json:"success"`
	}
	if err := k.postJSON(ctx, "/api/extra/abort", struct{}{}, &resp); err != nil {
		k.logger.Debug("abort request failed", "error", err)
		return
	}
	k.logger.Info("kobold abort sent", "success", resp.Success)
}

// PerfInfo is a subset of /api/extra/perf, capturing the fields most useful
// for diagnostics.
type PerfInfo struct {
	LastProcessTime float64 `json:"last_process_time"`  // seconds spent prompt-processing
	LastEvalTime    float64 `json:"last_eval_time"`     // seconds spent generating
	LastTokenCount  int     `json:"last_token_count"`   // tokens generated last call
	LastInputCount  int     `json:"last_input_count"`   // input tokens last call
	LastProcessSpd  float64 `json:"last_process_speed"` // input tokens/sec
	LastEvalSpd     float64 `json:"last_eval_speed"`    // output tokens/sec
	TotalGens       int     `json:"total_gens"`
	Queue           int     `json:"queue"`
	Idle            int     `json:"idle"`
	StopReason      int     `json:"stop_reason"` // -1 invalid, 0 OOT, 1 EOS, 2 custom
	Uptime          int     `json:"uptime"`      // server uptime in seconds
}

// Perf fetches recent performance information from the backend. Returns nil
// without error when the client isn't detected, so callers can use the
// "perf == nil" check as a feature gate.
func (k *Client) Perf(ctx context.Context) (*PerfInfo, error) {
	if !k.Detected() {
		return nil, nil
	}
	var perf PerfInfo
	if err := k.getJSON(ctx, "/api/extra/perf", &perf); err != nil {
		return nil, fmt.Errorf("kobold perf: %w", err)
	}
	return &perf, nil
}

// StopReasonString translates the integer stop_reason into a human label.
func StopReasonString(code int) string {
	switch code {
	case -1:
		return "invalid"
	case 0:
		return "max-tokens-hit"
	case 1:
		return "eos-token"
	case 2:
		return "custom-stopper"
	default:
		return fmt.Sprintf("unknown(%d)", code)
	}
}

// --- HTTP helpers ----------------------------------------------------------

func (k *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	return k.do(req, out)
}

func (k *Client) postJSON(ctx context.Context, path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return k.do(req, out)
}

func (k *Client) do(req *http.Request, out any) error {
	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a small amount of body for diagnostics, then bail.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
