package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// StdioClient is the command-based MCP client. It implements Caller.
type StdioClient struct {
	servers map[string]ServerConfig
}

// NewStdioClient returns a client for command-based MCP servers.
func NewStdioClient(servers []ServerConfig) *StdioClient {
	byName := make(map[string]ServerConfig, len(servers))
	for _, server := range servers {
		byName[server.Name] = server
	}
	return &StdioClient{servers: byName}
}

// Discover runs tools/list for one configured stdio server.
func (c *StdioClient) Discover(ctx context.Context, serverName string) (DiscoveredServer, error) {
	server, err := c.server(serverName)
	if err != nil {
		return DiscoveredServer{}, err
	}

	result, err := c.call(ctx, server, "tools/list", nil, 2)
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

// CallTool runs tools/call for one configured stdio server.
func (c *StdioClient) CallTool(ctx context.Context, serverName, toolName string, args json.RawMessage) (string, error) {
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

	result, err := c.call(ctx, server, "tools/call", params, 2)
	if err != nil {
		return "", err
	}
	return formatCallResult(result)
}

func (c *StdioClient) server(name string) (ServerConfig, error) {
	server, ok := c.servers[name]
	if !ok {
		return ServerConfig{}, fmt.Errorf("mcp server %q is not configured", name)
	}
	if server.Transport != "stdio" {
		return ServerConfig{}, fmt.Errorf("mcp server %q is not a stdio server", name)
	}
	return server, nil
}

func (c *StdioClient) call(ctx context.Context, server ServerConfig, method string, params json.RawMessage, id int) (json.RawMessage, error) {
	// Stdio MCP intentionally launches an operator-configured command from the
	// dedicated MCP config file. Agent edits to that file are gated by raw
	// collaborator approval, and arguments are passed without a shell.
	cmd := exec.CommandContext(ctx, server.Command, server.Args...) // #nosec G204 // nosemgrep
	cmd.Env = append(os.Environ(), envList(server.Env)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open mcp stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open mcp stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open mcp stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mcp server: %w", err)
	}

	reader := bufio.NewReader(stdout)
	if err := writeJSONLine(stdin, jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"` + defaultProtocolVersion + `","clientInfo":{"name":"faultline","version":"dev"}}`),
	}); err != nil {
		return nil, err
	}
	if _, err := readResponse(reader, 1); err != nil {
		return nil, err
	}

	if err := writeJSONLine(stdin, jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}); err != nil {
		return nil, err
	}

	if err := writeJSONLine(stdin, jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}); err != nil {
		return nil, err
	}
	result, err := readResponse(reader, id)
	if closeErr := stdin.Close(); closeErr != nil && err == nil {
		err = fmt.Errorf("close mcp stdin: %w", closeErr)
	}
	stderrData, _ := io.ReadAll(stderr)
	if waitErr := cmd.Wait(); waitErr != nil && err == nil {
		err = fmt.Errorf("run mcp server: %w: %s", waitErr, string(stderrData))
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

func envList(values map[string]string) []string {
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func writeJSONLine(w io.Writer, req jsonRPCRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal json-rpc request: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write mcp request: %w", err)
	}
	return nil
}

func readResponse(r *bufio.Reader, id int) (json.RawMessage, error) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read mcp response: %w", err)
		}

		var rpcResp jsonRPCResponse
		if err := json.Unmarshal(line, &rpcResp); err != nil {
			return nil, fmt.Errorf("parse json-rpc response: %w", err)
		}
		if rpcResp.ID != id {
			continue
		}
		if rpcResp.Error != nil {
			return nil, fmt.Errorf("mcp error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
		}
		return rpcResp.Result, nil
	}
}
