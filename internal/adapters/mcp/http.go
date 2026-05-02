package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTPClient is the Streamable-HTTP-shaped MCP client. It implements Caller.
type HTTPClient struct {
	servers map[string]ServerConfig
	client  *http.Client

	mu       sync.Mutex
	sessions map[string]sessionInfo
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
	return &HTTPClient{servers: byName, client: client, sessions: make(map[string]sessionInfo)}
}

// Discover runs tools/list for one configured HTTP server.
func (c *HTTPClient) Discover(ctx context.Context, serverName string) (DiscoveredServer, error) {
	server, err := c.server(serverName)
	if err != nil {
		return DiscoveredServer{}, err
	}

	session, err := c.session(ctx, server)
	if err != nil {
		return DiscoveredServer{}, err
	}

	result, err := c.call(ctx, server, session, "tools/list", nil, 2)
	if isSessionExpired(err) {
		c.clearSession(server.Name)
		session, err = c.session(ctx, server)
		if err != nil {
			return DiscoveredServer{}, err
		}
		result, err = c.call(ctx, server, session, "tools/list", nil, 2)
	}
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

	session, err := c.session(ctx, server)
	if err != nil {
		return "", err
	}

	result, err := c.call(ctx, server, session, "tools/call", params, 2)
	if isSessionExpired(err) {
		c.clearSession(server.Name)
		session, err = c.session(ctx, server)
		if err != nil {
			return "", err
		}
		result, err = c.call(ctx, server, session, "tools/call", params, 2)
	}
	if err != nil {
		return "", err
	}
	return formatCallResult(result)
}

// Close terminates any stateful Streamable HTTP sessions that the server
// assigned during initialization.
func (c *HTTPClient) Close() error {
	c.mu.Lock()
	sessions := make(map[string]sessionInfo, len(c.sessions))
	for name, session := range c.sessions {
		sessions[name] = session
	}
	c.sessions = make(map[string]sessionInfo)
	c.mu.Unlock()

	var firstErr error
	for name, session := range sessions {
		if session.SessionID == "" {
			continue
		}
		server, ok := c.servers[name]
		if !ok {
			continue
		}
		if err := c.deleteSession(context.Background(), server, session); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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

func (c *HTTPClient) session(ctx context.Context, server ServerConfig) (sessionInfo, error) {
	c.mu.Lock()
	if session, ok := c.sessions[server.Name]; ok {
		c.mu.Unlock()
		return session, nil
	}
	c.mu.Unlock()

	session, err := c.initialize(ctx, server)
	if err != nil {
		return sessionInfo{}, err
	}
	if err := c.notifyInitialized(ctx, server, session); err != nil {
		return sessionInfo{}, err
	}

	c.mu.Lock()
	c.sessions[server.Name] = session
	c.mu.Unlock()
	return session, nil
}

func (c *HTTPClient) clearSession(serverName string) {
	c.mu.Lock()
	delete(c.sessions, serverName)
	c.mu.Unlock()
}

func (c *HTTPClient) initialize(ctx context.Context, server ServerConfig) (sessionInfo, error) {
	params, err := json.Marshal(map[string]any{
		"protocolVersion": defaultProtocolVersion,
		"capabilities":    map[string]any{},
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

func (c *HTTPClient) deleteSession(ctx context.Context, server ServerConfig, session sessionInfo) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, server.URL, nil)
	if err != nil {
		return fmt.Errorf("create mcp delete request: %w", err)
	}
	if session.ProtocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", session.ProtocolVersion)
	}
	req.Header.Set("Mcp-Session-Id", session.SessionID)
	for key, value := range server.Headers {
		req.Header.Set(key, value)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("delete mcp session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mcp server returned HTTP %d", resp.StatusCode)
	}
	return nil
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

	if resp.StatusCode != http.StatusOK {
		return nil, resp.Header, httpStatusError{status: resp.StatusCode}
	}

	result, err := readHTTPResponse(resp.Body, resp.Header.Get("Content-Type"), id)
	if err != nil {
		return nil, resp.Header, err
	}
	return result, resp.Header, nil
}

type httpStatusError struct {
	status int
}

func (e httpStatusError) Error() string {
	return fmt.Sprintf("mcp server returned HTTP %d", e.status)
}

func isSessionExpired(err error) bool {
	var statusErr httpStatusError
	return errors.As(err, &statusErr) && statusErr.status == http.StatusNotFound
}

func readHTTPResponse(body io.Reader, contentType string, id int) (json.RawMessage, error) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return readSSEResponse(body, id)
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read mcp response: %w", err)
	}
	return parseJSONRPCResponse(data, id)
}

func parseJSONRPCResponse(data []byte, id int) (json.RawMessage, error) {
	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(data, &rpcResp); err != nil {
		return nil, fmt.Errorf("parse json-rpc response: %w", err)
	}
	if id != 0 && rpcResp.ID != id {
		return nil, fmt.Errorf("mcp response id = %d, want %d", rpcResp.ID, id)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("mcp error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

func readSSEResponse(body io.Reader, id int) (json.RawMessage, error) {
	reader := bufio.NewReader(body)
	var dataLines []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read mcp sse response: %w", err)
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			result, done, err := parseSSEEventData(dataLines, id)
			if err != nil || done {
				return result, err
			}
			dataLines = nil
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if err == io.EOF {
			break
		}
	}
	if len(dataLines) > 0 {
		result, done, err := parseSSEEventData(dataLines, id)
		if err != nil || done {
			return result, err
		}
	}
	return nil, fmt.Errorf("mcp sse stream ended before response id %d", id)
}

func parseSSEEventData(dataLines []string, id int) (json.RawMessage, bool, error) {
	if len(dataLines) == 0 {
		return nil, false, nil
	}
	data := []byte(strings.Join(dataLines, "\n"))
	var probe jsonRPCResponse
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, false, fmt.Errorf("parse sse json-rpc response: %w", err)
	}
	if probe.ID != id {
		return nil, false, nil
	}
	result, err := parseJSONRPCResponse(data, id)
	return result, true, err
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
