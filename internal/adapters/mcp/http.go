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
	auth    OAuthTokenProvider

	mu       sync.Mutex
	sessions map[string]sessionInfo
}

// OAuthTokenProvider supplies and refreshes access tokens for OAuth-backed
// HTTP MCP servers.
type OAuthTokenProvider interface {
	AccessToken(ctx context.Context, serverName string) (string, error)
	RefreshAccessToken(ctx context.Context, serverName string) (string, error)
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
	return NewHTTPClientWithAuth(servers, client, nil)
}

// NewHTTPClientWithAuth returns a client for HTTP MCP servers with optional
// OAuth token support.
func NewHTTPClientWithAuth(servers []ServerConfig, client *http.Client, auth OAuthTokenProvider) *HTTPClient {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	byName := make(map[string]ServerConfig, len(servers))
	for _, server := range servers {
		byName[server.Name] = server
	}
	return &HTTPClient{servers: byName, client: client, auth: auth, sessions: make(map[string]sessionInfo)}
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
	defer c.mu.Unlock()

	session, err := c.initialize(ctx, server)
	if err != nil {
		return sessionInfo{}, err
	}
	if err := c.notifyInitialized(ctx, server, session); err != nil {
		return sessionInfo{}, err
	}

	c.sessions[server.Name] = session
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
	if err := c.authorize(ctx, server, req); err != nil {
		return err
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

	resp, err := c.doOnce(ctx, server, session, body, false)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && c.auth != nil && server.Auth != nil {
		_ = resp.Body.Close()
		c.clearSession(server.Name)
		if _, err := c.auth.RefreshAccessToken(ctx, server.Name); err != nil {
			return nil, nil, err
		}
		resp, err = c.doOnce(ctx, server, session, body, true)
		if err != nil {
			return nil, nil, err
		}
	}
	defer resp.Body.Close()

	if !expectResponse {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, resp.Header, newHTTPStatusError(server, resp.StatusCode, resp.Header)
		}
		return nil, resp.Header, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, resp.Header, newHTTPStatusError(server, resp.StatusCode, resp.Header)
	}

	result, err := readHTTPResponse(resp.Body, resp.Header.Get("Content-Type"), id)
	if err != nil {
		return nil, resp.Header, err
	}
	return result, resp.Header, nil
}

func (c *HTTPClient) doOnce(ctx context.Context, server ServerConfig, session sessionInfo, body []byte, refreshed bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create mcp request: %w", err)
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
	if err := c.authorizeWithMode(ctx, server, req, refreshed); err != nil {
		return nil, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call mcp server: %w", err)
	}
	return resp, nil
}

func (c *HTTPClient) authorize(ctx context.Context, server ServerConfig, req *http.Request) error {
	return c.authorizeWithMode(ctx, server, req, false)
}

func (c *HTTPClient) authorizeWithMode(ctx context.Context, server ServerConfig, req *http.Request, refreshed bool) error {
	if c.auth == nil || server.Auth == nil {
		return nil
	}
	token, err := c.auth.AccessToken(ctx, server.Name)
	if err != nil {
		return err
	}
	if refreshed {
		token, err = c.auth.AccessToken(ctx, server.Name)
		if err != nil {
			return err
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

type httpStatusError struct {
	status  int
	message string
}

func (e httpStatusError) Error() string {
	if e.message != "" {
		return e.message
	}
	return fmt.Sprintf("mcp server returned HTTP %d", e.status)
}

func newHTTPStatusError(server ServerConfig, status int, headers http.Header) httpStatusError {
	return httpStatusError{status: status, message: httpStatusGuidance(server, status, headers)}
}

func httpStatusGuidance(server ServerConfig, status int, headers http.Header) string {
	base := fmt.Sprintf("mcp server returned HTTP %d", status)
	challenge := bearerChallenge(headers)
	if challenge == "" {
		return base
	}
	resourceMetadata := bearerChallengeParam(challenge, "resource_metadata")
	scope := bearerChallengeParam(challenge, "scope")
	if status == http.StatusUnauthorized {
		if server.Headers["Authorization"] != "" {
			return base + ": configured Authorization header was rejected. Check whether the bearer token is missing, invalid, expired, or for the wrong audience."
		}
		if server.Auth != nil && server.Auth.Type == "oauth_authorization_code" {
			return base + ": configured OAuth credentials were rejected. Run mcp_oauth_status for this server, then mcp_oauth_start if authorization is needed."
		}
		if resourceMetadata != "" {
			message := base + ": server likely requires OAuth. Propose an MCP config update adding " + MinimalOAuthAuthJSONWithScopes(server.Name, strings.Fields(scope)) + ", then run mcp_oauth_start for this server."
			if scope != "" {
				message += " The challenge requested scope " + scope + "."
			}
			return message
		}
		return base + ": server requires bearer authentication, but did not advertise OAuth protected-resource metadata. Configure a static Authorization header/token or provider-specific OAuth metadata before retrying."
	}
	if status == http.StatusForbidden && bearerChallengeParam(challenge, "error") == "insufficient_scope" {
		if scope != "" {
			return base + ": authenticated, but the token has insufficient scope. Required scope: " + scope + "."
		}
		return base + ": authenticated, but the token has insufficient scope."
	}
	return base
}

func bearerChallenge(headers http.Header) string {
	for _, challenge := range headers.Values("WWW-Authenticate") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(challenge)), "bearer ") {
			return challenge
		}
	}
	return ""
}

func MinimalOAuthAuthJSON(serverName string) string {
	return MinimalOAuthAuthJSONWithScopes(serverName, nil)
}

func MinimalOAuthAuthJSONWithScopes(serverName string, scopes []string) string {
	block := struct {
		Auth struct {
			Type          string   `json:"type"`
			CredentialRef string   `json:"credential_ref"`
			Scopes        []string `json:"scopes,omitempty"`
		} `json:"auth"`
	}{}
	block.Auth.Type = "oauth_authorization_code"
	block.Auth.CredentialRef = "mcp/" + serverName
	block.Auth.Scopes = scopes
	data, err := json.Marshal(block)
	if err != nil {
		return fmt.Sprintf(`"auth":{"type":"oauth_authorization_code","credential_ref":"mcp/%s"}`, serverName)
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(data), "{"), "}")
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
