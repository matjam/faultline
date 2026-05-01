package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

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
	"github.com/matjam/faultline/internal/search/bm25"
	"github.com/matjam/faultline/internal/search/vector"
	"github.com/matjam/faultline/internal/subagent"
	"github.com/matjam/faultline/internal/tools"
	"github.com/matjam/faultline/internal/update"
)

// subagentDeps bundles the shared dependencies a subagent needs. All
// fields are borrowed from the primary's lifecycle -- the spawnFn
// closure does not own any of them.
type subagentDeps struct {
	Memory      *fs.Store
	Index       *bm25.Index
	VectorIndex *vector.Index
	Telegram    *telegram.Bot
	Sandbox     *docker.Sandbox
	Email       *config.EmailConfig
	Kobold      *kobold.Client
	Updater     *update.Updater
	Embedder    tools.Embedder
	Skills      *skillsfs.Store
	WebCache    *tools.WebCache
	MCPCaller   mcp.Caller
	MCPTools    []mcp.DiscoveredServer
	Logger      *slog.Logger
}

// buildSubagentManager constructs the subagent.Manager and the
// SpawnFunc closure that runs a child agent loop. Returns nil when
// [subagent] is disabled (the primary then runs without subagent
// support; subagent_* tools are not advertised).
//
// Operator-supplied profiles that fail validation are logged and
// skipped rather than aborting startup; the synthesized "default"
// profile is always available when the feature is enabled.
func buildSubagentManager(cfg *config.Config, deps subagentDeps) *subagent.Manager {
	if !cfg.Subagent.Active() {
		return nil
	}

	// The synthesized default profile mirrors the primary's [api]
	// settings so the agent can delegate to "the same backend" without
	// any operator config.
	profiles := []subagent.Profile{
		{
			Name:    subagent.DefaultProfileName,
			APIURL:  cfg.API.URL,
			APIKey:  cfg.API.Key,
			Model:   cfg.API.Model,
			Purpose: "Same backend, model, and sampler settings as the primary agent. Use when no specialized profile fits.",
		},
	}

	for _, p := range cfg.Subagent.Profiles {
		cp := subagent.Profile{
			Name:              p.Name,
			APIURL:            p.APIURL,
			APIKey:            p.APIKey,
			Model:             p.Model,
			Purpose:           p.Purpose,
			Temperature:       p.Temperature,
			TopP:              p.TopP,
			TopK:              p.TopK,
			MinP:              p.MinP,
			RepetitionPenalty: p.RepetitionPenalty,
			MaxRespTokens:     p.MaxRespTokens,
		}
		if err := subagent.ValidateProfile(cp); err != nil {
			deps.Logger.Warn("subagent profile invalid; skipping",
				"name", p.Name, "error", err)
			continue
		}
		profiles = append(profiles, cp)
	}

	deps.Logger.Info("subagent support enabled",
		"profiles", len(profiles),
		"max_concurrent", cfg.Subagent.MaxConcurrent,
		"max_turns_per_run", cfg.Subagent.MaxTurnsPerRun,
		"max_inbox", cfg.Subagent.MaxInbox,
		"run_timeout", cfg.Subagent.RunTimeout.Duration().String(),
	)

	spawnFn := func(ctx context.Context, workID string, profile subagent.Profile, prompt string, maxTurns int) subagent.Report {
		return runSubagent(ctx, workID, profile, prompt, maxTurns, cfg, deps)
	}

	return subagent.New(subagent.Config{
		MaxConcurrent:  cfg.Subagent.MaxConcurrent,
		MaxTurnsPerRun: cfg.Subagent.MaxTurnsPerRun,
		MaxInbox:       cfg.Subagent.MaxInbox,
		RunTimeout:     cfg.Subagent.RunTimeout.Duration(),
	}, profiles, spawnFn, deps.Logger)
}

// subagentSystemPrompt is the system-prompt template used for child
// agents. The parent's task is interpolated into the body so the
// child sees its identity, the rules of the road, and the operator's
// (i.e. primary's) instructions in one message.
const subagentSystemPrompt = `You are a SUBAGENT of a primary agent. The primary delegated work to you and is waiting for your report.

Rules:
  - When you have finished (or determined the work cannot be done), call the subagent_report tool with a self-contained summary. After that call, your loop will exit -- do not issue further tool calls.
  - You have access to the same tools as the primary, EXCEPT: sleep, update_check, update_apply, MCP management/config tools, and the subagent_* tools (no nested delegation). The send_message tool is available; use it sparingly.
  - You share the primary's memory store, search indexes, sandbox, and skills catalog. Memory writes are visible to the primary and to other subagents.
  - The primary cannot see your conversation. Only the subagent_report payload reaches them. Put EVERYTHING the primary needs to know in that report.

=== Task from primary agent ===

%s`

// runSubagent constructs and runs a single child agent loop. Called
// by the SpawnFunc; blocks until the child's loop returns.
//
// The returned Report's WorkID/Profile/StartedAt fields are filled in
// by the Manager's runOne wrapper, so we leave them zero here.
func runSubagent(
	ctx context.Context,
	workID string,
	profile subagent.Profile,
	promptText string,
	maxTurns int,
	cfg *config.Config,
	deps subagentDeps,
) subagent.Report {
	childLogger := deps.Logger.With("agent", workID, "profile", profile.Name)
	childLogger.Info("subagent: starting", "max_turns", maxTurns)

	// Per-profile chat client and per-subagent chat log.
	chat := openai.New(profile.APIURL, profile.APIKey, profile.Model, childLogger)
	if cl, err := openai.NewChatLoggerPrefixed(cfg.Log.Dir, "chat-"+workID+"-"); err == nil {
		chat.SetChatLog(cl)
		defer cl.Close()
	} else {
		childLogger.Warn("could not open per-subagent chat log; continuing without it",
			"error", err)
	}

	// Forward declaration: the sink closure needs the *Agent so it
	// can call RequestStop after the report is captured. Assigned
	// below before any tool call could fire.
	var (
		reportText string
		reported   atomic.Bool
		childAgent *agent.Agent
	)
	sink := func(text string) {
		reportText = text
		reported.Store(true)
		if childAgent != nil {
			childAgent.RequestStop()
		}
	}

	// Per-subagent cfg copy so profile sampler overrides take effect
	// without mutating the shared cfg pointer the primary is using.
	// AgentConfig is value-typed; the slices on Config are read-only
	// here so the shallow copy is safe.
	childCfg := *cfg
	childCfg.API.Model = profile.Model
	if profile.Temperature != 0 {
		childCfg.Agent.Temperature = profile.Temperature
	}
	if profile.TopP != 0 {
		childCfg.Agent.TopP = profile.TopP
	}
	if profile.TopK != 0 {
		childCfg.Agent.TopK = profile.TopK
	}
	if profile.MinP != 0 {
		childCfg.Agent.MinP = profile.MinP
	}
	if profile.RepetitionPenalty != 0 {
		childCfg.Agent.RepetitionPenalty = profile.RepetitionPenalty
	}
	if profile.MaxRespTokens != 0 {
		childCfg.Agent.MaxRespTokens = profile.MaxRespTokens
	}
	// State persistence is off for subagents; conversation is
	// ephemeral and the primary's state file is the source of truth.
	childCfg.Agent.StateFile = ""

	// Translate adapters into agent.* ports (nil-allowed).
	var skillsPort agent.Skills
	if deps.Skills != nil {
		skillsPort = deps.Skills
	}
	var tokenizerPort agent.Tokenizer
	if deps.Kobold != nil {
		tokenizerPort = deps.Kobold
	}

	// Child Executor in subagent mode. Same shared infrastructure as
	// the primary (memory, indexes, sandbox, skills, webCache); no
	// updater (update_* tools are stripped via Mode anyway), no
	// subagent manager (no nesting), report sink wired so
	// subagent_report fires the closure above.
	childExec := tools.New(tools.Deps{
		Mode:                tools.ModeSubagent,
		Memory:              deps.Memory,
		Index:               deps.Index,
		VectorIndex:         deps.VectorIndex,
		Telegram:            deps.Telegram,
		Sandbox:             deps.Sandbox,
		Email:               deps.Email,
		Kobold:              deps.Kobold,
		Embedder:            deps.Embedder,
		Skills:              deps.Skills,
		SkillInstallEnabled: false,
		EmbedBatchSize:      cfg.Embeddings.BatchSize,
		MCPDiscovered:       deps.MCPTools,
		MCPCaller:           deps.MCPCaller,
		Logger:              childLogger,
		WebCache:            deps.WebCache,
		MaxTokens:           childCfg.Agent.MaxTokens,
		Limits:              childCfg.Limits,
		MaxSleep:            childCfg.Agent.MaxSleep.Duration(),
		SubagentReportFn:    sink,
	})

	systemPrompt := fmt.Sprintf(subagentSystemPrompt, promptText)

	childAgent = agent.New(&childCfg, agent.Deps{
		Chat:                 chat,
		Memory:               deps.Memory,
		Search:               deps.Index,
		Operator:             nil, // children never see the operator queue
		Tokenizer:            tokenizerPort,
		Tools:                childExec,
		State:                jsonfile.NewPersister("", childLogger),
		Skills:               skillsPort,
		Subagents:            nil, // no nesting
		MaxTurns:             maxTurns,
		SystemPromptOverride: systemPrompt,
	}, childLogger)
	defer childAgent.Close()

	// shutdownCh: nil for subagents. The Manager's parent ctx already
	// cancels on the agent's two-phase shutdown via the toolCtx
	// bridge in agent.Run; a nil shutdownCh just means "no graceful-
	// save phase for this child", which is what we want -- the child's
	// state isn't persisted anyway.
	runErr := childAgent.Run(ctx, nil)

	rep := subagent.Report{Text: reportText}
	switch {
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		rep.Canceled = true
		rep.Err = runErr
	case runErr != nil:
		rep.Err = runErr
	case !reported.Load():
		// Loop exited cleanly (MaxTurns hit) without subagent_report.
		rep.Truncated = true
	}
	childLogger.Info("subagent: finished",
		"reported", reported.Load(),
		"canceled", rep.Canceled,
		"truncated", rep.Truncated,
		"err", rep.Err,
	)
	return rep
}
