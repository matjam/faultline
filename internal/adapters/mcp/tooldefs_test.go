package mcp

import "testing"

func TestToolDefsExposeOnlyAllowlistedTools(t *testing.T) {
	discovered := []DiscoveredServer{
		{
			Server: ServerConfig{
				Name:       "github",
				Transport:  "stdio",
				Command:    "github-mcp",
				AllowTools: []string{"search_repositories"},
			},
			Tools: []DiscoveredTool{
				{
					Name:        "search_repositories",
					Description: "Search repositories.",
					InputSchema: map[string]any{"type": "object"},
				},
				{
					Name:        "delete_repository",
					Description: "Delete a repository.",
					InputSchema: map[string]any{"type": "object"},
				},
			},
		},
	}

	defs := ToolDefs(discovered)

	if len(defs) != 1 {
		t.Fatalf("len(ToolDefs) = %d, want 1", len(defs))
	}
	if defs[0].Function == nil {
		t.Fatal("tool definition missing function")
	}
	if defs[0].Function.Name != "mcp_github_search_repositories" {
		t.Fatalf("Function.Name = %q, want mcp_github_search_repositories", defs[0].Function.Name)
	}
	if defs[0].Function.Description != "Search repositories." {
		t.Fatalf("Description = %q, want MCP description", defs[0].Function.Description)
	}
}

func TestToolDefsSkipUnsafeOrReservedGeneratedNames(t *testing.T) {
	discovered := []DiscoveredServer{
		{
			Server: ServerConfig{
				Name:       "github",
				Transport:  "stdio",
				Command:    "github-mcp",
				AllowTools: []string{"valid_tool", "bad.tool"},
			},
			Tools: []DiscoveredTool{
				{Name: "valid_tool"},
				{Name: "bad.tool"},
			},
		},
		{
			Server: ServerConfig{
				Name:       "list",
				Transport:  "stdio",
				Command:    "list-mcp",
				AllowTools: []string{"servers"},
			},
			Tools: []DiscoveredTool{
				{Name: "servers"},
			},
		},
	}

	defs := ToolDefs(discovered)

	if len(defs) != 1 {
		t.Fatalf("len(ToolDefs) = %d, want 1", len(defs))
	}
	if defs[0].Function == nil || defs[0].Function.Name != "mcp_github_valid_tool" {
		t.Fatalf("unexpected tool definition: %#v", defs[0].Function)
	}
	if _, ok := ResolveToolName(discovered, "mcp_github_bad.tool"); ok {
		t.Fatal("expected invalid generated tool name not to resolve")
	}
	if _, ok := ResolveToolName(discovered, "mcp_list_servers"); ok {
		t.Fatal("expected reserved management tool name not to resolve")
	}
}

func TestDiscoveredServerStatusIncludesUnallowlistedTools(t *testing.T) {
	discovered := DiscoveredServer{
		Server: ServerConfig{
			Name:       "github",
			Transport:  "stdio",
			Command:    "github-mcp",
			AllowTools: []string{"search_repositories"},
		},
		Tools: []DiscoveredTool{
			{Name: "search_repositories"},
			{Name: "delete_repository"},
		},
	}

	status := discovered.Status()

	if len(status.Tools) != 2 {
		t.Fatalf("len(Tools) = %d, want 2", len(status.Tools))
	}
	if !status.Tools[0].Allowed {
		t.Fatal("expected allowlisted tool to be marked allowed")
	}
	if status.Tools[1].Allowed {
		t.Fatal("expected unallowlisted tool to be listed but not allowed")
	}
}

func TestResolveToolNameMapsAdvertisedNameToOriginalTool(t *testing.T) {
	discovered := []DiscoveredServer{
		{
			Server: ServerConfig{
				Name:       "github",
				Transport:  "stdio",
				Command:    "github-mcp",
				AllowTools: []string{"search_repositories"},
			},
			Tools: []DiscoveredTool{
				{Name: "search_repositories"},
				{Name: "delete_repository"},
			},
		},
	}

	resolved, ok := ResolveToolName(discovered, "mcp_github_search_repositories")
	if !ok {
		t.Fatal("expected advertised tool name to resolve")
	}
	if resolved.ServerName != "github" {
		t.Fatalf("ServerName = %q, want github", resolved.ServerName)
	}
	if resolved.ToolName != "search_repositories" {
		t.Fatalf("ToolName = %q, want search_repositories", resolved.ToolName)
	}

	if _, ok := ResolveToolName(discovered, "mcp_github_delete_repository"); ok {
		t.Fatal("expected unallowlisted discovered tool not to resolve")
	}
	if _, ok := ResolveToolName(discovered, "mcp_github_missing"); ok {
		t.Fatal("expected unknown MCP tool not to resolve")
	}
}
