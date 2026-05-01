package agent

import (
	"sync"
	"time"
)

// Phase is a coarse-grained label for what the agent loop is doing
// right now. Surfaced via Snapshot for the admin UI's status panel.
//
// Transitions happen at well-defined points in Run; each transition
// updates phaseSince, so the UI can show "generating since N seconds
// ago" for long LLM calls and "idle since N seconds ago" while the
// loop is waiting between turns.
type Phase string

const (
	// PhaseInitializing covers initializeContext: prompts loading,
	// state restore, search index build. Brief, only at startup.
	PhaseInitializing Phase = "initializing"

	// PhaseIdle is the steady-state between iterations when the
	// agent has nothing to do (e.g. after the model returns a
	// content-only response and we're about to inject the continue
	// prompt).
	PhaseIdle Phase = "idle"

	// PhaseGenerating means a chat.Chat call is in flight.
	PhaseGenerating Phase = "generating"

	// PhaseExecutingTool means the agent is dispatching tool
	// calls returned by the last LLM response.
	PhaseExecutingTool Phase = "executing-tool"

	// PhaseCompacting means context compaction is running: the
	// agent has been asked to summarize, save, and rebuild context.
	PhaseCompacting Phase = "compacting"

	// PhaseSaving covers the graceful-shutdown save phase
	// (handleShutdown / gracefulSave).
	PhaseSaving Phase = "saving"

	// PhaseStopped is the post-Run-return state. Run() sets this
	// just before returning so observers can distinguish a clean
	// exit from an active session.
	PhaseStopped Phase = "stopped"
)

// AgentSnapshot is an immutable point-in-time view of the agent's
// observable state. Returned by Agent.Snapshot; safe to share with
// non-loop goroutines (e.g. the admin HTTP server).
//
// Field semantics are documented inline. Counters are cumulative
// across the whole process lifetime; "Last…" fields refer to the
// most recent event of that kind.
type AgentSnapshot struct {
	// StartedAt is the wall-clock at Agent.New (or whenever the
	// loop was constructed). Used to compute uptime.
	StartedAt time.Time

	// Phase is the current loop phase.
	Phase Phase

	// PhaseSince is the wall-clock at which Phase was set. The
	// difference (now - PhaseSince) is the time spent in the
	// current phase; useful for "generating since 12s" UI.
	PhaseSince time.Time

	// MessageCount is len(messages) at last update — the size of
	// the live conversation log including the system prompt.
	MessageCount int

	// TokenEstimate is the most recent context-token count seen
	// by the agent (updated at the top of each loop iteration).
	TokenEstimate int

	// MaxTokens is the configured hard ceiling; same value the
	// agent uses to decide when to force compaction.
	MaxTokens int

	// CompactionThreshold is the soft compaction trigger from
	// config. Useful for UI progress bars.
	CompactionThreshold int

	// IdleStreak is the count of consecutive content-only
	// responses with no tool calls. Drives the nudge / forced
	// compaction logic.
	IdleStreak int

	// TotalChats is the cumulative count of LLM /chat/completions
	// requests this process has issued (across all turns).
	TotalChats int64

	// TotalPromptTokens / TotalCompletionTokens accumulate the
	// usage figures returned by the chat backend. Backends that
	// don't report usage contribute zero.
	TotalPromptTokens     int64
	TotalCompletionTokens int64

	// TotalToolCalls is the cumulative count of tools dispatched.
	// Unlike the per-Executor ring buffer this is a counter, not
	// a list.
	TotalToolCalls int64

	// LastChatAt is the wall-clock when the last chat call
	// completed (regardless of success).
	LastChatAt time.Time

	// LastChatLatency is the wall-clock duration of the most
	// recent chat call.
	LastChatLatency time.Duration

	// LastChatPromptTokens / LastChatCompletionTokens are the
	// usage figures from the most recent call. Zero when the
	// backend doesn't report usage.
	LastChatPromptTokens     int
	LastChatCompletionTokens int

	// LastFinishReason is the OpenAI-spec finish_reason from the
	// last chat call ("stop", "tool_calls", "length", etc.).
	LastFinishReason string

	// LastError is the formatted message of the most recent
	// non-recovered error, empty when the loop is healthy.
	// LastErrorAt is the wall-clock when it was recorded.
	LastError   string
	LastErrorAt time.Time

	// PendingOperator is the count of unread operator queue
	// entries observed at the most recent loop iteration top.
	PendingOperator int

	// ActiveSubagents is the count of currently-running spawned
	// children, observed at the most recent injection point.
	ActiveSubagents int
}

// inspectorState carries everything the snapshot exposes, guarded by
// a small RWMutex. Reads happen at most a few times per second from
// the admin server's polling endpoints; writes happen once per
// transition. Mutex is the right tool here — atomics would force
// per-field reads that can produce torn snapshots.
type inspectorState struct {
	mu sync.RWMutex

	startedAt time.Time

	phase      Phase
	phaseSince time.Time

	messageCount  int
	tokenEstimate int
	idleStreak    int

	totalChats            int64
	totalPromptTokens     int64
	totalCompletionTokens int64
	totalToolCalls        int64

	lastChatAt               time.Time
	lastChatLatency          time.Duration
	lastChatPromptTokens     int
	lastChatCompletionTokens int
	lastFinishReason         string

	lastError   string
	lastErrorAt time.Time

	pendingOperator int
	activeSubagents int
}

// newInspectorState constructs the per-Agent inspector state. Phase
// defaults to PhaseInitializing so a snapshot taken before Run begins
// reflects that we're not yet idle.
func newInspectorState(now time.Time) *inspectorState {
	return &inspectorState{
		startedAt:  now,
		phase:      PhaseInitializing,
		phaseSince: now,
	}
}

// Snapshot returns an AgentSnapshot reflecting the current observable
// state. Safe to call from any goroutine. The agent loop never blocks
// on a Snapshot call — readers acquire only the RLock.
func (a *Agent) Snapshot() AgentSnapshot {
	if a == nil || a.inspector == nil {
		return AgentSnapshot{}
	}
	s := a.inspector
	s.mu.RLock()
	defer s.mu.RUnlock()
	return AgentSnapshot{
		StartedAt:                s.startedAt,
		Phase:                    s.phase,
		PhaseSince:               s.phaseSince,
		MessageCount:             s.messageCount,
		TokenEstimate:            s.tokenEstimate,
		MaxTokens:                a.cfg.Agent.MaxTokens,
		CompactionThreshold:      a.cfg.Agent.CompactionThreshold,
		IdleStreak:               s.idleStreak,
		TotalChats:               s.totalChats,
		TotalPromptTokens:        s.totalPromptTokens,
		TotalCompletionTokens:    s.totalCompletionTokens,
		TotalToolCalls:           s.totalToolCalls,
		LastChatAt:               s.lastChatAt,
		LastChatLatency:          s.lastChatLatency,
		LastChatPromptTokens:     s.lastChatPromptTokens,
		LastChatCompletionTokens: s.lastChatCompletionTokens,
		LastFinishReason:         s.lastFinishReason,
		LastError:                s.lastError,
		LastErrorAt:              s.lastErrorAt,
		PendingOperator:          s.pendingOperator,
		ActiveSubagents:          s.activeSubagents,
	}
}

// setPhase records a phase transition. now() is taken inside the
// lock so the resulting (phase, phaseSince) pair is internally
// consistent.
func (a *Agent) setPhase(p Phase) {
	if a.inspector == nil {
		return
	}
	a.inspector.mu.Lock()
	a.inspector.phase = p
	a.inspector.phaseSince = time.Now()
	a.inspector.mu.Unlock()
}

// recordChat updates the per-call and cumulative chat-usage fields.
// Always called from the agent loop after a chat.Chat returns,
// regardless of error. promptTokens / completionTokens may be zero
// for backends that don't report usage.
func (a *Agent) recordChat(latency time.Duration, promptTokens, completionTokens int, finish string, err error) {
	if a.inspector == nil {
		return
	}
	now := time.Now()
	a.inspector.mu.Lock()
	defer a.inspector.mu.Unlock()
	a.inspector.totalChats++
	a.inspector.lastChatAt = now
	a.inspector.lastChatLatency = latency
	a.inspector.lastChatPromptTokens = promptTokens
	a.inspector.lastChatCompletionTokens = completionTokens
	a.inspector.lastFinishReason = finish
	if promptTokens > 0 {
		a.inspector.totalPromptTokens += int64(promptTokens)
	}
	if completionTokens > 0 {
		a.inspector.totalCompletionTokens += int64(completionTokens)
	}
	if err != nil {
		a.inspector.lastError = err.Error()
		a.inspector.lastErrorAt = now
	}
}

// recordIterationTop is called at the top of each loop iteration with
// the per-iteration observable state.
func (a *Agent) recordIterationTop(messageCount, tokenEstimate, idleStreak, pendingOperator, activeSubagents int) {
	if a.inspector == nil {
		return
	}
	a.inspector.mu.Lock()
	a.inspector.messageCount = messageCount
	a.inspector.tokenEstimate = tokenEstimate
	a.inspector.idleStreak = idleStreak
	a.inspector.pendingOperator = pendingOperator
	a.inspector.activeSubagents = activeSubagents
	a.inspector.mu.Unlock()
}

// recordToolCall bumps the cumulative tool-call counter. The detailed
// per-call ring buffer lives in the tools package's Observer hook;
// this is only the global tally.
func (a *Agent) recordToolCall() {
	if a.inspector == nil {
		return
	}
	a.inspector.mu.Lock()
	a.inspector.totalToolCalls++
	a.inspector.mu.Unlock()
}

// recordError records a non-recovered error visible to the caller.
// Used for paths that don't go through recordChat.
func (a *Agent) recordError(err error) {
	if a.inspector == nil || err == nil {
		return
	}
	a.inspector.mu.Lock()
	a.inspector.lastError = err.Error()
	a.inspector.lastErrorAt = time.Now()
	a.inspector.mu.Unlock()
}
