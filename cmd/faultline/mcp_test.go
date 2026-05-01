package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/matjam/faultline/internal/config"
)

func TestSetupMCPDisabled(t *testing.T) {
	caller, discovered, err := setupMCP(context.Background(), config.MCPConfig{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("setupMCP: %v", err)
	}
	if caller != nil {
		t.Fatal("expected nil caller when MCP disabled")
	}
	if discovered != nil {
		t.Fatalf("expected nil discovered when MCP disabled, got %d", len(discovered))
	}
}

func TestSetupMCPDiscoversStdioServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{
		"servers": [
			{
				"name": "local",
				"transport": "stdio",
				"command": "`+os.Args[0]+`",
				"args": ["-test.run=TestSetupMCPStdioHelperProcess", "--"],
				"env": {"GO_WANT_SETUP_MCP_HELPER": "1"},
				"allow_tools": ["local_search"]
			}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	caller, discovered, err := setupMCP(context.Background(), config.MCPConfig{
		Enabled:    true,
		ConfigFile: path,
	}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("setupMCP: %v", err)
	}
	if caller == nil {
		t.Fatal("expected caller when MCP enabled")
	}
	if len(discovered) != 1 {
		t.Fatalf("len(discovered) = %d, want 1", len(discovered))
	}
	if discovered[0].Server.Name != "local" {
		t.Fatalf("server name = %q, want local", discovered[0].Server.Name)
	}
	if len(discovered[0].Tools) != 1 || discovered[0].Tools[0].Name != "local_search" {
		t.Fatalf("tools = %+v, want local_search", discovered[0].Tools)
	}
}

func TestSetupMCPStdioHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SETUP_MCP_HELPER") != "1" {
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     int    `json:"id,omitempty"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		switch req.Method {
		case "initialize":
			_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}` + "\n")
		case "notifications/initialized":
		case "tools/list":
			_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"local_search"}]}}` + "\n")
		default:
			t.Fatalf("method = %q, want lifecycle/tools-list", req.Method)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("Scan stdin: %v", err)
	}
	os.Exit(0)
}

func TestSetupMCPDiscoversHTTPServers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"jsonrpc": "2.0",
			"id": 1,
			"result": {
				"tools": [
					{"name": "search_repositories", "description": "Search repositories."},
					{"name": "delete_repository", "description": "Delete a repository."}
				]
			}
		}`))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{
		"servers": [
			{
				"name": "github",
				"transport": "http",
				"url": "`+server.URL+`",
				"allow_tools": ["search_repositories"]
			}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	caller, discovered, err := setupMCP(context.Background(), config.MCPConfig{
		Enabled:    true,
		ConfigFile: path,
	}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("setupMCP: %v", err)
	}
	if caller == nil {
		t.Fatal("expected caller when MCP enabled")
	}
	if len(discovered) != 1 {
		t.Fatalf("len(discovered) = %d, want 1", len(discovered))
	}
	if discovered[0].Server.Name != "github" {
		t.Fatalf("server name = %q, want github", discovered[0].Server.Name)
	}
	if len(discovered[0].Tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2", len(discovered[0].Tools))
	}
}
