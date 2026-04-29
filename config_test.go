package main

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

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	defaults := DefaultConfig()

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

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
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
	if _, err := LoadConfig(path); err == nil {
		t.Error("expected error parsing invalid duration")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "no-such.toml")); err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestLoadConfig_InvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("this is not toml ==="), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
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
