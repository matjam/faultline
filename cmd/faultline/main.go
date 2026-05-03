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
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	adminhttp "github.com/matjam/faultline/internal/adapters/admin/http"
	"github.com/matjam/faultline/internal/adapters/llm/kobold"
	"github.com/matjam/faultline/internal/adapters/llm/openai"
	"github.com/matjam/faultline/internal/adapters/mcp"
	"github.com/matjam/faultline/internal/adapters/memory/fs"
	"github.com/matjam/faultline/internal/adapters/operator/telegram"
	"github.com/matjam/faultline/internal/adapters/sandbox/docker"
	skillsfs "github.com/matjam/faultline/internal/adapters/skills/fs"
	"github.com/matjam/faultline/internal/adapters/state/jsonfile"
	"github.com/matjam/faultline/internal/agent"
	"github.com/matjam/faultline/internal/config"
	"github.com/matjam/faultline/internal/log"
	"github.com/matjam/faultline/internal/prompts"
	"github.com/matjam/faultline/internal/search/bm25"
	"github.com/matjam/faultline/internal/search/vector"
	"github.com/matjam/faultline/internal/subagent"
	"github.com/matjam/faultline/internal/tools"
	"github.com/matjam/faultline/internal/update"
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

	// Refuse to run as root. The agent has unrestricted access to its
	// memory store, web fetch, sandbox bind-mounts, and the host
	// filesystem inside its working directory; running as root means
	// any prompt-injection or malicious-skill blast radius is the
	// whole machine. The sandbox security model also depends on
	// --user <unprivileged>:<unprivileged>, which collapses to root
	// when os.Getuid()==0.
	//
	// Loud-fail on stderr (logger isn't built yet at this point) and
	// exit non-zero so systemd / docker / k8s see a clear failure
	// rather than silently downgrading to an insecure run.
	//
	// os.Getuid returns -1 on Windows, so this is effectively a
	// no-op there; faultline isn't a meaningful Windows target.
	if uid := os.Getuid(); uid == 0 {
		fmt.Fprintln(os.Stderr, "faultline: refusing to run as root (uid=0). The agent has broad filesystem and network access; running as root means a prompt injection or a malicious skill compromises the whole machine. Run as an unprivileged user (systemd User=, sudo -u <user>, container USER directive, etc.). To override this check intentionally is not supported.")
		os.Exit(1)
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

	// shutdownCh is closed exactly once -- by either the signal
	// handler or the updater. Whichever fires first wins; the other
	// is a no-op via shutdownOnce.
	shutdownCh := make(chan struct{})
	var shutdownOnce sync.Once

	// updateResult is set by the updater BEFORE it closes shutdownCh,
	// so when Agent.Run returns we can read it and dispatch the
	// configured restart action. nil = signal-driven shutdown, no
	// restart needed.
	var updateResult atomic.Pointer[update.Result]

	closeShutdown := func(r *update.Result) {
		shutdownOnce.Do(func() {
			if r != nil {
				updateResult.Store(r)
			}
			close(shutdownCh)
		})
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		logger.Info("shutdown requested, saving state... (send again to force quit)")
		closeShutdown(nil)

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

	oauthStore := mcp.NewFileCredentialStore(cfg.OAuth.CredentialFile)
	oauthManager := mcp.NewOAuthManager(nil, mcp.OAuthOptions{
		PublicBaseURL: cfg.OAuth.PublicBaseURL,
		CallbackPath:  cfg.OAuth.CallbackPath,
		StateTTL:      cfg.OAuth.StateTTL.Duration(),
	}, oauthStore, nil)

	mcpCaller, mcpDiscovered, err := setupMCP(ctx, cfg.MCP, oauthManager, sandboxMCPStdioRunner{sandbox: sb}, logger)
	if err != nil {
		logger.Error("init mcp", "error", err)
		os.Exit(1)
	}
	var mcpApprovals *mcp.Approvals
	if cfg.MCP.Enabled && cfg.MCP.AllowAgentEditConfig {
		mcpApprovals = mcp.NewApprovals()
	}

	// Updater. Always constructed so the get_version tool works even
	// when self-update is disabled; the polling goroutine starts only
	// when cfg.Update.Enabled.
	binaryPath := cfg.Update.BinaryPath
	if binaryPath == "" {
		if exe, err := os.Executable(); err == nil {
			binaryPath = exe
		}
	}
	updater := update.New(update.Config{
		Enabled:         cfg.Update.Enabled,
		CheckInterval:   cfg.Update.CheckInterval.Duration(),
		GitHubRepo:      cfg.Update.GitHubRepo,
		AllowPrerelease: cfg.Update.AllowPrerelease,
		BinaryPath:      binaryPath,
	}, memory, closeShutdown, logger)
	go updater.Run(ctx)

	// Embeddings + vector index. Optional; failure on probe disables
	// the feature for this session but doesn't stop the agent — the
	// core loop is fully functional with BM25-only search.
	var embedder tools.Embedder
	var vIndex *vector.Index
	if cfg.Embeddings.Active() {
		embedder, vIndex = setupEmbeddings(ctx, cfg, memory, logger)
	}
	if vIndex != nil {
		// Persistence loop runs for the lifetime of the agent. It
		// flushes the index when dirty on a 30s tick, plus a final
		// flush on shutdown via defer below.
		vectorPath := vectorIndexPath(cfg.Agent.MemoryDir)
		go runVectorPersistence(ctx, vIndex, vectorPath, logger)
		defer flushVectorIndex(vIndex, vectorPath, logger)
	}

	// Skills (Agent Skills support, https://agentskills.io). Optional;
	// when configured but the root directory doesn't exist, the catalog
	// stays empty and Reload picks up skills the operator adds later.
	var skillStore *skillsfs.Store
	if cfg.Skills.Active() {
		skillStore, err = skillsfs.New(cfg.Skills.Dir, logger)
		if err != nil {
			logger.Error("init skills store", "error", err, "dir", cfg.Skills.Dir)
			os.Exit(1)
		}
		logger.Info("skills enabled", "dir", skillStore.Root())

		// Load the operator-controlled enable/disable state file
		// (admin UI's Skills page persists toggles here). Missing
		// file is fine: it means "no skills disabled". Parse
		// errors are loud but not fatal — the agent runs without
		// the toggle layer in that case.
		if cfg.Admin.SkillsFile != "" {
			if err := skillStore.LoadDisabledFromFile(cfg.Admin.SkillsFile); err != nil {
				logger.Error("skills: failed to load disabled-state file",
					"path", cfg.Admin.SkillsFile, "error", err)
			} else {
				logger.Info("skills: disabled-state file loaded",
					"path", cfg.Admin.SkillsFile)
			}
		}

		// Wipe the per-call /work scratch root from the previous
		// session so stale work_ids issued before a restart can't
		// resolve. The Sandbox is already constructed at this point
		// when sandbox is enabled; if it's not, skill_execute will
		// surface a useful error at call time.
		if sb != nil {
			if err := sb.ResetSkillWork(); err != nil {
				logger.Warn("could not reset skill /work root; stale call_ids may resolve",
					"error", err)
			}
		}
	} else {
		logger.Info("skills not configured, skill_* tools disabled")
	}

	// Shared web cache is owned by the composition root, not the
	// Executor. With subagents enabled, multiple Executors (primary +
	// children) share one cache; a child's Close must not yank it out
	// from under the primary, so the cache lifecycle lives here.
	webCache := tools.NewWebCache(60 * time.Second)
	defer webCache.Close()

	// Subagent manager (optional). Constructed before the primary's
	// tool executor so the SubagentManager pointer can be wired in;
	// the spawnFn closure shares the primary's adapters.
	var subMgr *subagent.Manager
	if cfg.Subagent.Active() {
		subMgr = buildSubagentManager(cfg, subagentDeps{
			Memory:      memory,
			Index:       index,
			VectorIndex: vIndex,
			Telegram:    tg,
			Sandbox:     sb,
			Email:       email,
			Kobold:      kb,
			Updater:     updater,
			Embedder:    embedder,
			Skills:      skillStore,
			WebCache:    webCache,
			MCPCaller:   mcpCaller,
			MCPTools:    mcpDiscovered,
			Logger:      logger,
		})
	} else {
		logger.Info("subagent support disabled, subagent_* tools not advertised")
	}

	oauthSrv, err := buildOAuthCallbackServer(cfg.OAuth, oauthManager, logger)
	if err != nil {
		logger.Error("OAuth callback server failed to configure", "error", err)
		os.Exit(1)
	}
	oauthSrv.Start(ctx)

	// Admin UI is constructed BEFORE the tool executor when enabled,
	// because the executor takes the admin's tool ring buffer as its
	// Observer. Inspectors are attached back to the admin server
	// after the agent is built (see AttachInspectors below).
	processStarted := time.Now()
	adminSrv, err := buildAdmin(ctx, cfg, processStarted, logger)
	if err != nil {
		logger.Error("admin UI failed to start", "error", err)
		os.Exit(1)
	}

	toolExec := tools.New(tools.Deps{
		Mode:                 tools.ModePrimary,
		Memory:               memory,
		Index:                index,
		VectorIndex:          vIndex,
		Telegram:             tg,
		Sandbox:              sb,
		Email:                email,
		Kobold:               kb,
		Updater:              updater,
		Embedder:             embedder,
		Skills:               skillStore,
		SkillInstallEnabled:  cfg.Skills.InstallEnabled,
		EmbedBatchSize:       cfg.Embeddings.BatchSize,
		MCPDiscovered:        mcpDiscovered,
		MCPCaller:            mcpCaller,
		MCPConfigFile:        cfg.MCP.ConfigFile,
		MCPConfigEditEnabled: cfg.MCP.Enabled && cfg.MCP.AllowAgentEditConfig,
		MCPApprovals:         mcpApprovals,
		MCPReload: func(ctx context.Context) (mcp.Caller, []mcp.DiscoveredServer, error) {
			return setupMCP(ctx, cfg.MCP, oauthManager, sandboxMCPStdioRunner{sandbox: sb}, logger)
		},
		MCPOAuth:        oauthManager,
		Logger:          logger,
		WebCache:        webCache,
		MaxTokens:       cfg.Agent.MaxTokens,
		Limits:          cfg.Limits,
		MaxSleep:        cfg.Agent.MaxSleep.Duration(),
		SubagentManager: subMgr,
		Observer:        adminSrv.ToolObserver(),
	})
	// NOTE: do not defer toolExec.Close() here. The agent owns the tool
	// executor's lifecycle via the Tools port; agent.Close() (deferred
	// below) calls tools.Close() exactly once. The shared webCache has
	// its own defer above.

	state := jsonfile.NewPersister(cfg.Agent.StateFile, logger)

	// --- Agent ---------------------------------------------------------

	// Translate the concrete *skillsfs.Store into the agent.Skills
	// interface. agent.Skills is nil-allowed; the conversion via a
	// nil typed pointer would create a non-nil interface, which is
	// the wrong behavior here -- so guard the conversion explicitly.
	var skillsPort agent.Skills
	if skillStore != nil {
		skillsPort = skillStore
	}

	// agent.Subagents port: wire the manager when subagent support is
	// enabled. Same nil-interface guard as the Skills port -- a typed
	// nil pointer would create a non-nil interface, defeating the
	// nil-allowed check inside the agent loop.
	var subagentsPort agent.Subagents
	if subMgr != nil {
		subagentsPort = subMgr
	}

	a := agent.New(cfg, agent.Deps{
		Chat:      chat,
		Memory:    memory,
		Search:    index,
		Operator:  operator,
		Tokenizer: tokenizer,
		Tools:     toolExec,
		State:     state,
		Skills:    skillsPort,
		Subagents: subagentsPort,
	}, logger)
	defer a.Close()

	// Now that the agent and (optionally) subagent manager exist,
	// hand them to the admin server as inspector ports. The admin
	// dashboard's live fragments read from these on every poll.
	if adminSrv != nil {
		var subInspector adminhttp.SubagentInspector
		if subMgr != nil {
			subInspector = subMgr
		}
		adminSrv.AttachInspectors(a, subInspector)
		// Skills admin: wired separately because the skills
		// store has no dependency on the agent or subagent
		// manager. nil-safe inside SetSkillsAdmin if skills
		// support is off entirely.
		if skillStore != nil {
			adminSrv.AttachSkills(skillStore)
		}
		// Self-update inspector: always wired when admin is on.
		// The updater itself is harmless when [update] is
		// disabled — Apply refuses, State() returns the cached
		// (mostly empty) state, and the UI surfaces "auto-update
		// off".
		adminSrv.AttachUpdater(updater)

		// Configuration store: lets the operator edit
		// config.toml from the UI, validate, save, and trigger a
		// graceful restart through the same closeShutdown the
		// SIGINT handler and updater use.
		cfgStore, err := newFileConfigStore(*configPath, logger, func() { closeShutdown(nil) })
		if err != nil {
			logger.Error("admin: failed to construct config store", "error", err)
			os.Exit(1)
		}
		adminSrv.AttachConfig(cfgStore)

		adminSrv.Start(ctx)
	}

	logger.Info("agent starting",
		"api_url", cfg.API.URL,
		"model", cfg.API.Model,
		"memory_dir", cfg.Agent.MemoryDir,
		"max_tokens", cfg.Agent.MaxTokens,
		"compaction_threshold", cfg.Agent.CompactionThreshold,
	)

	if err := a.Run(ctx, shutdownCh); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("agent terminated with error", "error", err)
		// Best-effort admin shutdown before exit.
		forceCancel()
		oauthSrv.Wait()
		adminSrv.Wait()
		adminSrv.Close()
		os.Exit(1)
	}

	logger.Info("agent shut down gracefully")

	// Wind down the admin server. The agent has saved state and
	// returned; nothing else useful runs through the admin UI now.
	forceCancel()
	oauthSrv.Wait()
	adminSrv.Wait()
	adminSrv.Close()

	// If shutdown was triggered by an applied update, dispatch the
	// configured restart action. Adapters that own their own resources
	// were closed via defer above, so it is safe to exec a new binary
	// or run a restart command at this point.
	if r := updateResult.Load(); r != nil {
		dispatchRestart(*r, cfg.Update.RestartMode, cfg.Update.RestartCommand, binaryPath, logger)
	}
}

// dispatchRestart runs the configured action after a successful update
// has been applied and the agent loop has shut down cleanly. Returns
// only on "exit" or "command" (the supervisor / new process takes over
// from here); does not return for "self-exec".
func dispatchRestart(r update.Result, mode, command, binaryPath string, logger *slog.Logger) {
	switch mode {
	case "", "exit":
		logger.Info("update applied; exiting for supervisor restart",
			"from", r.FromVersion, "to", r.ToVersion)
	case "self-exec":
		logger.Info("update applied; replacing process image with new binary",
			"from", r.FromVersion, "to", r.ToVersion, "binary", binaryPath)
		// syscall.Exec replaces the current process image. Same PID.
		// On success this call never returns. On failure we fall
		// through to os.Exit so the supervisor (if any) can pick up
		// the new binary on the next start.
		// binaryPath is the validated self-update target path selected during
		// startup, not shell-expanded user input.
		if err := syscall.Exec(binaryPath, os.Args, os.Environ()); err != nil { // #nosec G204 // nosemgrep
			logger.Error("self-exec failed; falling back to exit", "error", err)
			os.Exit(1)
		}
	case "command":
		logger.Info("update applied; running configured restart command",
			"from", r.FromVersion, "to", r.ToVersion, "command", command)
		runRestartCommand(command, logger)
	default:
		logger.Warn("unknown restart_mode; treating as exit",
			"mode", mode, "from", r.FromVersion, "to", r.ToVersion)
	}
}

// runRestartCommand splits command on whitespace and starts it
// detached so it survives this process exiting. Errors are logged but
// not fatal; the new binary is already in place, so even a failed
// restart command leaves the deploy in a recoverable state.
func runRestartCommand(command string, logger *slog.Logger) {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		logger.Warn("restart_command is empty; nothing to run")
		return
	}
	// restart_command is an operator-configured post-update command and is
	// executed without a shell.
	cmd := exec.Command(parts[0], parts[1:]...) // #nosec G204 // nosemgrep
	// Detach: new session group so the child outlives our exit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logger.Error("restart command failed to start", "error", err)
	}
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
