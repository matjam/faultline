package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServerConfigValidateRequiresAllowlistOnly(t *testing.T) {
	cfg := ServerConfig{
		Name:       "github",
		Transport:  "stdio",
		Command:    "github-mcp",
		AllowTools: []string{"search_repositories"},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestServerConfigMissingAllowToolsAllowsNoTools(t *testing.T) {
	cfg := ServerConfig{
		Name:      "github",
		Transport: "stdio",
		Command:   "github-mcp",
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.AllowedTool("search_repositories") {
		t.Fatal("expected missing allow_tools to allow no tools")
	}
}

func TestServerConfigValidateRejectsDenyTools(t *testing.T) {
	cfg := ServerConfig{
		Name:       "github",
		Transport:  "stdio",
		Command:    "github-mcp",
		AllowTools: []string{"search_repositories"},
		DenyTools:  []string{"delete_repository"},
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected deny_tools to be rejected")
	}
}

func TestServerConfigAllowedTool(t *testing.T) {
	cfg := ServerConfig{
		AllowTools: []string{"search_repositories"},
	}

	if !cfg.AllowedTool("search_repositories") {
		t.Fatal("expected exact allowlisted tool to be allowed")
	}
	if cfg.AllowedTool("delete_repository") {
		t.Fatal("expected unallowlisted tool to be denied")
	}
}

func TestLoadConfigParsesAndValidatesServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	contents := `{
		"servers": [
			{
				"name": "github",
				"transport": "stdio",
				"command": "github-mcp",
				"allow_tools": ["search_repositories"]
			}
		]
	}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if len(cfg.Servers) != 1 {
		t.Fatalf("len(Servers) = %d, want 1", len(cfg.Servers))
	}
	if !cfg.Servers[0].AllowedTool("search_repositories") {
		t.Fatal("expected loaded allow_tools to allow exact tool")
	}
}

func TestLoadConfigRejectsNullServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"servers":null}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected null servers to be rejected")
	}
}

func TestLoadConfigAllowsEmptyServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"servers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Servers) != 0 {
		t.Fatalf("len(Servers) = %d, want 0", len(cfg.Servers))
	}
}

func TestLoadConfigAcceptsEnvironmentAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	contents := `{
		"servers": [
			{
				"name": "nerdoracle",
				"transport": "stdio",
				"command": "npx",
				"environment": {"NERDORACLE_API_URL": "https://example.invalid"}
			}
		]
	}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Servers[0].Env["NERDORACLE_API_URL"]; got != "https://example.invalid" {
		t.Fatalf("Env[NERDORACLE_API_URL] = %q, want https://example.invalid", got)
	}
}

func TestLoadConfigRejectsInvalidServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	contents := `{
		"servers": [
			{
				"name": "github",
				"transport": "stdio",
				"command": "github-mcp",
				"allow_tools": ["search_repositories"],
				"deny_tools": ["delete_repository"]
			}
		]
	}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected invalid server config to be rejected")
	}
}

func TestConfigValidateRejectsDuplicateServerNames(t *testing.T) {
	cfg := Config{
		Servers: []ServerConfig{
			{Name: "github", Transport: "http", URL: "https://example.invalid/one"},
			{Name: "github", Transport: "http", URL: "https://example.invalid/two"},
		},
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected duplicate server names to be rejected")
	}
}

func TestSaveConfigWritesJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "mcp.json")
	cfg := Config{
		Servers: []ServerConfig{
			{
				Name:       "github",
				Transport:  "stdio",
				Command:    "github-mcp",
				AllowTools: []string{"search_repositories"},
			},
		},
	}

	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(loaded.Servers) != 1 {
		t.Fatalf("len(Servers) = %d, want 1", len(loaded.Servers))
	}
	if loaded.Servers[0].Name != "github" {
		t.Errorf("Name = %q, want github", loaded.Servers[0].Name)
	}
}

func TestServerStatusDoesNotExposeSecrets(t *testing.T) {
	cfg := ServerConfig{
		Name:       "github",
		Transport:  "http",
		URL:        "https://example.com/mcp?token=url-secret",
		Headers:    map[string]string{"Authorization": "Bearer header-secret"},
		Env:        map[string]string{"GITHUB_TOKEN": "env-secret"},
		AllowTools: []string{"search_repositories"},
	}

	data, err := json.Marshal(cfg.Status())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	status := string(data)
	for _, secret := range []string{"url-secret", "header-secret", "env-secret", "example.com"} {
		if strings.Contains(status, secret) {
			t.Fatalf("status exposed %q in %s", secret, status)
		}
	}
	if !strings.Contains(status, "GITHUB_TOKEN") {
		t.Fatalf("status should include env key name, got %s", status)
	}
}
