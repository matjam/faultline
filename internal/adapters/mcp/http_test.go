package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPClientDiscoverSendsToolsListRequest(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Accept"); !strings.Contains(got, "application/json") || !strings.Contains(got, "text/event-stream") {
			t.Fatalf("Accept = %q, want application/json and text/event-stream", got)
		}

		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		methods = append(methods, req.Method)

		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-123")
			_, _ = w.Write([]byte(`{
				"jsonrpc": "2.0",
				"id": 1,
				"result": {"protocolVersion": "2025-06-18"}
			}`))
		case "notifications/initialized":
			if got := r.Header.Get("MCP-Protocol-Version"); got != "2025-06-18" {
				t.Fatalf("MCP-Protocol-Version = %q, want 2025-06-18", got)
			}
			if got := r.Header.Get("Mcp-Session-Id"); got != "session-123" {
				t.Fatalf("Mcp-Session-Id = %q, want session-123", got)
			}
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if got := r.Header.Get("MCP-Protocol-Version"); got != "2025-06-18" {
				t.Fatalf("MCP-Protocol-Version = %q, want 2025-06-18", got)
			}
			if got := r.Header.Get("Mcp-Session-Id"); got != "session-123" {
				t.Fatalf("Mcp-Session-Id = %q, want session-123", got)
			}
			_, _ = w.Write([]byte(`{
				"jsonrpc": "2.0",
				"id": 2,
				"result": {
					"tools": [
						{
							"name": "search_repositories",
							"description": "Search repositories.",
							"inputSchema": {"type": "object"}
						}
					]
				}
			}`))
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	client := NewHTTPClient([]ServerConfig{
		{
			Name:      "github",
			Transport: "http",
			URL:       server.URL,
		},
	}, server.Client())

	discovered, err := client.Discover(context.Background(), "github")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if discovered.Server.Name != "github" {
		t.Fatalf("Server.Name = %q, want github", discovered.Server.Name)
	}
	if len(discovered.Tools) != 1 {
		t.Fatalf("len(Tools) = %d, want 1", len(discovered.Tools))
	}
	if discovered.Tools[0].Name != "search_repositories" {
		t.Fatalf("tool name = %q, want search_repositories", discovered.Tools[0].Name)
	}
	want := []string{"initialize", "notifications/initialized", "tools/list"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
}

func TestHTTPClientCallToolSendsToolsCallRequest(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		methods = append(methods, req.Method)

		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-123")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`))
			return
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/call":
			if got := r.Header.Get("MCP-Protocol-Version"); got != "2025-06-18" {
				t.Fatalf("MCP-Protocol-Version = %q, want 2025-06-18", got)
			}
			if got := r.Header.Get("Mcp-Session-Id"); got != "session-123" {
				t.Fatalf("Mcp-Session-Id = %q, want session-123", got)
			}
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}

		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Fatalf("Unmarshal params: %v", err)
		}
		if params.Name != "search_repositories" {
			t.Fatalf("params.name = %q, want search_repositories", params.Name)
		}
		if string(params.Arguments) != `{"query":"faultline"}` {
			t.Fatalf("params.arguments = %s, want original arguments", params.Arguments)
		}

		_, _ = w.Write([]byte(`{
			"jsonrpc": "2.0",
			"id": 1,
			"result": {
				"content": [
					{"type": "text", "text": "repository results"}
				]
			}
		}`))
	}))
	defer server.Close()

	client := NewHTTPClient([]ServerConfig{
		{
			Name:      "github",
			Transport: "http",
			URL:       server.URL,
		},
	}, server.Client())

	result, err := client.CallTool(context.Background(), "github", "search_repositories", json.RawMessage(`{"query":"faultline"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result != "repository results" {
		t.Fatalf("result = %q, want repository results", result)
	}
	want := []string{"initialize", "notifications/initialized", "tools/call"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
}
