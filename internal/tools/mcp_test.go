package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matjam/faultline/internal/adapters/mcp"
	"github.com/matjam/faultline/internal/llm"
)

func TestToolDefsAdvertisesOnlyAllowlistedMCPTools(t *testing.T) {
	te := New(Deps{
		Logger: silentTestLogger(),
		MCPDiscovered: []mcp.DiscoveredServer{
			{
				Server: mcp.ServerConfig{
					Name:       "github",
					Transport:  "stdio",
					Command:    "github-mcp",
					AllowTools: []string{"search_repositories"},
				},
				Tools: []mcp.DiscoveredTool{
					{Name: "search_repositories"},
					{Name: "delete_repository"},
				},
			},
		},
	})

	names := toolDefNames(te.ToolDefs())

	if !names["mcp_github_search_repositories"] {
		t.Fatal("expected allowlisted MCP tool to be advertised")
	}
	if names["mcp_github_delete_repository"] {
		t.Fatal("expected unallowlisted MCP tool to stay out of ToolDefs")
	}
}

func TestSubagentToolDefsIncludesOrdinaryAllowlistedMCPTools(t *testing.T) {
	te := New(Deps{
		Mode:   ModeSubagent,
		Logger: silentTestLogger(),
		MCPDiscovered: []mcp.DiscoveredServer{
			{
				Server: mcp.ServerConfig{
					Name:       "github",
					Transport:  "stdio",
					Command:    "github-mcp",
					AllowTools: []string{"search_repositories"},
				},
				Tools: []mcp.DiscoveredTool{
					{Name: "search_repositories"},
				},
			},
		},
	})

	names := toolDefNames(te.ToolDefs())

	if !names["mcp_github_search_repositories"] {
		t.Fatal("expected ordinary allowlisted MCP tool to be available to wired subagents")
	}
	if names["mcp_list_servers"] {
		t.Fatal("expected MCP management tools to be unavailable to subagents")
	}
	if names["mcp_discover_tools"] {
		t.Fatal("expected MCP discovery management tools to be unavailable to subagents")
	}
}

func TestExecuteRoutesAllowlistedMCPToolCall(t *testing.T) {
	caller := &fakeMCPCaller{result: "mcp result"}
	te := New(Deps{
		Logger:    silentTestLogger(),
		MCPCaller: caller,
		MCPDiscovered: []mcp.DiscoveredServer{
			{
				Server: mcp.ServerConfig{
					Name:       "github",
					Transport:  "stdio",
					Command:    "github-mcp",
					AllowTools: []string{"search_repositories"},
				},
				Tools: []mcp.DiscoveredTool{
					{Name: "search_repositories"},
				},
			},
		},
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_github_search_repositories",
			Arguments: `{"query":"faultline"}`,
		},
	})

	if got != "mcp result" {
		t.Fatalf("Execute = %q, want mcp result", got)
	}
	if caller.serverName != "github" {
		t.Fatalf("serverName = %q, want github", caller.serverName)
	}
	if caller.toolName != "search_repositories" {
		t.Fatalf("toolName = %q, want search_repositories", caller.toolName)
	}
	if string(caller.args) != `{"query":"faultline"}` {
		t.Fatalf("args = %s, want original arguments", caller.args)
	}
}

func TestExecuteRejectsUnallowlistedMCPToolCall(t *testing.T) {
	caller := &fakeMCPCaller{result: "should not be called"}
	te := New(Deps{
		Logger:    silentTestLogger(),
		MCPCaller: caller,
		MCPDiscovered: []mcp.DiscoveredServer{
			{
				Server: mcp.ServerConfig{
					Name:       "github",
					Transport:  "stdio",
					Command:    "github-mcp",
					AllowTools: []string{"search_repositories"},
				},
				Tools: []mcp.DiscoveredTool{
					{Name: "search_repositories"},
					{Name: "delete_repository"},
				},
			},
		},
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_github_delete_repository",
			Arguments: `{"repo":"faultline"}`,
		},
	})

	if caller.called {
		t.Fatal("unallowlisted MCP tool should not call MCP caller")
	}
	if !strings.Contains(got, "not configured or allowlisted") {
		t.Fatalf("expected defensive rejection, got %q", got)
	}
}

func TestToolDefsAdvertisesMCPManagementToolsOnlyToPrimary(t *testing.T) {
	primary := New(Deps{
		Logger:        silentTestLogger(),
		MCPDiscovered: testMCPDiscovered(),
	})
	primaryNames := toolDefNames(primary.ToolDefs())
	if !primaryNames["mcp_list_servers"] {
		t.Fatal("expected primary to advertise mcp_list_servers")
	}
	if !primaryNames["mcp_discover_tools"] {
		t.Fatal("expected primary to advertise mcp_discover_tools")
	}

	subagent := New(Deps{
		Mode:          ModeSubagent,
		Logger:        silentTestLogger(),
		MCPDiscovered: testMCPDiscovered(),
	})
	subagentNames := toolDefNames(subagent.ToolDefs())
	if subagentNames["mcp_list_servers"] {
		t.Fatal("expected subagent not to advertise mcp_list_servers")
	}
	if subagentNames["mcp_discover_tools"] {
		t.Fatal("expected subagent not to advertise mcp_discover_tools")
	}
}

func TestExecuteMCPListServersRedactsSecrets(t *testing.T) {
	te := New(Deps{
		Logger:        silentTestLogger(),
		MCPDiscovered: testMCPDiscovered(),
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "mcp_list_servers"},
	})

	for _, secret := range []string{"url-secret", "header-secret", "env-secret", "example.com"} {
		if strings.Contains(got, secret) {
			t.Fatalf("mcp_list_servers exposed %q in %s", secret, got)
		}
	}
	if !strings.Contains(got, "GITHUB_TOKEN") {
		t.Fatalf("expected env key name in redacted status, got %s", got)
	}
}

func TestExecuteMCPDiscoverToolsIncludesUnallowlistedAsDisabled(t *testing.T) {
	te := New(Deps{
		Logger:        silentTestLogger(),
		MCPDiscovered: testMCPDiscovered(),
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "mcp_discover_tools"},
	})

	var statuses []mcp.DiscoveryStatus
	if err := json.Unmarshal([]byte(got), &statuses); err != nil {
		t.Fatalf("mcp_discover_tools returned invalid JSON: %v\n%s", err, got)
	}

	tools := statuses[0].Tools
	if len(tools) != 2 {
		t.Fatalf("len(Tools) = %d, want 2", len(tools))
	}
	if tools[0].Name != "search_repositories" || !tools[0].Allowed {
		t.Fatalf("allowlisted tool status = %+v, want allowed search_repositories", tools[0])
	}
	if tools[1].Name != "delete_repository" || tools[1].Allowed {
		t.Fatalf("unallowlisted tool status = %+v, want disabled delete_repository", tools[1])
	}
}

func TestSubagentExecuteRejectsMCPManagementTools(t *testing.T) {
	te := New(Deps{
		Mode:          ModeSubagent,
		Logger:        silentTestLogger(),
		MCPDiscovered: testMCPDiscovered(),
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "mcp_list_servers"},
	})

	if !strings.Contains(strings.ToLower(got), "not available") {
		t.Fatalf("expected subagent rejection, got %q", got)
	}
}

func TestExecuteMCPRestartStdioServerUpdatesDiscovery(t *testing.T) {
	caller := &fakeRestartMCPCaller{
		discovered: mcp.DiscoveredServer{
			Server: mcp.ServerConfig{
				Name:       "local",
				Transport:  "stdio",
				Command:    "local-mcp",
				AllowTools: []string{"new_search"},
			},
			Tools: []mcp.DiscoveredTool{{Name: "new_search"}},
		},
	}
	te := New(Deps{
		Logger:    silentTestLogger(),
		MCPCaller: caller,
		MCPDiscovered: []mcp.DiscoveredServer{
			{
				Server: mcp.ServerConfig{
					Name:       "local",
					Transport:  "stdio",
					Command:    "local-mcp",
					AllowTools: []string{"old_search"},
				},
				Tools: []mcp.DiscoveredTool{{Name: "old_search"}},
			},
		},
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_restart_stdio_server",
			Arguments: `{"server_name":"local"}`,
		},
	})

	if !strings.Contains(got, "restarted") {
		t.Fatalf("expected restart success, got %q", got)
	}
	if caller.serverName != "local" {
		t.Fatalf("serverName = %q, want local", caller.serverName)
	}
	names := toolDefNames(te.ToolDefs())
	if !names["mcp_local_new_search"] {
		t.Fatal("expected restarted discovery to update ToolDefs")
	}
	if names["mcp_local_old_search"] {
		t.Fatal("expected old discovered tool to be replaced")
	}
}

func TestExecuteMCPRestartStdioServerRejectsHTTPServer(t *testing.T) {
	te := New(Deps{
		Logger:    silentTestLogger(),
		MCPCaller: &fakeRestartMCPCaller{},
		MCPDiscovered: []mcp.DiscoveredServer{
			{
				Server: mcp.ServerConfig{
					Name:      "remote",
					Transport: "http",
					URL:       "https://example.invalid/mcp",
				},
			},
		},
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_restart_stdio_server",
			Arguments: `{"server_name":"remote"}`,
		},
	})

	if !strings.Contains(got, "not a stdio server") {
		t.Fatalf("expected http rejection, got %q", got)
	}
}

func toolDefNames(defs []llm.Tool) map[string]bool {
	names := make(map[string]bool, len(defs))
	for _, def := range defs {
		if def.Function != nil {
			names[def.Function.Name] = true
		}
	}
	return names
}

func testMCPDiscovered() []mcp.DiscoveredServer {
	return []mcp.DiscoveredServer{
		{
			Server: mcp.ServerConfig{
				Name:       "github",
				Transport:  "http",
				URL:        "https://example.com/mcp?token=url-secret",
				Headers:    map[string]string{"Authorization": "Bearer header-secret"},
				Env:        map[string]string{"GITHUB_TOKEN": "env-secret"},
				AllowTools: []string{"search_repositories"},
			},
			Tools: []mcp.DiscoveredTool{
				{Name: "search_repositories"},
				{Name: "delete_repository"},
			},
		},
	}
}

type fakeMCPCaller struct {
	result     string
	called     bool
	serverName string
	toolName   string
	args       json.RawMessage
}

func (f *fakeMCPCaller) CallTool(_ context.Context, serverName, toolName string, args json.RawMessage) (string, error) {
	f.called = true
	f.serverName = serverName
	f.toolName = toolName
	f.args = append(json.RawMessage(nil), args...)
	return f.result, nil
}

type fakeRestartMCPCaller struct {
	discovered mcp.DiscoveredServer
	serverName string
}

func (f *fakeRestartMCPCaller) CallTool(context.Context, string, string, json.RawMessage) (string, error) {
	return "", nil
}

func (f *fakeRestartMCPCaller) RestartStdioServer(_ context.Context, serverName string) (mcp.DiscoveredServer, error) {
	f.serverName = serverName
	return f.discovered, nil
}
