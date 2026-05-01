// Package config loads and validates the agent's TOML configuration.
package config

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
	Email    EmailConfig    `toml:"email"`
	Limits   LimitsConfig   `toml:"limits"`
	Update   UpdateConfig   `toml:"update"`

	// Embeddings is optional; when Enabled, semantic search is added
	// alongside BM25 in memory_search and memory mutations re-embed
	// the affected file synchronously.
	Embeddings EmbeddingsConfig `toml:"embeddings"`
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

	// Sampler parameters. The OpenAI-spec fields (Temperature, TopP,
	// PresencePenalty, FrequencyPenalty, Seed) are sent on every request.
	// The vendor-extension fields (TopK, MinP, RepetitionPenalty) are
	// not in the OpenAI spec but are accepted as additional JSON keys
	// by KoboldCpp, llama.cpp, and vLLM. Servers that don't recognize
	// them silently ignore them, so it's safe to set them regardless of
	// backend.
	//
	// All sampler fields use zero-value-omitted semantics: a field set
	// to 0 is not sent on the wire, and the server uses its own default
	// (or whatever is configured server-side via e.g. KoboldCpp's
	// --gendefaults). Seed=0 specifically means "unset" because random
	// seeds are the typical desired default.
	TopP              float32 `toml:"top_p"`
	PresencePenalty   float32 `toml:"presence_penalty"`
	FrequencyPenalty  float32 `toml:"frequency_penalty"`
	Seed              int     `toml:"seed"`
	TopK              int     `toml:"top_k"`
	MinP              float32 `toml:"min_p"`
	RepetitionPenalty float32 `toml:"repetition_penalty"`

	// MaxSleep is the upper bound enforced by the `sleep` tool. The agent
	// can request shorter sleeps; longer ones are clamped to this value.
	// Operator messages and forced shutdown still interrupt mid-sleep
	// regardless of this setting. Zero or negative is replaced with the
	// 15-minute default at config load.
	MaxSleep duration `toml:"max_sleep"`

	// StateFile is the path to a JSON file holding the live conversation
	// log. When non-empty, the agent saves the message log atomically at
	// the top of every loop iteration (right before each LLM call) and
	// restores it on startup. The system message is always rebuilt from
	// current prompts and memories on load, so prompt edits take effect
	// across restarts; only the conversation history is preserved.
	// Empty string disables persistence (legacy behavior).
	StateFile string `toml:"state_file"`
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

// LimitsConfig holds configurable size caps for content the agent sees in
// its context. Each limit applies to a different LLM-facing surface; when
// content is clipped, a retrieval hint is appended so the agent knows which
// tool to call to read the rest. Zero or negative values disable the cap
// for that surface (the full content is included).
type LimitsConfig struct {
	// RecentMemoryChars caps each "Recent Memories" entry in the system
	// prompt. Five entries are surfaced per turn, so this multiplied by 5
	// is the rough upper bound on memory content in the system prompt.
	RecentMemoryChars int `toml:"recent_memory_chars"`

	// MemorySearchResultChars caps each result returned by memory_search.
	// Five results are returned per query.
	MemorySearchResultChars int `toml:"memory_search_result_chars"`

	// SandboxOutputChars caps the combined stdout/stderr returned by
	// sandbox_execute and sandbox_shell. Larger output should be written
	// to /output/ and read back with sandbox_read.
	SandboxOutputChars int `toml:"sandbox_output_chars"`
}

// Enabled returns true if Telegram is configured.
func (t TelegramConfig) Enabled() bool {
	return t.Token != "" && t.ChatID != 0
}

// UpdateConfig holds settings for the automatic self-update goroutine.
// Disabled by default; opt in by setting Enabled = true.
type UpdateConfig struct {
	// Enabled toggles the entire self-update mechanism. When false, the
	// updater goroutine doesn't run and the update_check / update_apply
	// tools are not advertised to the LLM.
	Enabled bool `toml:"enabled"`

	// CheckInterval controls how often the updater polls GitHub for new
	// releases. Operator-triggered checks (via the update_check tool)
	// run on demand regardless. Zero falls back to a 1-hour default.
	CheckInterval duration `toml:"check_interval"`

	// GitHubRepo is the "owner/name" path of the repository whose
	// releases we poll. Defaults to "matjam/faultline".
	GitHubRepo string `toml:"github_repo"`

	// RestartMode controls what the agent does after a successful
	// update applies. One of:
	//   "exit"      - save state and os.Exit(0); supervisor (systemd,
	//                 docker, k8s) is expected to respawn the agent.
	//   "self-exec" - save state and syscall.Exec the new binary,
	//                 replacing this process image. Same PID. Suitable
	//                 for bare-process runs without a supervisor.
	//   "command"   - save state, run RestartCommand, exit. For custom
	//                 orchestrators.
	// Default "exit".
	RestartMode string `toml:"restart_mode"`

	// RestartCommand is run when RestartMode = "command". Split on
	// whitespace and exec'd. Empty for the other modes.
	RestartCommand string `toml:"restart_command"`

	// AllowPrerelease, when true, considers GitHub releases marked as
	// prerelease (alpha/beta/rc tags) as candidates. Default false.
	AllowPrerelease bool `toml:"allow_prerelease"`

	// BinaryPath is the absolute path of the running binary. The
	// updater swaps this file in place. Empty falls back to
	// os.Executable() at startup.
	BinaryPath string `toml:"binary_path"`
}

// EmbeddingsConfig holds optional semantic-search settings. When
// Enabled, the agent constructs an OpenAI-compatible embeddings
// client, builds an in-memory vector index of memory files (persisted
// to disk in a custom binary format), and surfaces a "Semantic
// results" section in memory_search alongside the existing BM25
// "Lexical results" section.
//
// The LLM does not see the embeddings directly; it only sees ranked
// path/score lists. The mechanism is best-effort enrichment — embed
// failures are logged but never block memory writes.
type EmbeddingsConfig struct {
	// Enabled toggles the entire feature. When false, no embeddings
	// client is constructed, no vector index is loaded, and
	// memory_search is BM25-only.
	Enabled bool `toml:"enabled"`

	// URL is the OpenAI-compatible API base URL ending in /v1 (no
	// trailing slash). The adapter appends "/embeddings". Defaults to
	// the public OpenAI endpoint.
	URL string `toml:"url"`

	// APIKey is sent as a Bearer token. Required for OpenAI; may be
	// empty for local servers (Ollama, LM Studio, vLLM) that don't
	// authenticate.
	APIKey string `toml:"api_key"`

	// Model is the embedding model identifier (e.g.
	// "text-embedding-3-small", "nomic-embed-text"). The vector
	// index records this on disk; if it changes, the index is
	// discarded and rebuilt on next startup.
	Model string `toml:"model"`

	// Timeout is applied per HTTP request. Zero falls back to 30s.
	Timeout duration `toml:"timeout"`

	// BatchSize is the maximum number of texts per /v1/embeddings
	// call during the startup reconcile pass and bulk re-indexing.
	// Per-mutation embeds always send a single text. Zero falls back
	// to 100.
	BatchSize int `toml:"batch_size"`
}

// Enabled is a struct-receiver alias so callers can write
// `cfg.Embeddings.Active()` without checking both Enabled and the
// minimum required fields. Returns true only when the feature is
// turned on AND has the bare minimum to function.
func (e EmbeddingsConfig) Active() bool {
	return e.Enabled && e.URL != "" && e.Model != ""
}

// EmailConfig holds optional IMAP email connection settings.
type EmailConfig struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

// Enabled returns true if Email is configured.
func (e EmailConfig) Enabled() bool {
	return e.Host != "" && e.User != "" && e.Password != ""
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

// Default returns a Config with sensible defaults.
func Default() *Config {
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
			MaxSleep:            duration(15 * time.Minute),
		},
		Log: LogConfig{
			Level: "info",
			Dir:   "./logs",
		},
		Sandbox: SandboxConfig{
			Enabled: false,
			// Faultline's own multi-runtime sandbox image (Arch-based;
			// ships uv/uvx, python+pip, node+npm+npx, bun, deno, go,
			// plus common LLM-friendly CLI tools). Built from
			// docker/sandbox/Dockerfile and published by the
			// sandbox-image GH Actions workflow. Pin to a versioned
			// tag in your config.toml if you want a specific image
			// version locked down.
			Image:       "ghcr.io/matjam/faultline-sandbox:latest",
			Dir:         "./sandbox",
			Timeout:     duration(5 * time.Minute),
			Network:     false,
			MemoryLimit: "512m",
		},
		Limits: LimitsConfig{
			// Defaults are substantially larger than the original
			// hard-coded values (2000 / 1500 / 24000) so the agent
			// rarely sees clipped content in practice.
			RecentMemoryChars:       8000,
			MemorySearchResultChars: 6000,
			SandboxOutputChars:      64000,
		},
		Update: UpdateConfig{
			Enabled:       false,
			CheckInterval: duration(1 * time.Hour),
			GitHubRepo:    "matjam/faultline",
			RestartMode:   "exit",
		},
		Embeddings: EmbeddingsConfig{
			Enabled:   false,
			URL:       "https://api.openai.com/v1",
			Model:     "text-embedding-3-small",
			Timeout:   duration(30 * time.Second),
			BatchSize: 100,
		},
	}
}

// Load reads a TOML config file. Missing fields keep their defaults.
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config file: %w", err)
	}

	// Replace nonsensical sleep caps with the default rather than allowing
	// max_sleep = 0 to silently disable the sleep tool's clamp. A user who
	// genuinely wants no sleep cap can set a very large value explicitly.
	if cfg.Agent.MaxSleep.Duration() <= 0 {
		cfg.Agent.MaxSleep = duration(15 * time.Minute)
	}

	// Same logic for update poll interval -- 0 in the file shouldn't
	// silently disable polling. A user who wants polling off should
	// set update.enabled = false.
	if cfg.Update.CheckInterval.Duration() <= 0 {
		cfg.Update.CheckInterval = duration(1 * time.Hour)
	}

	// Embeddings: backfill defaults when the operator enables the
	// feature but leaves these fields zero.
	if cfg.Embeddings.Timeout.Duration() <= 0 {
		cfg.Embeddings.Timeout = duration(30 * time.Second)
	}
	if cfg.Embeddings.BatchSize <= 0 {
		cfg.Embeddings.BatchSize = 100
	}

	return cfg, nil
}
