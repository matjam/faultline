package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestStdioClientDiscoverSendsToolsListRequest(t *testing.T) {
	client := NewStdioClient([]ServerConfig{helperStdioServerConfig(t, "tools/list")})

	discovered, err := client.Discover(context.Background(), "local")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(discovered.Tools) != 1 {
		t.Fatalf("len(Tools) = %d, want 1", len(discovered.Tools))
	}
	if discovered.Tools[0].Name != "local_search" {
		t.Fatalf("tool name = %q, want local_search", discovered.Tools[0].Name)
	}
}

func TestStdioClientCallToolSendsToolsCallRequest(t *testing.T) {
	client := NewStdioClient([]ServerConfig{helperStdioServerConfig(t, "tools/call")})

	result, err := client.CallTool(context.Background(), "local", "local_search", json.RawMessage(`{"query":"faultline"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result != "local results" {
		t.Fatalf("result = %q, want local results", result)
	}
}

func helperStdioServerConfig(t *testing.T, wantMethod string) ServerConfig {
	t.Helper()
	return ServerConfig{
		Name:      "local",
		Transport: "stdio",
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestStdioMCPHelperProcess", "--"},
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"MCP_WANT_METHOD":        wantMethod,
		},
	}
}

func TestStdioMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	var methods []string
	for scanner.Scan() {
		var req jsonRPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		methods = append(methods, req.Method)

		switch req.Method {
		case "initialize":
			_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}` + "\n")
		case "notifications/initialized":
		case "tools/list":
			if os.Getenv("MCP_WANT_METHOD") != "tools/list" {
				t.Fatalf("unexpected tools/list for %q", os.Getenv("MCP_WANT_METHOD"))
			}
			_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"local_search","description":"Search locally."}]}}` + "\n")
		case "tools/call":
			if os.Getenv("MCP_WANT_METHOD") != "tools/call" {
				t.Fatalf("unexpected tools/call for %q", os.Getenv("MCP_WANT_METHOD"))
			}
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatalf("Unmarshal params: %v", err)
			}
			if params.Name != "local_search" {
				t.Fatalf("name = %q, want local_search", params.Name)
			}
			if string(params.Arguments) != `{"query":"faultline"}` {
				t.Fatalf("arguments = %s, want original arguments", params.Arguments)
			}
			_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"local results"}]}}` + "\n")
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("Scan stdin: %v", err)
	}
	want := []string{"initialize", "notifications/initialized", os.Getenv("MCP_WANT_METHOD")}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
	os.Exit(0)
}
