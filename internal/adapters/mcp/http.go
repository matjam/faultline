package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPClient is the Streamable-HTTP-shaped MCP client. It implements Caller.
type HTTPClient struct {
	servers map[string]ServerConfig
	client  *http.Client
}

const defaultProtocolVersion = "2025-06-18"

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	ID     int             `json:"id,omitempty"`
	Result json.RawMessage `json:"result"`
	Error  *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type sessionInfo struct {
	ProtocolVersion string
	SessionID       string
}

// NewHTTPClient returns a client for HTTP MCP servers.
func NewHTTPClient(servers []ServerConfig, client *http.Client) *HTTPClient {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	byName := make(map[string]ServerConfig, len(servers))
	for _, server := range servers {
		byName[server.Name] = server
	}
	return &HTTPClient{servers: byName, client: client}
}

// Discover runs tools/list for one configured HTTP server.
func (c *HTTPClient) Discover(ctx context.Context, serverName string) (DiscoveredServer, error) {
	server, err := c.server(serverName)
	if err != nil {
		return DiscoveredServer{}, err
	}

	session, err := c.initialize(ctx, server)
	if err != nil {
		return DiscoveredServer{}, err
	}
	if err := c.notifyInitialized(ctx, server, session); err != nil {
		return DiscoveredServer{}, err
	}

	result, err := c.call(ctx, server, session, "tools/list", nil, 2)
	if err != nil {
		return DiscoveredServer{}, err
	}

	var parsed struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
			InputSchema any    `json:"inputSchema,omitempty"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return DiscoveredServer{}, fmt.Errorf("parse tools/list result: %w", err)
	}

	tools := make([]DiscoveredTool, 0, len(parsed.Tools))
	for _, tool := range parsed.Tools {
		tools = append(tools, DiscoveredTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}

	return DiscoveredServer{Server: server, Tools: tools}, nil
}

// CallTool runs tools/call for one configured HTTP server.
func (c *HTTPClient) CallTool(ctx context.Context, serverName, toolName string, args json.RawMessage) (string, error) {
	server, err := c.server(serverName)
	if err != nil {
		return "", err
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}

	params, err := json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return "", fmt.Errorf("marshal tools/call params: %w", err)
	}

	session, err := c.initialize(ctx, server)
	if err != nil {
		return "", err
	}
	if err := c.notifyInitialized(ctx, server, session); err != nil {
		return "", err
	}

	result, err := c.call(ctx, server, session, "tools/call", params, 2)
	if err != nil {
		return "", err
	}
	return formatCallResult(result)
}

func (c *HTTPClient) server(name string) (ServerConfig, error) {
	server, ok := c.servers[name]
	if !ok {
		return ServerConfig{}, fmt.Errorf("mcp server %q is not configured", name)
	}
	if server.Transport != "http" {
		return ServerConfig{}, fmt.Errorf("mcp server %q is not an http server", name)
	}
	return server, nil
}

func (c *HTTPClient) initialize(ctx context.Context, server ServerConfig) (sessionInfo, error) {
	params, err := json.Marshal(map[string]any{
		"protocolVersion": defaultProtocolVersion,
		"clientInfo": map[string]any{
			"name":    "faultline",
			"version": "dev",
		},
	})
	if err != nil {
		return sessionInfo{}, fmt.Errorf("marshal initialize params: %w", err)
	}

	result, headers, err := c.do(ctx, server, sessionInfo{}, "initialize", params, 1, true)
	if err != nil {
		return sessionInfo{}, err
	}
	session := sessionInfo{
		ProtocolVersion: defaultProtocolVersion,
		SessionID:       headers.Get("Mcp-Session-Id"),
	}
	var parsed struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(result, &parsed); err == nil && parsed.ProtocolVersion != "" {
		session.ProtocolVersion = parsed.ProtocolVersion
	}
	return session, nil
}

func (c *HTTPClient) notifyInitialized(ctx context.Context, server ServerConfig, session sessionInfo) error {
	_, _, err := c.do(ctx, server, session, "notifications/initialized", nil, 0, false)
	return err
}

func (c *HTTPClient) call(ctx context.Context, server ServerConfig, session sessionInfo, method string, params json.RawMessage, id int) (json.RawMessage, error) {
	result, _, err := c.do(ctx, server, session, method, params, id, true)
	return result, err
}

func (c *HTTPClient) do(ctx context.Context, server ServerConfig, session sessionInfo, method string, params json.RawMessage, id int, expectResponse bool) (json.RawMessage, http.Header, error) {
	body, err := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal json-rpc request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("create mcp request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session.ProtocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", session.ProtocolVersion)
	}
	if session.SessionID != "" {
		req.Header.Set("Mcp-Session-Id", session.SessionID)
	}
	for key, value := range server.Headers {
		req.Header.Set(key, value)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("call mcp server: %w", err)
	}
	defer resp.Body.Close()

	if !expectResponse {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, resp.Header, fmt.Errorf("mcp server returned HTTP %d", resp.StatusCode)
		}
		return nil, resp.Header, nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read mcp response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.Header, fmt.Errorf("mcp server returned HTTP %d", resp.StatusCode)
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(data, &rpcResp); err != nil {
		return nil, resp.Header, fmt.Errorf("parse json-rpc response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, resp.Header, fmt.Errorf("mcp error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, resp.Header, nil
}

func formatCallResult(result json.RawMessage) (string, error) {
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return "", fmt.Errorf("parse tools/call result: %w", err)
	}

	var texts []string
	for _, item := range parsed.Content {
		if item.Type == "text" && item.Text != "" {
			texts = append(texts, item.Text)
		}
	}
	if len(texts) > 0 {
		return strings.Join(texts, "\n"), nil
	}
	return string(result), nil
}
