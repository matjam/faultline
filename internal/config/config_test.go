package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig_DefaultsPreserved(t *testing.T) {
	// A minimal config that overrides only one field; everything else should
	// keep its default.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[api]
url = "http://example.com/v1"
`
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	defaults := Default()

	if cfg.API.URL != "http://example.com/v1" {
		t.Errorf("URL = %q, want override applied", cfg.API.URL)
	}
	if cfg.API.Model != defaults.API.Model {
		t.Errorf("Model = %q, want default %q", cfg.API.Model, defaults.API.Model)
	}
	if cfg.Agent.MemoryDir != defaults.Agent.MemoryDir {
		t.Errorf("MemoryDir = %q, want default %q", cfg.Agent.MemoryDir, defaults.Agent.MemoryDir)
	}
	if cfg.Agent.MaxTokens != defaults.Agent.MaxTokens {
		t.Errorf("MaxTokens = %d, want default %d", cfg.Agent.MaxTokens, defaults.Agent.MaxTokens)
	}
	if cfg.Limits.RecentMemoryChars != defaults.Limits.RecentMemoryChars {
		t.Errorf("Limits.RecentMemoryChars = %d, want default %d",
			cfg.Limits.RecentMemoryChars, defaults.Limits.RecentMemoryChars)
	}
	if cfg.Limits.MemorySearchResultChars != defaults.Limits.MemorySearchResultChars {
		t.Errorf("Limits.MemorySearchResultChars = %d, want default %d",
			cfg.Limits.MemorySearchResultChars, defaults.Limits.MemorySearchResultChars)
	}
	if cfg.Limits.SandboxOutputChars != defaults.Limits.SandboxOutputChars {
		t.Errorf("Limits.SandboxOutputChars = %d, want default %d",
			cfg.Limits.SandboxOutputChars, defaults.Limits.SandboxOutputChars)
	}
}

func TestLoadConfig_LimitsOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[limits]
recent_memory_chars = 12345
memory_search_result_chars = 6789
sandbox_output_chars = 100000
`
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Limits.RecentMemoryChars != 12345 {
		t.Errorf("RecentMemoryChars = %d, want 12345", cfg.Limits.RecentMemoryChars)
	}
	if cfg.Limits.MemorySearchResultChars != 6789 {
		t.Errorf("MemorySearchResultChars = %d, want 6789", cfg.Limits.MemorySearchResultChars)
	}
	if cfg.Limits.SandboxOutputChars != 100000 {
		t.Errorf("SandboxOutputChars = %d, want 100000", cfg.Limits.SandboxOutputChars)
	}
}

func TestLoadConfig_MCPDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[api]
url = "http://example.com/v1"
`
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.MCP.Enabled {
		t.Error("MCP.Enabled should default false")
	}
	if cfg.MCP.ConfigFile != "./mcp.json" {
		t.Errorf("MCP.ConfigFile = %q, want ./mcp.json", cfg.MCP.ConfigFile)
	}
	if cfg.MCP.AllowAgentEditConfig {
		t.Error("MCP.AllowAgentEditConfig should default false")
	}
	if got, want := cfg.MCP.StdioIdleTimeout.Duration(), 10*time.Minute; got != want {
		t.Errorf("MCP.StdioIdleTimeout = %v, want %v", got, want)
	}
}

func TestLoadConfig_MCPOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[mcp]
enabled = true
config_file = "./custom-mcp.json"
allow_agent_edit_config = true
stdio_idle_timeout = "30s"
`
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.MCP.Enabled {
		t.Error("MCP.Enabled should be true")
	}
	if cfg.MCP.ConfigFile != "./custom-mcp.json" {
		t.Errorf("MCP.ConfigFile = %q, want ./custom-mcp.json", cfg.MCP.ConfigFile)
	}
	if !cfg.MCP.AllowAgentEditConfig {
		t.Error("MCP.AllowAgentEditConfig should be true")
	}
	if got, want := cfg.MCP.StdioIdleTimeout.Duration(), 30*time.Second; got != want {
		t.Errorf("MCP.StdioIdleTimeout = %v, want %v", got, want)
	}
}

func TestLoadConfig_AdminUIDefaultsToMatrix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[api]
url = "http://example.com/v1"
`
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Admin.UI != "matrix" {
		t.Errorf("Admin.UI = %q, want matrix", cfg.Admin.UI)
	}
}

func TestLoadConfig_AdminUIOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[admin]
enabled = true
ui = "modern"
`
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Admin.UI != "modern" {
		t.Errorf("Admin.UI = %q, want modern", cfg.Admin.UI)
	}
}

func TestLoadConfig_InvalidAdminUI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[admin]
ui = "neon"
`
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid admin ui error")
	}
}

func TestLoadConfig_DurationParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[sandbox]
enabled = true
timeout = "2m30s"
`
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := cfg.Sandbox.Timeout.Duration(), 2*time.Minute+30*time.Second; got != want {
		t.Errorf("Timeout = %v, want %v", got, want)
	}
	if !cfg.Sandbox.Enabled {
		t.Error("Sandbox.Enabled should be true")
	}
}

func TestLoadConfig_InvalidDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[sandbox]
timeout = "not-a-duration"
`
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("expected error parsing invalid duration")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "no-such.toml")); err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestLoadConfig_InvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("this is not toml ==="), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("expected error for invalid TOML")
	}
}

func TestTelegramConfig_Enabled(t *testing.T) {
	if (TelegramConfig{}).Enabled() {
		t.Error("empty TelegramConfig should not be Enabled")
	}
	if (TelegramConfig{Token: "x"}).Enabled() {
		t.Error("token without chat_id should not be Enabled")
	}
	if (TelegramConfig{ChatID: 123}).Enabled() {
		t.Error("chat_id without token should not be Enabled")
	}
	if !(TelegramConfig{Token: "x", ChatID: 1}).Enabled() {
		t.Error("token + chat_id should be Enabled")
	}
}
