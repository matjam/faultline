package main

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// Config holds the agent's configuration, loaded from a TOML file.
type Config struct {
	API      APIConfig      `toml:"api"`
	Agent    AgentConfig    `toml:"agent"`
	Telegram TelegramConfig `toml:"telegram"`
	Log      LogConfig      `toml:"log"`
	Sandbox  SandboxConfig  `toml:"sandbox"`
}

// APIConfig holds LLM API connection settings.
type APIConfig struct {
	URL   string `toml:"url"`
	Key   string `toml:"key"`
	Model string `toml:"model"`

	// KoboldExtras enables auto-detection and use of KoboldCpp-specific
	// endpoints (real tokenization, generation aborts, perf metrics) that
	// sit alongside the OpenAI compatibility layer at the same base URL.
	// Safe to leave on for non-KoboldCpp backends: detection fails silently
	// and the agent falls back to heuristics.
	KoboldExtras bool `toml:"kobold_extras"`
}

// AgentConfig holds agent behavior settings.
type AgentConfig struct {
	MemoryDir           string  `toml:"memory_dir"`
	MaxTokens           int     `toml:"max_tokens"`
	Temperature         float32 `toml:"temperature"`
	MaxRespTokens       int     `toml:"max_response_tokens"`
	CompactionThreshold int     `toml:"compaction_threshold"`
}

// TelegramConfig holds optional Telegram bot settings.
type TelegramConfig struct {
	Token  string `toml:"token"`
	ChatID int64  `toml:"chat_id"`
}

// LogConfig holds logging settings.
type LogConfig struct {
	Level string `toml:"level"`
	Dir   string `toml:"dir"`
}

// SandboxConfig holds Python sandbox execution settings.
type SandboxConfig struct {
	Enabled     bool     `toml:"enabled"`
	Image       string   `toml:"image"`
	Dir         string   `toml:"dir"`
	Timeout     duration `toml:"timeout"`
	Network     bool     `toml:"network"`
	MemoryLimit string   `toml:"memory_limit"`
}

// Enabled returns true if Telegram is configured.
func (t TelegramConfig) Enabled() bool {
	return t.Token != "" && t.ChatID != 0
}

// duration is a wrapper around time.Duration that supports TOML string unmarshaling.
type duration time.Duration

func (d *duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = duration(parsed)
	return nil
}

func (d duration) Duration() time.Duration {
	return time.Duration(d)
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		API: APIConfig{
			URL:          "http://192.168.1.5:5001/v1",
			Model:        "qwen",
			KoboldExtras: true,
		},
		Agent: AgentConfig{
			MemoryDir:           "./memory",
			MaxTokens:           262144,
			Temperature:         0.8,
			MaxRespTokens:       4096,
			CompactionThreshold: 150000,
		},
		Log: LogConfig{
			Level: "info",
			Dir:   "./logs",
		},
		Sandbox: SandboxConfig{
			Enabled:     false,
			Image:       "ghcr.io/astral-sh/uv:python3.12-bookworm-slim",
			Dir:         "./sandbox",
			Timeout:     duration(5 * time.Minute),
			Network:     false,
			MemoryLimit: "512m",
		},
	}
}

// LoadConfig reads a TOML config file. Missing fields keep their defaults.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config file: %w", err)
	}

	return cfg, nil
}
