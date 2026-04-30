package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/matjam/faultline/internal/adapters/operator/telegram"
	"github.com/matjam/faultline/internal/config"
	"github.com/matjam/faultline/internal/log"
)

func main() {
	configPath := flag.String("config", "./config.toml", "Path to configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "path", *configPath, "error", err)
		os.Exit(1)
	}

	// Console log level from config
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

	// Console handler at configured level
	consoleHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: consoleLevel,
	})

	// File handler at debug level (always captures everything)
	fileWriter, err := log.NewDaily(cfg.Log.Dir)
	if err != nil {
		slog.Error("failed to create log directory", "dir", cfg.Log.Dir, "error", err)
		os.Exit(1)
	}
	defer fileWriter.Close()

	fileHandler := slog.NewTextHandler(fileWriter, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	// Combine: console at configured level, file always at debug
	logger := slog.New(log.NewMultiHandler(consoleHandler, fileHandler))
	slog.SetDefault(logger)

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

	// Set up Telegram if configured
	var tg *telegram.Bot
	if cfg.Telegram.Enabled() {
		tg, err = telegram.New(cfg.Telegram.Token, cfg.Telegram.ChatID, logger)
		if err != nil {
			logger.Error("failed to connect telegram bot", "error", err)
			os.Exit(1)
		}
		go tg.Start(ctx)
		logger.Info("telegram bot enabled", "chat_id", cfg.Telegram.ChatID)

		// Send a startup ping so the collaborator knows the bot is alive
		if err := tg.Send("Agent starting up. I can hear you."); err != nil {
			logger.Warn("failed to send startup ping", "error", err)
		}
	} else {
		logger.Info("telegram not configured, messaging disabled")
	}

	agent, err := NewAgent(cfg, tg, logger)
	if err != nil {
		logger.Error("failed to create agent", "error", err)
		os.Exit(1)
	}

	logger.Info("agent starting",
		"api_url", cfg.API.URL,
		"model", cfg.API.Model,
		"memory_dir", cfg.Agent.MemoryDir,
		"max_tokens", cfg.Agent.MaxTokens,
		"compaction_threshold", cfg.Agent.CompactionThreshold,
	)

	defer agent.Close()

	if err := agent.Run(ctx, shutdownCh); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("agent terminated with error", "error", err)
		os.Exit(1)
	}

	logger.Info("agent shut down gracefully")
}
