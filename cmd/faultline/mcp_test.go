package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/matjam/faultline/internal/adapters/mcp"
	"github.com/matjam/faultline/internal/config"
)

func TestSetupMCPDisabled(t *testing.T) {
	caller, discovered, err := setupMCP(context.Background(), config.MCPConfig{}, nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
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
	}, nil, hostTestMCPStdioStarter{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
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

func TestSetupMCPReportsStdioDiscoveryErrorWithoutSandbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{
		"servers": [
			{
				"name": "local",
				"transport": "stdio",
				"command": "local-mcp"
			}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	caller, discovered, err := setupMCP(context.Background(), config.MCPConfig{
		Enabled:    true,
		ConfigFile: path,
	}, nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("setupMCP: %v", err)
	}
	if caller == nil {
		t.Fatal("expected caller when MCP enabled")
	}
	if len(discovered) != 1 {
		t.Fatalf("len(discovered) = %d, want 1", len(discovered))
	}
	if discovered[0].DiscoveryError == "" {
		t.Fatal("expected missing sandbox to be reported as discovery error")
	}
}

type hostTestMCPStdioStarter struct{}

func (hostTestMCPStdioStarter) Start(ctx context.Context, command mcp.StdioCommand) (mcp.StdioProcess, error) {
	cmd := exec.CommandContext(ctx, command.Command, command.Args...)
	cmd.Env = append(os.Environ(), testEnvList(command.Env)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &hostTestMCPStdioProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
	}, nil
}

type hostTestMCPStdioProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

func (p *hostTestMCPStdioProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *hostTestMCPStdioProcess) Stdout() io.Reader     { return p.stdout }
func (p *hostTestMCPStdioProcess) Stderr() io.Reader     { return p.stderr }
func (p *hostTestMCPStdioProcess) Wait() error           { return p.cmd.Wait() }

func testEnvList(values map[string]string) []string {
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func TestSetupMCPDiscoversHTTPServers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id,omitempty"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode request: %v", err)
		}
		switch req.Method {
		case "initialize":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_, _ = w.Write([]byte(`{
				"jsonrpc": "2.0",
				"id": 2,
				"result": {
					"tools": [
						{"name": "search_repositories", "description": "Search repositories."},
						{"name": "delete_repository", "description": "Delete a repository."}
					]
				}
			}`))
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
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
	}, nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
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

func TestSetupMCPKeepsServerWhenDiscoveryFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	server.Close()

	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{
		"servers": [
			{
				"name": "genie",
				"transport": "http",
				"url": "`+server.URL+`"
			}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	caller, discovered, err := setupMCP(context.Background(), config.MCPConfig{
		Enabled:    true,
		ConfigFile: path,
	}, nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("setupMCP: %v", err)
	}
	if caller == nil {
		t.Fatal("expected caller when MCP enabled")
	}
	if len(discovered) != 1 {
		t.Fatalf("len(discovered) = %d, want 1", len(discovered))
	}
	if discovered[0].Server.Name != "genie" {
		t.Fatalf("server name = %q, want genie", discovered[0].Server.Name)
	}
	if discovered[0].DiscoveryError == "" {
		t.Fatal("expected discovery error to be recorded")
	}
	if len(discovered[0].Tools) != 0 {
		t.Fatalf("len(tools) = %d, want 0", len(discovered[0].Tools))
	}
}
