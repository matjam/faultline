package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
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
	if primaryNames["mcp_oauth_start"] {
		t.Fatal("expected OAuth tools to stay hidden when OAuth manager is absent")
	}

	oauthWithoutOAuthServers := New(Deps{
		Logger:        silentTestLogger(),
		MCPDiscovered: testMCPDiscovered(),
		MCPOAuth:      testOAuthManager(t),
	})
	oauthNames := toolDefNames(oauthWithoutOAuthServers.ToolDefs())
	if !oauthNames["mcp_oauth_start"] {
		t.Fatal("expected OAuth-enabled primary to advertise mcp_oauth_start even before an OAuth server is configured")
	}
	if !oauthNames["mcp_oauth_status"] {
		t.Fatal("expected OAuth-enabled primary to advertise mcp_oauth_status even before an OAuth server is configured")
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
	if subagentNames["mcp_oauth_start"] {
		t.Fatal("expected subagent not to advertise OAuth management tools")
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

func TestExecuteMCPDiscoverToolsRefreshesDiscoveryWhenReloadConfigured(t *testing.T) {
	oldCaller := &fakeMCPCaller{}
	newCaller := &fakeMCPCaller{}
	reloads := 0
	te := New(Deps{
		Logger:    silentTestLogger(),
		MCPCaller: oldCaller,
		MCPDiscovered: []mcp.DiscoveredServer{
			{
				Server:         mcp.ServerConfig{Name: "coralogix", Transport: "http", URL: "https://example.com/mcp"},
				DiscoveryError: `oauth credentials for "coralogix" need authorization`,
			},
		},
		MCPReload: func(context.Context) (mcp.Caller, []mcp.DiscoveredServer, error) {
			reloads++
			return newCaller, []mcp.DiscoveredServer{
				{
					Server: mcp.ServerConfig{Name: "coralogix", Transport: "http", URL: "https://example.com/mcp"},
					Tools:  []mcp.DiscoveredTool{{Name: "query"}},
				},
			}, nil
		},
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "mcp_discover_tools"},
	})

	if reloads != 1 {
		t.Fatalf("reloads = %d, want 1", reloads)
	}
	var statuses []mcp.DiscoveryStatus
	if err := json.Unmarshal([]byte(got), &statuses); err != nil {
		t.Fatalf("mcp_discover_tools returned invalid JSON: %v\n%s", err, got)
	}
	if len(statuses) != 1 || statuses[0].DiscoveryError != "" {
		t.Fatalf("statuses = %#v, want refreshed success without stale error", statuses)
	}
	if len(statuses[0].Tools) != 1 || statuses[0].Tools[0].Name != "query" {
		t.Fatalf("statuses = %#v, want refreshed query tool", statuses)
	}
	if te.mcpCaller != newCaller {
		t.Fatal("expected mcp caller to be replaced after refresh")
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
	path := filepath.Join(t.TempDir(), "mcp.json")
	freshServer := mcp.ServerConfig{
		Name:       "local",
		Transport:  "stdio",
		Command:    "local-mcp",
		AllowTools: []string{"new_search"},
	}
	if err := mcp.SaveConfig(path, mcp.Config{Servers: []mcp.ServerConfig{freshServer}}); err != nil {
		t.Fatal(err)
	}
	caller := &fakeRestartMCPCaller{
		discovered: mcp.DiscoveredServer{
			Server: freshServer,
			Tools:  []mcp.DiscoveredTool{{Name: "new_search"}},
		},
	}
	te := New(Deps{
		Logger:        silentTestLogger(),
		MCPCaller:     caller,
		MCPConfigFile: path,
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
	if caller.server.AllowTools[0] != "new_search" {
		t.Fatalf("restart used stale config: allow_tools = %#v", caller.server.AllowTools)
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
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := mcp.SaveConfig(path, mcp.Config{Servers: []mcp.ServerConfig{
		{Name: "remote", Transport: "http", URL: "https://example.invalid/mcp"},
	}}); err != nil {
		t.Fatal(err)
	}
	te := New(Deps{
		Logger:        silentTestLogger(),
		MCPCaller:     &fakeRestartMCPCaller{},
		MCPConfigFile: path,
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

func TestExecuteMCPOAuthStartReturnsSafeAuthorizationURL(t *testing.T) {
	te := New(Deps{
		Logger:        silentTestLogger(),
		MCPDiscovered: testOAuthMCPDiscovered(),
		MCPOAuth:      testOAuthManager(t),
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_oauth_start",
			Arguments: `{"server_name":"coralogix"}`,
		},
	})

	for _, secret := range []string{"access_token", "refresh_token", "authorization_code", "pkce", "verifier", "client_secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("OAuth start leaked %q in %s", secret, got)
		}
	}
	if !strings.Contains(got, "https://auth.example.com/authorize") {
		t.Fatalf("OAuth start did not return authorization URL: %s", got)
	}
	if !strings.Contains(got, "state=") {
		t.Fatalf("OAuth start did not include state in URL: %s", got)
	}
}

func TestExecuteMCPOAuthStartSuggestsMinimalAuthBlockWhenMissing(t *testing.T) {
	te := New(Deps{
		Logger: silentTestLogger(),
		MCPDiscovered: []mcp.DiscoveredServer{
			{
				Server: mcp.ServerConfig{
					Name:      "coralogix",
					Transport: "http",
					URL:       "https://api.eu1.coralogix.com/mgmt/api/v1/mcp",
				},
			},
		},
		MCPOAuth: testOAuthManager(t),
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_oauth_start",
			Arguments: `{"server_name":"coralogix"}`,
		},
	})

	for _, want := range []string{
		`"auth":{"type":"oauth_authorization_code","credential_ref":"mcp/coralogix"}`,
		"mcp_read_config",
		"mcp_propose_config_update",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("OAuth start guidance missing %q:\n%s", want, got)
		}
	}
}

func TestExecuteMCPOAuthStartRefreshesConfigBeforeBuildingURL(t *testing.T) {
	staleServer := mcp.ServerConfig{
		Name:      "slack",
		Transport: "http",
		URL:       "https://mcp.slack.com/mcp",
		Auth: &mcp.AuthConfig{
			Type:             "oauth_authorization_code",
			CredentialRef:    "mcp/slack",
			AuthorizationURL: "https://slack.com/oauth/v2_user/authorize",
			TokenURL:         "https://slack.com/api/oauth.v2.access",
			ClientID:         "client-id",
			Scopes:           []string{"search:read.public"},
		},
	}
	updatedServer := staleServer
	updatedServer.Auth = &mcp.AuthConfig{
		Type:             "oauth_authorization_code",
		CredentialRef:    "mcp/slack",
		AuthorizationURL: "https://slack.com/oauth/v2_user/authorize",
		TokenURL:         "https://slack.com/api/oauth.v2.access",
		ClientID:         "client-id",
		Scopes:           []string{"channels:history", "users:read"},
	}
	manager := mcp.NewOAuthManager(
		[]mcp.ServerConfig{staleServer},
		mcp.OAuthOptions{PublicBaseURL: "http://127.0.0.1:8745"},
		mcp.NewFileCredentialStore(filepath.Join(t.TempDir(), "oauth-tokens.json")),
		nil,
	)
	te := New(Deps{
		Logger:        silentTestLogger(),
		MCPDiscovered: []mcp.DiscoveredServer{{Server: staleServer}},
		MCPOAuth:      manager,
		MCPReload: func(context.Context) (mcp.Caller, []mcp.DiscoveredServer, error) {
			manager.SetServers([]mcp.ServerConfig{updatedServer})
			return &fakeMCPCaller{}, []mcp.DiscoveredServer{{Server: updatedServer}}, nil
		},
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_oauth_start",
			Arguments: `{"server_name":"slack"}`,
		},
	})

	if strings.Contains(got, "search%3Aread.public") {
		t.Fatalf("OAuth start used stale scopes: %s", got)
	}
	if !strings.Contains(got, "channels%3Ahistory+users%3Aread") {
		t.Fatalf("OAuth start did not use refreshed configured scopes: %s", got)
	}
}

func TestExecuteMCPOAuthStatusReturnsSafeStatus(t *testing.T) {
	manager := testOAuthManager(t)
	te := New(Deps{
		Logger:        silentTestLogger(),
		MCPDiscovered: testOAuthMCPDiscovered(),
		MCPOAuth:      manager,
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_oauth_status",
			Arguments: `{"server_name":"coralogix"}`,
		},
	})

	if !strings.Contains(got, "needs_authorization") {
		t.Fatalf("OAuth status = %s, want needs_authorization", got)
	}
	for _, secret := range []string{"access_token", "refresh_token", "client_secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("OAuth status leaked %q in %s", secret, got)
		}
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

func testOAuthMCPDiscovered() []mcp.DiscoveredServer {
	return []mcp.DiscoveredServer{
		{
			Server: mcp.ServerConfig{
				Name:      "coralogix",
				Transport: "http",
				URL:       "https://api.eu1.coralogix.com/mgmt/api/v1/mcp",
				Auth: &mcp.AuthConfig{
					Type:             "oauth_authorization_code",
					CredentialRef:    "mcp/coralogix",
					AuthorizationURL: "https://auth.example.com/authorize",
					TokenURL:         "https://auth.example.com/token",
					ClientID:         "faultline",
				},
			},
		},
	}
}

func testOAuthManager(t *testing.T) *mcp.OAuthManager {
	t.Helper()
	return mcp.NewOAuthManager(
		[]mcp.ServerConfig{testOAuthMCPDiscovered()[0].Server},
		mcp.OAuthOptions{PublicBaseURL: "https://faultline.example.com"},
		mcp.NewFileCredentialStore(filepath.Join(t.TempDir(), "oauth-tokens.json")),
		nil,
	)
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
	server     mcp.ServerConfig
}

func (f *fakeRestartMCPCaller) CallTool(context.Context, string, string, json.RawMessage) (string, error) {
	return "", nil
}

func (f *fakeRestartMCPCaller) RestartStdioServerWithConfig(_ context.Context, server mcp.ServerConfig) (mcp.DiscoveredServer, error) {
	f.serverName = server.Name
	f.server = server
	return f.discovered, nil
}
