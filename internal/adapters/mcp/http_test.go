package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
			var params struct {
				Capabilities map[string]any `json:"capabilities"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatalf("Unmarshal initialize params: %v", err)
			}
			if params.Capabilities == nil {
				t.Fatal("initialize params missing capabilities")
			}
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
			"id": 2,
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

func TestHTTPClientParsesStreamableHTTPSSEResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-123")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`event: message
data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"message":"working"}}

event: message
data: {"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"streamed result"}]}}

`))
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	client := NewHTTPClient([]ServerConfig{
		{Name: "github", Transport: "http", URL: server.URL},
	}, server.Client())

	result, err := client.CallTool(context.Background(), "github", "search_repositories", json.RawMessage(`{"query":"faultline"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result != "streamed result" {
		t.Fatalf("result = %q, want streamed result", result)
	}
}

func TestHTTPClientParsesLargeStreamableHTTPSSEDataLine(t *testing.T) {
	largeText := strings.Repeat("x", 1024*1024+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-123")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n\n", largeText)
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	client := NewHTTPClient([]ServerConfig{
		{Name: "github", Transport: "http", URL: server.URL},
	}, server.Client())

	result, err := client.CallTool(context.Background(), "github", "search_repositories", json.RawMessage(`{"query":"faultline"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result != largeText {
		t.Fatalf("result length = %d, want %d", len(result), len(largeText))
	}
}

func TestHTTPClientReusesStreamableHTTPSession(t *testing.T) {
	var methods []string
	var callCount int
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
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if got := r.Header.Get("Mcp-Session-Id"); got != "session-123" {
				t.Fatalf("Mcp-Session-Id = %q, want session-123", got)
			}
			callCount++
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"call %d"}]}}`, req.ID, callCount)
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	client := NewHTTPClient([]ServerConfig{
		{Name: "github", Transport: "http", URL: server.URL},
	}, server.Client())

	first, err := client.CallTool(context.Background(), "github", "search_repositories", json.RawMessage(`{"query":"one"}`))
	if err != nil {
		t.Fatalf("first CallTool: %v", err)
	}
	second, err := client.CallTool(context.Background(), "github", "search_repositories", json.RawMessage(`{"query":"two"}`))
	if err != nil {
		t.Fatalf("second CallTool: %v", err)
	}
	if first != "call 1" || second != "call 2" {
		t.Fatalf("results = %q, %q; want call 1, call 2", first, second)
	}
	want := []string{"initialize", "notifications/initialized", "tools/call", "tools/call"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
}

func TestHTTPClientReinitializesWhenSessionExpires(t *testing.T) {
	var initializeCount int
	var expired bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		switch req.Method {
		case "initialize":
			initializeCount++
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", initializeCount))
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if !expired {
				expired = true
				http.NotFound(w, r)
				return
			}
			if got := r.Header.Get("Mcp-Session-Id"); got != "session-2" {
				t.Fatalf("Mcp-Session-Id = %q, want session-2", got)
			}
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"fresh result"}]}}`, req.ID)
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	client := NewHTTPClient([]ServerConfig{
		{Name: "github", Transport: "http", URL: server.URL},
	}, server.Client())

	result, err := client.CallTool(context.Background(), "github", "search_repositories", json.RawMessage(`{"query":"faultline"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result != "fresh result" {
		t.Fatalf("result = %q, want fresh result", result)
	}
	if initializeCount != 2 {
		t.Fatalf("initializeCount = %d, want 2", initializeCount)
	}
}

func TestHTTPClientCloseDeletesStreamableHTTPSession(t *testing.T) {
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if got := r.Header.Get("Mcp-Session-Id"); got != "session-123" {
				t.Fatalf("Mcp-Session-Id = %q, want session-123", got)
			}
			deleted = true
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-123")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`))
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	client := NewHTTPClient([]ServerConfig{
		{Name: "github", Transport: "http", URL: server.URL},
	}, server.Client())
	if _, err := client.Discover(context.Background(), "github"); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !deleted {
		t.Fatal("expected Close to DELETE the server session")
	}
}
