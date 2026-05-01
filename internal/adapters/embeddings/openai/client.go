// Package openai is the OpenAI-compatible /v1/embeddings adapter. It
// implements the embeddings interface used by the tools dispatcher to
// turn memory text and search queries into dense vectors.
//
// Hand-rolled HTTP, no SDK, mirroring internal/adapters/llm/openai/.
// This package is deliberately separate from the chat-completions
// adapter even though both target OpenAI-compatible servers — they
// have different endpoints, different request shapes, different
// failure modes, and conflating them would muddle the architecture.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Client is an OpenAI-compatible embeddings client. Safe for concurrent
// use; the underlying http.Client is the only mutable shared state.
type Client struct {
	apiURL  string // base URL ending in /v1 (no trailing slash)
	apiKey  string
	model   string
	timeout time.Duration
	http    *http.Client
	logger  *slog.Logger

	// dim is the vector dimensionality reported by the endpoint. Set
	// by Probe and immutable thereafter. Guarded by dimMu so Dim() can
	// be called safely before Probe completes (it returns 0 until set).
	dimMu sync.RWMutex
	dim   int
}

// New constructs an embeddings client. The dim is unknown until Probe is
// called; until then Dim() returns 0.
//
// apiURL must include the version prefix (e.g. "/v1"); "/embeddings" is
// appended internally. apiKey may be empty for endpoints that don't
// require auth. model is the embedding model identifier ("text-embedding-3-small",
// "nomic-embed-text", etc.).
//
// timeout is applied per HTTP request (set on the request context, not
// the http.Client itself, so callers can use a longer ambient context
// for batch operations and still bound individual calls).
func New(apiURL, apiKey, model string, timeout time.Duration, logger *slog.Logger) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		apiURL:  strings.TrimRight(apiURL, "/"),
		apiKey:  apiKey,
		model:   model,
		timeout: timeout,
		http:    &http.Client{},
		logger:  logger,
	}
}

// Model returns the embedding model identifier this client was
// constructed with.
func (c *Client) Model() string { return c.model }

// Dim returns the vector dimensionality, or 0 if Probe has not yet
// been called successfully.
func (c *Client) Dim() int {
	c.dimMu.RLock()
	defer c.dimMu.RUnlock()
	return c.dim
}

// Probe makes a single embedding request to discover the model's
// vector dimensionality. Must be called once before Dim() returns a
// useful value.
//
// Returns an error if the endpoint is unreachable, the response is
// malformed, or the response embedding has zero length. Callers should
// treat probe failure as "embeddings unavailable for this session"
// (log loudly, disable the feature) rather than fatal — the agent can
// run without semantic search.
func (c *Client) Probe(ctx context.Context) error {
	vecs, err := c.Embed(ctx, []string{"ping"})
	if err != nil {
		return fmt.Errorf("embeddings: probe: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return errors.New("embeddings: probe returned empty vector")
	}
	dim := len(vecs[0])

	c.dimMu.Lock()
	c.dim = dim
	c.dimMu.Unlock()

	c.logger.Info("embeddings: probe ok",
		slog.String("model", c.model),
		slog.Int("dim", dim))
	return nil
}

// embedRequest is the wire shape POSTed to /v1/embeddings. encoding_format
// is set to "float" explicitly even though it's the default — some
// non-OpenAI servers default to "base64" and we want raw floats.
type embedRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

// embedResponse is the wire shape returned by /v1/embeddings.
type embedResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	// Error response shape (some compatible servers).
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// Embed returns one vector per input string, in input order.
//
// The per-call timeout configured at New() is applied via a context
// derived from ctx. Caller cancellation propagates as well.
//
// Empty input returns an empty result without making a network call.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(embedRequest{
		Input:          texts,
		Model:          c.model,
		EncodingFormat: "float",
	})
	if err != nil {
		return nil, fmt.Errorf("embeddings: marshal request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	url := c.apiURL + "/embeddings"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embeddings: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: HTTP: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embeddings: read response: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		// Try to surface the server's error message if it sent one in
		// the standard shape; otherwise return a snippet of the raw
		// body for diagnosis.
		var er embedResponse
		if jerr := json.Unmarshal(respBody, &er); jerr == nil && er.Error != nil && er.Error.Message != "" {
			return nil, fmt.Errorf("embeddings: HTTP %d: %s", resp.StatusCode, er.Error.Message)
		}
		snippet := string(respBody)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return nil, fmt.Errorf("embeddings: HTTP %d: %s", resp.StatusCode, snippet)
	}

	var er embedResponse
	if err := json.Unmarshal(respBody, &er); err != nil {
		return nil, fmt.Errorf("embeddings: decode response: %w", err)
	}
	if er.Error != nil && er.Error.Message != "" {
		// Some servers return 200 with an error body. Treat that as a
		// failure rather than silently returning empty data.
		return nil, fmt.Errorf("embeddings: server error: %s", er.Error.Message)
	}
	if len(er.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: response count mismatch: got %d, want %d", len(er.Data), len(texts))
	}

	// Defensive: sort by index so ordering matches input even if the
	// server returns out of order.
	sort.Slice(er.Data, func(i, j int) bool { return er.Data[i].Index < er.Data[j].Index })

	out := make([][]float32, len(er.Data))
	for i, d := range er.Data {
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embeddings: empty vector at index %d", i)
		}
		out[i] = d.Embedding
	}
	return out, nil
}
