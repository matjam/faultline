// Package agent is the hexagon: the autonomous agent loop, context
// compaction, idle-loop detection, and graceful shutdown logic. It
// depends only on ports defined in this package; concrete adapter
// implementations are wired up in cmd/faultline/main.go.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/adapters/llm/kobold"
	"github.com/matjam/faultline/internal/adapters/memory/fs"
	"github.com/matjam/faultline/internal/config"
	"github.com/matjam/faultline/internal/llm"
	prompt "github.com/matjam/faultline/internal/prompts"
	"github.com/matjam/faultline/internal/search/bm25"
	skillsdomain "github.com/matjam/faultline/internal/skills"
)

// Agent is the autonomous agent that runs in a continuous loop.
type Agent struct {
	cfg       *config.Config
	chat      ChatModel
	memory    Memory
	search    Searcher
	operator  Operator  // nil when no collaborator channel is configured
	tokenizer Tokenizer // nil when no real tokenizer is detected
	tools     Tools
	state     StateStore
	skills    Skills // nil when skills support is disabled
	logger    *slog.Logger
}

// Deps bundles the agent's port dependencies. Constructing the Agent
// through Deps keeps the call site readable when the parameter list
// would otherwise grow long.
type Deps struct {
	Chat      ChatModel
	Memory    Memory
	Search    Searcher
	Operator  Operator  // optional
	Tokenizer Tokenizer // optional
	Tools     Tools
	State     StateStore
	Skills    Skills // optional
}

// New constructs an Agent. Any nil-allowed dependency (Operator,
// Tokenizer, Skills) may be left as a nil interface; the agent handles
// those cases internally with heuristic fallbacks or empty catalogs.
func New(cfg *config.Config, deps Deps, logger *slog.Logger) *Agent {
	return &Agent{
		cfg:       cfg,
		chat:      deps.Chat,
		memory:    deps.Memory,
		search:    deps.Search,
		operator:  deps.Operator,
		tokenizer: deps.Tokenizer,
		tools:     deps.Tools,
		state:     deps.State,
		skills:    deps.Skills,
		logger:    logger,
	}
}

// gatherSkillCatalog returns the current skill catalog for system-prompt
// injection. Returns nil when skills support is disabled. Reload errors
// are logged but never fatal -- a transient filesystem hiccup
// shouldn't kill the agent loop, and the previous catalog stays in
// place until the next attempt.
func (a *Agent) gatherSkillCatalog() []skillsdomain.Skill {
	if a.skills == nil {
		return nil
	}
	if err := a.skills.Reload(); err != nil {
		a.logger.Warn("skills: reload failed; using previous catalog",
			"error", err)
	}
	return a.skills.List()
}

// Close releases the resources owned by the agent. Adapters whose
// lifecycles outlive the agent (the Sandbox, ChatLogger, etc.) are
// closed by the composition root, not here.
func (a *Agent) Close() {
	a.tools.Close()
}

// errShutdown is a sentinel error indicating a graceful shutdown was completed.
var errShutdown = errors.New("graceful shutdown completed")

// Idle-loop detection thresholds. When the model produces back-to-back
// text-only responses (no tool calls, no collaborator input), context grows
// slowly and compaction never fires, so we need a separate signal to break
// out. This was added after observing a real failure mode: a low-information
// continue prompt convinced the model to "stay silent", and it then emitted
// short text-only replies forever, pinned at ~97k tokens.
const (
	// idleNudgeThreshold is the number of consecutive text-only responses
	// after which the normal continue prompt is replaced with a stronger
	// instruction telling the model to call a tool or save state.
	idleNudgeThreshold = 3

	// idleForceCompactionThreshold is the number of consecutive text-only
	// responses after which we force a context compaction regardless of
	// token count. By this point the model is clearly stuck and a fresh
	// rebuild from memories is cheaper than continuing to feed it nudges.
	idleForceCompactionThreshold = 8
)

// idleNudgePrompt is injected in place of the normal continue prompt once
// idleNudgeThreshold consecutive text-only responses have been observed.
// It is more directive than continue.md on purpose: at this point the
// model has demonstrated it is not going to act on a gentle reminder.
const idleNudgePrompt = "[Time: %s]\n\nYou have produced %d text-only responses in a row with no tool calls and no new input from your collaborator. This is a stuck loop. Break out of it now: call a tool. Good options are `context_status` (to see your token usage), `memory_list` with directory `\"\"` (to remember what you have), or `memory_write` to save whatever you were thinking about. Do not reply with another text-only message — that will only deepen the loop."

// toolMessage builds a tool-role chat message satisfying a tool_call_id.
// The body is prefixed with an RFC1123 timestamp so the model has a
// consistent temporal frame for every tool result it sees.
func toolMessage(toolCallID, body string) llm.Message {
	return llm.Message{
		Role:       llm.RoleTool,
		Content:    fmt.Sprintf("[%s]\n%s", time.Now().Format(time.RFC1123), body),
		ToolCallID: toolCallID,
	}
}

// countMessageTokens returns the token count for a message log, using the
// real tokenizer when available and falling back to the heuristic
// otherwise. The tokenizer path uses a short timeout so a slow/failing
// tokenizer never wedges the agent loop.
func (a *Agent) countMessageTokens(messages []llm.Message) int {
	if a.tokenizer == nil {
		return llm.EstimateMessagesTokens(messages)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.tokenizer.CountMessages(ctx, messages)
}

// chatReq builds a ChatRequest pre-populated with the agent's configured
// sampler parameters. Used by every Chat() call site so sampler config
// stays in one place. Messages and tools are caller-supplied because they
// vary per call (main loop, compaction, shutdown save).
func (a *Agent) chatReq(messages []llm.Message, tools []llm.Tool) llm.ChatRequest {
	return llm.ChatRequest{
		Messages:          messages,
		Tools:             tools,
		Temperature:       a.cfg.Agent.Temperature,
		TopP:              a.cfg.Agent.TopP,
		PresencePenalty:   a.cfg.Agent.PresencePenalty,
		FrequencyPenalty:  a.cfg.Agent.FrequencyPenalty,
		Seed:              a.cfg.Agent.Seed,
		MaxTokens:         a.cfg.Agent.MaxRespTokens,
		TopK:              a.cfg.Agent.TopK,
		MinP:              a.cfg.Agent.MinP,
		RepetitionPenalty: a.cfg.Agent.RepetitionPenalty,
	}
}

// abortInFlight asks the backend to stop any currently-running generation.
// Best-effort and bounded by a short timeout: the parent context is already
// canceled (forced shutdown), so we use Background() with our own deadline.
// No-op when no Tokenizer is configured.
func (a *Agent) abortInFlight() {
	if a.tokenizer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.tokenizer.Abort(ctx)
}

// logBackendPerf fetches and logs recent backend performance info. Called
// after each turn so we can spot regressions in prefix-cache reuse: if
// last_process_time suddenly spikes when the conversation only grew by one
// short message, the KV cache was invalidated and we want to know.
//
// Bounded by a short timeout. No-op when no Tokenizer is configured, and
// silently skips on any error so a transient backend hiccup doesn't pollute
// the logs.
func (a *Agent) logBackendPerf() {
	if a.tokenizer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	perf, err := a.tokenizer.Perf(ctx)
	if err != nil || perf == nil {
		return
	}
	a.logger.Info("backend perf",
		"input_tokens", perf.LastInputCount,
		"output_tokens", perf.LastTokenCount,
		"process_time_s", perf.LastProcessTime,
		"eval_time_s", perf.LastEvalTime,
		"process_speed_tps", perf.LastProcessSpd,
		"eval_speed_tps", perf.LastEvalSpd,
		"stop", kobold.StopReasonString(perf.StopReason),
		"queue", perf.Queue,
	)
}

// Run starts the agent's continuous operation loop.
// The agent runs indefinitely in a single conversation. When context reaches
// the compaction threshold, the agent is asked to save state and produce a
// summary, then the context is rebuilt and operation continues seamlessly.
// ctx is canceled only on forced exit (second SIGINT).
// shutdownCh is closed on first SIGINT to trigger graceful save.
func (a *Agent) Run(ctx context.Context, shutdownCh <-chan struct{}) error {
	a.logger.Info("=== agent starting continuous operation ===")

	// Build initial context. When state persistence is enabled and a
	// saved file exists, this resumes the conversation log; otherwise it
	// returns a fresh context. idleStreak is restored from the same file
	// so loop-detection survives restarts.
	messages, prompts, idleStreak, err := a.initializeContext()
	if err != nil {
		return err
	}

	toolDefs := a.tools.ToolDefs()

	// Derive a context for tool execution that cancels on either parent
	// ctx done OR graceful-shutdown signal. The single bridge goroutine
	// below translates a shutdownCh close into a cancellation on toolCtx.
	//
	// LLM Chat calls keep using parent ctx so a generation in flight when
	// shutdown is requested can finish naturally (cutting it off discards
	// model reasoning and wastes tokens). Long-running tool calls -- in
	// particular sleep, but also web_fetch, sandbox runs, embeddings --
	// must yield to graceful shutdown so the save phase reaches quickly.
	//
	// gracefulSave below derives its own saveCtx from parent ctx + a 2 min
	// timeout, so that path is unaffected by toolCtx.
	toolCtx, cancelToolCtx := context.WithCancel(ctx)
	defer cancelToolCtx()
	go func() {
		select {
		case <-ctx.Done():
		case <-shutdownCh:
			cancelToolCtx()
		}
	}()

	// Track the message log length and idle streak at the moment of the
	// last successful save. We only re-save when something has actually
	// changed since then, so an agent sitting on `select` waiting for
	// a collaborator (rare, but possible if the loop short-circuits
	// somehow in the future) doesn't grind the disk for nothing.
	// Length is a sufficient proxy for "did messages change" because
	// the loop only ever appends to the slice between saves.
	lastSavedLen := -1
	lastSavedIdle := -1

	for {
		// Check for shutdown
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-shutdownCh:
			return a.handleShutdown(ctx, messages, toolDefs, prompts)
		default:
		}

		// Inject any collaborator messages that arrived between turns.
		// If any were injected, the model has new input to respond to and
		// is no longer idling.
		var injected bool
		messages, injected = a.injectPendingMessages(messages)
		if injected {
			idleStreak = 0
		}

		// Check if compaction is needed
		tokenEst := a.countMessageTokens(messages)
		if tokenEst > a.cfg.Agent.CompactionThreshold {
			a.logger.Warn("context at compaction threshold, compacting",
				"tokens_est", tokenEst, "threshold", a.cfg.Agent.CompactionThreshold)
			messages, prompts, err = a.compactContext(ctx, toolCtx, messages, toolDefs, prompts)
			if err != nil {
				return err
			}
			idleStreak = 0
			continue
		}

		// Hard safety limit - force compaction even if threshold wasn't hit
		if tokenEst > int(float64(a.cfg.Agent.MaxTokens)*0.95) {
			a.logger.Warn("context at hard limit, forcing compaction",
				"tokens_est", tokenEst, "max", a.cfg.Agent.MaxTokens)
			messages, prompts, err = a.compactContext(ctx, toolCtx, messages, toolDefs, prompts)
			if err != nil {
				return err
			}
			idleStreak = 0
			continue
		}

		// Idle-loop escape hatch. Token-based compaction does not help here
		// because text-only responses are tiny and the conversation can sit
		// well below the threshold for hundreds of turns. After enough
		// consecutive text-only replies, force a rebuild.
		if idleStreak >= idleForceCompactionThreshold {
			a.logger.Warn("idle loop detected, forcing compaction",
				"idle_streak", idleStreak, "tokens_est", tokenEst)
			messages, prompts, err = a.compactContext(ctx, toolCtx, messages, toolDefs, prompts)
			if err != nil {
				return err
			}
			idleStreak = 0
			continue
		}

		// Persist conversation state before the LLM call. This is the
		// only point in the loop where messages is at a clean turn
		// boundary (no half-applied tool calls). A crash between here
		// and the next save loses at most the current turn's work, and
		// the saved log is always valid for replay -- the system message
		// is rebuilt from current prompts on load, so prompt edits also
		// take effect on restart.
		//
		// Skip the write when nothing has changed since the last save
		// (same message count, same idle streak). The loop only ever
		// appends to messages between saves, so length is a sufficient
		// change detector.
		//
		// Errors are logged but not fatal: a transient disk problem
		// should not kill the agent. The StateStore implementation
		// handles the "persistence disabled" case as a no-op internally.
		if len(messages) != lastSavedLen || idleStreak != lastSavedIdle {
			if err := a.state.Save(messages, idleStreak); err != nil {
				a.logger.Error("save state failed", "error", err)
			} else {
				lastSavedLen = len(messages)
				lastSavedIdle = idleStreak
			}
		}

		// Send to LLM. We deliberately let the request run to completion
		// rather than canceling on a collaborator message: cutting a
		// generation off mid-thought wastes tokens and discards the model's
		// reasoning. Any collaborator messages that arrive during generation
		// are handled after the response comes back (see below).
		resp, err := a.chat.Chat(ctx, a.chatReq(messages, toolDefs))
		if err != nil {
			// If the parent context was canceled (forced shutdown via
			// second SIGINT), return the cancellation error verbatim so
			// main.go's errors.Is(err, context.Canceled) filter recognizes
			// it as a clean exit rather than a fatal LLM error. We also
			// best-effort tell the backend to actually stop generating;
			// otherwise the model can keep eating GPU until kcpp realizes
			// the client has gone.
			if ctx.Err() != nil {
				a.abortInFlight()
				return ctx.Err()
			}
			return fmt.Errorf("llm chat: %w", err)
		}

		msg := resp.Choices[0].Message
		messages = append(messages, msg)

		if msg.Content != "" {
			a.logThought(msg.Content)
		}

		// Log backend perf right after the call returns, while the perf
		// counters still reflect this generation. Watch last_process_time:
		// when prefix caching is working it stays low even on huge contexts;
		// a sudden spike means the KV cache was invalidated.
		a.logBackendPerf()

		// Drain any collaborator messages that arrived while the LLM was
		// generating. We will handle them at the next available
		// opportunity rather than mid-generation.
		var pending []string
		if a.operator != nil {
			pending = a.operator.Pending()
		}

		switch {
		case len(msg.ToolCalls) > 0 && len(pending) > 0:
			// A collaborator message arrived while the model wanted to use
			// tools. Defer the tool calls: every tool_call_id must still
			// have a matching tool response or the next API call will fail,
			// so we emit a "deferred" stub for each, then surface the
			// collaborator message. The agent can reply and decide whether
			// the deferred actions are still appropriate.
			a.logger.Info("collaborator message arrived during generation; deferring tool calls",
				"tool_calls", len(msg.ToolCalls), "pending", len(pending))
			const deferredBody = "[Deferred] A message from your collaborator arrived before this tool call could run. Read their message below and reply with send_message first. After replying, re-issue this tool call if it still makes sense, or move on if their message changes your plan."
			for _, tc := range msg.ToolCalls {
				messages = append(messages, toolMessage(tc.ID, deferredBody))
			}
			messages = a.appendCollaboratorMessages(messages, pending)
			// Tool calls + new input both count as the model engaging.
			idleStreak = 0

		case len(msg.ToolCalls) > 0:
			// Normal tool execution path. Uses toolCtx (cancels on
			// graceful shutdown) so long-running tools like sleep
			// yield to a pending shutdown without waiting for ctx
			// cancellation (which only happens on the second SIGINT).
			a.tools.SetContextInfo(a.countMessageTokens(messages))
			for _, tc := range msg.ToolCalls {
				result := a.tools.Execute(toolCtx, tc)
				messages = append(messages, toolMessage(tc.ID, result))
			}
			idleStreak = 0

		case len(pending) > 0:
			// Text-only response with collaborator messages waiting: surface
			// them in place of the continue prompt so the next turn
			// addresses the collaborator naturally. New collaborator input
			// resets the idle counter.
			messages = a.appendCollaboratorMessages(messages, pending)
			idleStreak = 0

		default:
			// Text-only response, nothing to inject. This is the path that
			// can degenerate into an infinite loop if the model decides to
			// "stay silent". Track the streak and escalate the prompt once
			// it crosses idleNudgeThreshold; force compaction higher up
			// the loop once it crosses idleForceCompactionThreshold.
			idleStreak++
			now := time.Now()
			var content string
			if idleStreak >= idleNudgeThreshold {
				a.logger.Warn("idle streak escalating, injecting nudge prompt",
					"idle_streak", idleStreak)
				content = fmt.Sprintf(idleNudgePrompt, now.Format(time.RFC1123), idleStreak)
			} else {
				content = prompt.Render(prompts["continue"], now)
			}
			messages = append(messages, llm.Message{
				Role:    llm.RoleUser,
				Content: content,
			})
		}

		a.logger.Debug("turn complete",
			"messages", len(messages),
			"tokens_est", a.countMessageTokens(messages))
	}
}

// initializeContext builds the initial conversation context.
//
// When state persistence is enabled and a saved state file exists, the
// conversation history is restored from disk; only the system message is
// rebuilt from the current prompt and recent memories so prompt edits take
// effect across restarts. The returned idleStreak is the value at the
// point of the last save (so a daemon that crashed mid-loop resumes its
// loop-detection counters too).
//
// When persistence is disabled or no state file exists, a fresh context
// is built with the standard system prompt + cycle-start user turn.
func (a *Agent) initializeContext() ([]llm.Message, map[string]string, int, error) {
	// Build search index
	docs, err := a.memory.AllFiles()
	if err == nil && len(docs) > 0 {
		a.search.Build(docs)
		a.logger.Info("search index built", "documents", len(docs))
	}

	// Load all mutable prompts from disk (seeding defaults on first run)
	prompts, err := prompt.LoadAll(a.memory)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("load prompts: %w", err)
	}

	// Build a fresh system message from current prompts + recent memories
	// + current skill catalog. This is used both for fresh starts and
	// for replacing the (stale) system message in a restored state file.
	memories := a.gatherContextMemories()
	skillCatalog := a.gatherSkillCatalog()
	now := time.Now()
	fullSystemPrompt := prompt.BuildCycleContext(prompts["system"], memories, skillCatalog, now, a.cfg.Limits.RecentMemoryChars)
	systemMsg := llm.Message{
		Role:    llm.RoleSystem,
		Content: fullSystemPrompt,
	}

	// Try to resume from a saved state file.
	saved, savedIdle, err := a.state.Load()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("load state: %w", err)
	}
	if len(saved) > 0 {
		// Replace the saved system message (which reflects whatever the
		// prompt and memories looked like when the file was saved) with
		// a freshly-built one. Keep everything from index 1 onwards.
		// If the saved log somehow had no system message at index 0,
		// just prepend the fresh one rather than discarding history.
		var resumed []llm.Message
		if saved[0].Role == llm.RoleSystem {
			resumed = append([]llm.Message{systemMsg}, saved[1:]...)
		} else {
			resumed = append([]llm.Message{systemMsg}, saved...)
		}

		a.logger.Info("context resumed from state file",
			"messages", len(resumed),
			"idle_streak", savedIdle,
			"tokens_est", a.countMessageTokens(resumed))
		return resumed, prompts, savedIdle, nil
	}

	// Fresh start: system message + cycle-start user turn.
	messages := []llm.Message{
		systemMsg,
		{
			Role:    llm.RoleUser,
			Content: prompt.Render(prompts["cycle-start"], now),
		},
	}

	a.logger.Info("context initialized",
		"messages", len(messages),
		"tokens_est", a.countMessageTokens(messages))

	return messages, prompts, 0, nil
}

// compactContext performs context compaction: asks the agent to save its state
// and produce a summary, then rebuilds the context with that summary.
//
// The returned prompts map may differ from the one passed in: rebuildContext
// re-reads all prompt files from the memory store, so any edits the agent
// made to its own prompts during compaction take effect on the next turn.
// The compaction prompt itself was already rendered before this loop began,
// so edits to prompts/compaction.md only take effect on the *next* compaction.
//
// ctx is the parent context (used for the LLM Chat calls). toolCtx is the
// tool-execution context derived in Run; it cancels on either ctx done or
// the graceful-shutdown signal so any tools issued during compaction also
// honor a shutdown that arrives mid-compaction.
func (a *Agent) compactContext(ctx context.Context, toolCtx context.Context, messages []llm.Message, toolDefs []llm.Tool, prompts map[string]string) ([]llm.Message, map[string]string, error) {
	a.logger.Info("starting context compaction")

	// Inject compaction prompt
	messages = append(messages, llm.Message{
		Role:    llm.RoleUser,
		Content: prompt.Render(prompts["compaction"], time.Now()),
	})

	var summary string
	const maxCompactionTurns = 10

	for i := 0; i < maxCompactionTurns; i++ {
		// Safety: don't exceed hard token limit during compaction
		tokenEst := a.countMessageTokens(messages)
		if tokenEst > int(float64(a.cfg.Agent.MaxTokens)*0.98) {
			a.logger.Warn("approaching hard limit during compaction, using best available summary")
			break
		}

		resp, err := a.chat.Chat(ctx, a.chatReq(messages, toolDefs))
		if err != nil {
			if ctx.Err() != nil {
				a.abortInFlight()
				return nil, nil, ctx.Err()
			}
			return nil, nil, fmt.Errorf("llm chat during compaction: %w", err)
		}

		msg := resp.Choices[0].Message
		messages = append(messages, msg)

		if msg.Content != "" {
			summary = msg.Content
			a.logThought(msg.Content)
		}

		a.logBackendPerf()

		if len(msg.ToolCalls) > 0 {
			a.tools.SetContextInfo(a.countMessageTokens(messages))

			for _, tc := range msg.ToolCalls {
				result := a.tools.Execute(toolCtx, tc)
				messages = append(messages, toolMessage(tc.ID, result))
			}
		} else {
			// Text-only response - agent is done saving state
			break
		}
	}

	a.logger.Info("compaction complete, rebuilding context", "summary_len", len(summary))
	return a.rebuildContext(summary)
}

// rebuildContext creates a fresh conversation with the system prompt,
// memories, and an optional summary from compaction.
func (a *Agent) rebuildContext(summary string) ([]llm.Message, map[string]string, error) {
	// Rebuild search index in case files changed
	docs, err := a.memory.AllFiles()
	if err == nil && len(docs) > 0 {
		a.search.Build(docs)
	}

	// Reload prompts (agent may have modified them)
	prompts, err := prompt.LoadAll(a.memory)
	if err != nil {
		return nil, nil, fmt.Errorf("load prompts: %w", err)
	}

	// Load fresh memories and refresh the skill catalog so any skills
	// the operator has dropped in since the last cycle become visible.
	memories := a.gatherContextMemories()
	skillCatalog := a.gatherSkillCatalog()
	now := time.Now()
	fullSystemPrompt := prompt.BuildCycleContext(prompts["system"], memories, skillCatalog, now, a.cfg.Limits.RecentMemoryChars)

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: fullSystemPrompt},
	}

	if summary != "" {
		messages = append(messages, llm.Message{
			Role: llm.RoleUser,
			Content: fmt.Sprintf("[Context Compacted - %s]\n\nYour context was compacted. Here is your summary from before compaction:\n\n%s",
				now.Format(time.RFC1123), summary),
		})
	} else {
		messages = append(messages, llm.Message{
			Role:    llm.RoleUser,
			Content: prompt.Render(prompts["continue"], now),
		})
	}

	a.logger.Info("context rebuilt",
		"messages", len(messages),
		"tokens_est", a.countMessageTokens(messages))

	return messages, prompts, nil
}

// isOperationalFile returns true for files that are loaded separately
// (prompts, trash) and should not be surfaced as memories.
func isOperationalFile(path string) bool {
	if strings.HasPrefix(path, "prompts/") {
		return true
	}
	if fs.IsTrashPath(path) {
		return true
	}
	return false
}

// contextMemoryCount is the maximum number of recent memory files surfaced
// in the system prompt. Kept low to bound the prompt size; each entry is
// also content-truncated by BuildCycleContext.
const contextMemoryCount = 5

// gatherContextMemories finds relevant memories to include in context.
// Returns up to contextMemoryCount most-recently-modified non-operational files.
func (a *Agent) gatherContextMemories() []bm25.Result {
	// Request more than we need: operational files (prompts/, trash) are
	// filtered out below, and we want to land at contextMemoryCount real
	// memories whenever that many exist on disk.
	recent, err := a.memory.RecentFiles(contextMemoryCount * 4)
	if err != nil {
		return nil
	}

	results := make([]bm25.Result, 0, contextMemoryCount)
	for _, r := range recent {
		if isOperationalFile(r.Path) {
			continue
		}
		results = append(results, r)
		if len(results) >= contextMemoryCount {
			break
		}
	}
	return results
}

// injectPendingMessages checks for queued collaborator messages and appends
// them to the conversation. Returns the updated messages and whether any
// were injected.
func (a *Agent) injectPendingMessages(messages []llm.Message) ([]llm.Message, bool) {
	if a.operator == nil {
		return messages, false
	}

	pending := a.operator.Pending()
	if len(pending) == 0 {
		return messages, false
	}

	return a.appendCollaboratorMessages(messages, pending), true
}

// appendCollaboratorMessages formats each collaborator message as a user
// turn and appends them to the conversation. Used by both the between-turn
// injector and the post-response handler when messages arrive during
// generation.
func (a *Agent) appendCollaboratorMessages(messages []llm.Message, pending []string) []llm.Message {
	for _, text := range pending {
		a.logger.Info("injecting collaborator message into conversation", "text", text)
		messages = append(messages, llm.Message{
			Role: llm.RoleUser,
			Content: fmt.Sprintf("[Collaborator message - %s]\n\nYour collaborator says: %s\n\nReply with send_message before continuing. If their message changes what you should do next, adjust your plan accordingly; otherwise resume where you left off.",
				time.Now().Format(time.RFC1123), text),
		})
	}
	return messages
}

// handleShutdown wraps gracefulSave and translates errShutdown to nil.
func (a *Agent) handleShutdown(ctx context.Context, messages []llm.Message, toolDefs []llm.Tool, prompts map[string]string) error {
	err := a.gracefulSave(ctx, messages, toolDefs, prompts)
	if errors.Is(err, errShutdown) {
		return nil
	}
	return err
}

// gracefulSave gives the agent a limited number of turns to save its state
// before the process exits. Uses a 2-minute timeout.
func (a *Agent) gracefulSave(ctx context.Context, messages []llm.Message, toolDefs []llm.Tool, prompts map[string]string) error {
	a.logger.Info("graceful shutdown: asking agent to save state")

	saveCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Load the mutable shutdown prompt
	shutdownPrompt := prompts["shutdown"]
	messages = append(messages, llm.Message{
		Role:    llm.RoleUser,
		Content: prompt.Render(shutdownPrompt, time.Now()),
	})

	const maxSaveTurns = 10
	for i := 0; i < maxSaveTurns; i++ {
		resp, err := a.chat.Chat(saveCtx, a.chatReq(messages, toolDefs))
		if err != nil {
			a.logger.Error("LLM call failed during save", "error", err)
			return errShutdown
		}

		msg := resp.Choices[0].Message
		messages = append(messages, msg)

		if msg.Content != "" {
			a.logThought(msg.Content)
		}

		a.logBackendPerf()

		if len(msg.ToolCalls) > 0 {
			a.tools.SetContextInfo(a.countMessageTokens(messages))
			for _, tc := range msg.ToolCalls {
				result := a.tools.Execute(saveCtx, tc)
				messages = append(messages, toolMessage(tc.ID, result))
			}
		} else {
			// No tool calls - agent is done saving
			a.logger.Info("agent finished saving state (no more tool calls)")
			return errShutdown
		}
	}

	a.logger.Info("save turn limit reached, shutting down")
	return errShutdown
}

// logThought prints the agent's thought with some formatting.
func (a *Agent) logThought(content string) {
	// Show a preview in structured log
	preview := content
	if len(preview) > 300 {
		preview = preview[:300] + "..."
	}
	a.logger.Info("agent", "thought", preview)

	// Also print the full thought to stdout for live monitoring
	fmt.Println()
	fmt.Println(content)
	fmt.Println()
}
