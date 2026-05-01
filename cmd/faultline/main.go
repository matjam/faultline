// Faultline composition root: parse config, wire concrete adapters into
// the agent, run the loop. This is the only place in the codebase that
// knows which adapter implements which port.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/matjam/faultline/internal/adapters/llm/kobold"
	"github.com/matjam/faultline/internal/adapters/llm/openai"
	"github.com/matjam/faultline/internal/adapters/memory/fs"
	"github.com/matjam/faultline/internal/adapters/operator/telegram"
	"github.com/matjam/faultline/internal/adapters/sandbox/docker"
	"github.com/matjam/faultline/internal/adapters/state/jsonfile"
	"github.com/matjam/faultline/internal/agent"
	"github.com/matjam/faultline/internal/config"
	"github.com/matjam/faultline/internal/log"
	"github.com/matjam/faultline/internal/prompts"
	"github.com/matjam/faultline/internal/search/bm25"
	"github.com/matjam/faultline/internal/tools"
	"github.com/matjam/faultline/internal/version"
)

func main() {
	configPath := flag.String("config", "./config.toml", "Path to configuration file")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		os.Exit(0)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "path", *configPath, "error", err)
		os.Exit(1)
	}

	logger := buildLogger(cfg)

	// Two-phase shutdown:
	//   First SIGINT/SIGTERM  -> close shutdownCh, agent saves state
	//   Second SIGINT/SIGTERM -> cancel ctx, force immediate exit
	ctx, forceCancel := context.WithCancel(context.Background())
	defer forceCancel()

	shutdownCh := make(chan struct{})
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		logger.Info("shutdown requested, saving state... (send again to force quit)")
		close(shutdownCh)

		<-sigCh
		logger.Info("forced shutdown")
		forceCancel()
	}()

	// --- Adapter construction -----------------------------------------
	// Each adapter is a concrete implementation of an agent port. The
	// agent struct's only knowledge of these is via the interface.

	memory, err := fs.New(cfg.Agent.MemoryDir)
	if err != nil {
		logger.Error("init memory store", "error", err)
		os.Exit(1)
	}

	// Apply one-time prompt filename migrations (e.g. cycle_start.md ->
	// cycle-start.md). Errors here mean an unresolvable conflict (both
	// old and new files present); operator must intervene before we can
	// safely start.
	if err := prompts.Migrate(memory); err != nil {
		logger.Error("prompt migration", "error", err)
		os.Exit(1)
	}

	index := bm25.New()

	var sb *docker.Sandbox
	if cfg.Sandbox.Enabled {
		workDir, err := os.Getwd()
		if err != nil {
			logger.Error("get working directory", "error", err)
			os.Exit(1)
		}
		sb, err = docker.New(cfg.Sandbox, workDir, cfg.Log.Dir, logger)
		if err != nil {
			logger.Error("init sandbox", "error", err)
			os.Exit(1)
		}
		defer sb.Close()
		logger.Info("sandbox enabled", "dir", cfg.Sandbox.Dir, "image", cfg.Sandbox.Image)
	} else {
		logger.Info("sandbox not configured, Python execution disabled")
	}

	chatLog, err := openai.NewChatLogger(cfg.Log.Dir)
	if err != nil {
		logger.Warn("could not open chat transcript log; continuing without it",
			"error", err, "dir", cfg.Log.Dir)
		chatLog = nil
	} else {
		defer chatLog.Close()
	}

	chat := openai.New(cfg.API.URL, cfg.API.Key, cfg.API.Model, logger)
	chat.SetChatLog(chatLog)

	// Best-effort detection of KoboldCpp-specific endpoints. Detection
	// failure is silent: kb stays nil and the agent falls back to
	// heuristic estimates plus skips abort/perf. Same instance is shared
	// between the agent (as a Tokenizer port) and the tools package
	// (consumed concretely for context_status's perf reporting).
	var kb *kobold.Client
	if cfg.API.KoboldExtras {
		kb = kobold.New(cfg.API.URL, logger)
		detectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := kb.Detect(detectCtx); err != nil {
			logger.Info("kobold extras unavailable, using heuristic token estimates",
				"error", err)
			kb = nil
		}
		cancel()
	}
	var tokenizer agent.Tokenizer
	if kb != nil {
		tokenizer = kb
	}

	// Telegram is optional. When disabled, the operator port stays nil.
	var operator agent.Operator
	var tg *telegram.Bot
	if cfg.Telegram.Enabled() {
		tg, err = telegram.New(cfg.Telegram.Token, cfg.Telegram.ChatID, logger)
		if err != nil {
			logger.Error("failed to connect telegram bot", "error", err)
			os.Exit(1)
		}
		go tg.Start(ctx)
		logger.Info("telegram bot enabled", "chat_id", cfg.Telegram.ChatID)

		// Send a startup ping so the collaborator knows the bot is alive.
		if err := tg.Send("Agent starting up. I can hear you."); err != nil {
			logger.Warn("failed to send startup ping", "error", err)
		}
		operator = tg
	} else {
		logger.Info("telegram not configured, messaging disabled")
	}

	// Email is optional and is only used by the email_fetch tool. The
	// tool dispatcher constructs short-lived imap.Client instances per
	// request; a *config.EmailConfig pointer is enough to gate it.
	var email *config.EmailConfig
	if cfg.Email.Enabled() {
		email = &cfg.Email
	}

	exec := tools.New(memory, index, tg, sb, email, kb, logger,
		cfg.Agent.MaxTokens, cfg.Limits, cfg.Agent.MaxSleep.Duration())
	defer exec.Close()

	state := jsonfile.NewPersister(cfg.Agent.StateFile, logger)

	// --- Agent ---------------------------------------------------------

	a := agent.New(cfg, agent.Deps{
		Chat:      chat,
		Memory:    memory,
		Search:    index,
		Operator:  operator,
		Tokenizer: tokenizer,
		Tools:     exec,
		State:     state,
	}, logger)
	defer a.Close()

	logger.Info("agent starting",
		"api_url", cfg.API.URL,
		"model", cfg.API.Model,
		"memory_dir", cfg.Agent.MemoryDir,
		"max_tokens", cfg.Agent.MaxTokens,
		"compaction_threshold", cfg.Agent.CompactionThreshold,
	)

	if err := a.Run(ctx, shutdownCh); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("agent terminated with error", "error", err)
		os.Exit(1)
	}

	logger.Info("agent shut down gracefully")
}

// buildLogger wires a console handler at the configured level alongside a
// debug-level rotating file handler, so the operator sees a curated stream
// while the on-disk log captures everything for postmortem inspection.
func buildLogger(cfg *config.Config) *slog.Logger {
	var consoleLevel slog.Level
	switch cfg.Log.Level {
	case "debug":
		consoleLevel = slog.LevelDebug
	case "warn":
		consoleLevel = slog.LevelWarn
	case "error":
		consoleLevel = slog.LevelError
	default:
		consoleLevel = slog.LevelInfo
	}

	consoleHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: consoleLevel,
	})

	fileWriter, err := log.NewDaily(cfg.Log.Dir)
	if err != nil {
		slog.Error("failed to create log directory", "dir", cfg.Log.Dir, "error", err)
		os.Exit(1)
	}

	fileHandler := slog.NewTextHandler(fileWriter, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	logger := slog.New(log.NewMultiHandler(consoleHandler, fileHandler))
	slog.SetDefault(logger)
	return logger
}
