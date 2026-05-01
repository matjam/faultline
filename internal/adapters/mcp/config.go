// Package mcp contains MCP server configuration and client adapters.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/matjam/faultline/internal/llm"
)

// Config is the dedicated MCP server config file shape.
type Config struct {
	Servers []ServerConfig `json:"servers"`
}

// Caller invokes an MCP tool on a configured server.
type Caller interface {
	CallTool(ctx context.Context, serverName, toolName string, args json.RawMessage) (string, error)
}

// Validate checks every configured server.
func (c Config) Validate() error {
	if c.Servers == nil {
		return fmt.Errorf("servers must be an array; use [] when no MCP servers are configured")
	}

	names := make(map[string]struct{}, len(c.Servers))
	for i, server := range c.Servers {
		if err := server.Validate(); err != nil {
			return fmt.Errorf("server %d: %w", i, err)
		}
		if _, exists := names[server.Name]; exists {
			return fmt.Errorf("server %d: duplicate name %q", i, server.Name)
		}
		names[server.Name] = struct{}{}
	}
	return nil
}

// ServerConfig describes one MCP server. Tools are callable only when their
// exact MCP tool name is listed in AllowTools.
type ServerConfig struct {
	Name       string            `json:"name"`
	Transport  string            `json:"transport"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	URL        string            `json:"url,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	AllowTools []string          `json:"allow_tools,omitempty"`
	DenyTools  []string          `json:"deny_tools,omitempty"`
}

// UnmarshalJSON accepts both this package's "env" field and the common MCP
// client config spelling "environment".
func (c *ServerConfig) UnmarshalJSON(data []byte) error {
	type serverConfigAlias ServerConfig
	var raw struct {
		serverConfigAlias
		Environment map[string]string `json:"environment,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	*c = ServerConfig(raw.serverConfigAlias)
	if c.Env == nil && raw.Environment != nil {
		c.Env = raw.Environment
	}
	return nil
}

// ServerStatus is safe to expose to the LLM or collaborator. It reports shape
// and allowlist state without carrying command, URL, env values, or headers.
type ServerStatus struct {
	Name              string   `json:"name"`
	Transport         string   `json:"transport"`
	AllowTools        []string `json:"allow_tools,omitempty"`
	CommandConfigured bool     `json:"command_configured,omitempty"`
	URLConfigured     bool     `json:"url_configured,omitempty"`
	EnvKeys           []string `json:"env_keys,omitempty"`
	HeaderKeys        []string `json:"header_keys,omitempty"`
}

// DiscoveredTool is metadata returned by MCP tools/list.
type DiscoveredTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}

// DiscoveredServer pairs a configured server with its discovered tool list.
type DiscoveredServer struct {
	Server ServerConfig     `json:"server"`
	Tools  []DiscoveredTool `json:"tools"`
}

// ResolvedTool maps a Faultline-prefixed tool name back to its MCP origin.
type ResolvedTool struct {
	ServerName string
	ToolName   string
}

// DiscoveryStatus is safe discovery output. It includes unallowlisted tools
// for review, but marks them unavailable for ordinary tool calls.
type DiscoveryStatus struct {
	Server ServerStatus           `json:"server"`
	Tools  []DiscoveredToolStatus `json:"tools"`
}

// DiscoveredToolStatus is one tool in status/discovery output.
type DiscoveredToolStatus struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Allowed     bool   `json:"allowed"`
}

// Validate rejects unsupported config shapes. Empty AllowTools is valid and
// means discovery-only: no ordinary MCP tools are callable.
func (c ServerConfig) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(c.DenyTools) > 0 {
		return fmt.Errorf("deny_tools is not supported; use allow_tools only")
	}

	switch c.Transport {
	case "stdio":
		if c.Command == "" {
			return fmt.Errorf("command is required for stdio transport")
		}
	case "http":
		if c.URL == "" {
			return fmt.Errorf("url is required for http transport")
		}
	default:
		return fmt.Errorf("unsupported transport %q", c.Transport)
	}

	return nil
}

// AllowedTool reports whether name is exactly present in allow_tools.
func (c ServerConfig) AllowedTool(name string) bool {
	for _, allowed := range c.AllowTools {
		if allowed == name {
			return true
		}
	}
	return false
}

// Status returns a redacted server summary.
func (c ServerConfig) Status() ServerStatus {
	return ServerStatus{
		Name:              c.Name,
		Transport:         c.Transport,
		AllowTools:        append([]string(nil), c.AllowTools...),
		CommandConfigured: c.Command != "",
		URLConfigured:     c.URL != "",
		EnvKeys:           sortedKeys(c.Env),
		HeaderKeys:        sortedKeys(c.Headers),
	}
}

// Status returns redacted discovery output, including tools that are not
// allowlisted so the collaborator can decide whether to enable them.
func (d DiscoveredServer) Status() DiscoveryStatus {
	tools := make([]DiscoveredToolStatus, 0, len(d.Tools))
	for _, tool := range d.Tools {
		tools = append(tools, DiscoveredToolStatus{
			Name:        tool.Name,
			Description: tool.Description,
			Allowed:     d.Server.AllowedTool(tool.Name),
		})
	}
	return DiscoveryStatus{
		Server: d.Server.Status(),
		Tools:  tools,
	}
}

// ToolDefs converts discovered MCP metadata into callable Faultline tools.
// Only exact allow_tools matches are exposed; unallowlisted tools remain
// visible through discovery/status output but are not callable.
func ToolDefs(discovered []DiscoveredServer) []llm.Tool {
	var defs []llm.Tool
	for _, server := range discovered {
		for _, tool := range server.Tools {
			if !server.Server.AllowedTool(tool.Name) {
				continue
			}
			defs = append(defs, llm.Tool{
				Type: llm.ToolTypeFunction,
				Function: &llm.FunctionDef{
					Name:        toolDefName(server.Server.Name, tool.Name),
					Description: tool.Description,
					Parameters:  tool.InputSchema,
				},
			})
		}
	}
	return defs
}

// ResolveToolName maps a Faultline-prefixed MCP tool name back to the original
// server and MCP tool name, but only when it corresponds to an allowlisted
// discovered tool.
func ResolveToolName(discovered []DiscoveredServer, name string) (ResolvedTool, bool) {
	for _, server := range discovered {
		for _, tool := range server.Tools {
			if !server.Server.AllowedTool(tool.Name) {
				continue
			}
			if toolDefName(server.Server.Name, tool.Name) == name {
				return ResolvedTool{
					ServerName: server.Server.Name,
					ToolName:   tool.Name,
				}, true
			}
		}
	}
	return ResolvedTool{}, false
}

func toolDefName(serverName, toolName string) string {
	return "mcp_" + serverName + "_" + toolName
}

// LoadConfig reads and validates a dedicated MCP JSON config file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := Config{Servers: []ServerConfig{}}
			if err := SaveConfig(path, cfg); err != nil {
				return Config{}, err
			}
			return cfg, nil
		}
		return Config{}, fmt.Errorf("read mcp config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse mcp config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate mcp config: %w", err)
	}
	return cfg, nil
}

// SaveConfig validates and writes a dedicated MCP JSON config file atomically.
func SaveConfig(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate mcp config: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create mcp config dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".mcp-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp mcp config: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp mcp config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write mcp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync mcp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close mcp config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename mcp config: %w", err)
	}

	return nil
}

func sortedKeys(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
