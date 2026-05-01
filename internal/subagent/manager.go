package subagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Manager owns the live subagent registry and the report inbox the
// primary agent drains alongside operator messages.
//
// Lifecycle:
//   - constructed once in cmd/faultline/main.go with the operator's
//     [subagent] config, the synthesized default profile, and a
//     SpawnFunc that knows how to construct + run a child agent.
//   - the agent loop calls Pending() / HasPending() to drain async
//     reports, parallel to the operator queue.
//   - the tools layer calls Run() / Spawn() / Status() / Cancel() to
//     drive the four subagent_* tools.
//   - on parent shutdown, CancelAll() fires; in-flight goroutines
//     observe their canceled context and return promptly.
//
// The Manager is safe for concurrent use. The internal map and
// pending slice are mutex-guarded; goroutines launched per Run/Spawn
// touch only their own activeRun and the inbox.
type Manager struct {
	logger *slog.Logger

	profiles   []Profile
	profileMap map[string]Profile

	spawn SpawnFunc

	maxConcurrent  int
	maxInbox       int
	maxTurnsPerRun int
	runTimeout     time.Duration

	mu      sync.Mutex
	active  map[string]*activeRun
	pending []Report
}

// SpawnFunc is the bridge from domain to composition. It constructs
// and runs a child agent loop with the supplied profile and prompt,
// blocking until the child terminates (via subagent_report, turn cap,
// timeout, or context cancellation). The returned Report's WorkID
// must equal the workID argument; other fields are populated from
// the child's behavior.
//
// SpawnFunc must respect ctx: when ctx is canceled, the child's
// in-flight LLM call should abort and the child loop should exit
// promptly. The Manager wraps the parent's context with the
// configured run timeout before passing it in.
type SpawnFunc func(ctx context.Context, workID string, profile Profile, prompt string, maxTurns int) Report

// activeRun is the per-subagent bookkeeping the Manager holds while
// a child is running. result and done are set/closed exactly once,
// when the spawn goroutine returns.
type activeRun struct {
	workID  string
	profile string
	prompt  string
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}
	result  Report
	async   bool
}

// Config wraps the runtime parameters Manager needs. Mirrors
// config.SubagentConfig minus the on-disk representation; main.go
// translates one to the other.
type Config struct {
	MaxConcurrent  int
	MaxTurnsPerRun int
	MaxInbox       int
	RunTimeout     time.Duration
}

// New constructs a Manager. profiles must include the synthesized
// "default" profile; main.go is responsible for building it from
// the primary's [api] config. If two profiles share a name, the
// later one wins and a warning is logged (composition decision; the
// Manager does not reject duplicates).
func New(cfg Config, profiles []Profile, spawn SpawnFunc, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.MaxTurnsPerRun <= 0 {
		cfg.MaxTurnsPerRun = 50
	}
	if cfg.MaxInbox <= 0 {
		cfg.MaxInbox = 32
	}
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = 30 * time.Minute
	}

	pm := make(map[string]Profile, len(profiles))
	for _, p := range profiles {
		if _, exists := pm[p.Name]; exists {
			logger.Warn("duplicate subagent profile name; later definition wins", "name", p.Name)
		}
		pm[p.Name] = p
	}

	return &Manager{
		logger:         logger,
		profiles:       profiles,
		profileMap:     pm,
		spawn:          spawn,
		maxConcurrent:  cfg.MaxConcurrent,
		maxTurnsPerRun: cfg.MaxTurnsPerRun,
		maxInbox:       cfg.MaxInbox,
		runTimeout:     cfg.RunTimeout,
		active:         make(map[string]*activeRun),
	}
}

// Profiles returns the configured profiles in deterministic order
// (the order they were passed to New). Used by the prompts package
// to render the catalog into the primary's system prompt.
func (m *Manager) Profiles() []Profile {
	out := make([]Profile, len(m.profiles))
	copy(out, m.profiles)
	return out
}

// FindProfile returns the named profile and ok=true, or zero+false
// if no such profile exists.
func (m *Manager) FindProfile(name string) (Profile, bool) {
	p, ok := m.profileMap[name]
	return p, ok
}

// MaxTurnsPerRun is exposed to the SpawnFunc closure so the child
// agent can cap its own loop iterations.
func (m *Manager) MaxTurnsPerRun() int { return m.maxTurnsPerRun }

// Pending drains and returns all queued completion Reports. Mirrors
// the telegram.Bot contract so the agent loop can treat both queues
// the same way.
func (m *Manager) Pending() []Report {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 0 {
		return nil
	}
	out := m.pending
	m.pending = nil
	return out
}

// HasPending reports whether any reports are queued, without
// draining them.
func (m *Manager) HasPending() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending) > 0
}

// Status returns a snapshot of currently running subagents.
func (m *Manager) Status() []ActiveStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ActiveStatus, 0, len(m.active))
	for _, ar := range m.active {
		out = append(out, ActiveStatus{
			WorkID:  ar.workID,
			Profile: ar.profile,
			Prompt:  truncate(ar.prompt, 200),
			Started: ar.started,
			Async:   ar.async,
		})
	}
	return out
}

// ActiveCount returns the number of currently running subagents.
// Used for cap enforcement and the context_status tool.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// Wait blocks until the named subagent's report arrives, ctx cancels,
// or the subagent is unknown. The returned bool reports whether the
// report was found; on true the report is also removed from the
// inbox so a subsequent Pending() drain won't see it twice.
//
// Lookup order:
//  1. Already-pending? Take and return immediately.
//  2. Currently-active? Block on its done channel, then take.
//  3. Neither? Return found=false with an error.
func (m *Manager) Wait(ctx context.Context, workID string) (Report, bool, error) {
	m.mu.Lock()
	if r, ok := m.takePendingLocked(workID); ok {
		m.mu.Unlock()
		return r, true, nil
	}
	ar, alive := m.active[workID]
	m.mu.Unlock()

	if !alive {
		return Report{}, false, fmt.Errorf("no subagent with work_id %q (already drained or never existed?)", workID)
	}

	select {
	case <-ar.done:
		// fall through
	case <-ctx.Done():
		return Report{}, false, ctx.Err()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.takePendingLocked(workID); ok {
		return r, true, nil
	}
	// runOne always appends to pending before closing done for async
	// runs; for sync runs it doesn't, so fall back to ar.result.
	return ar.result, true, nil
}

// takePendingLocked removes and returns the pending report with the
// given workID. Caller must hold m.mu.
func (m *Manager) takePendingLocked(workID string) (Report, bool) {
	for i, r := range m.pending {
		if r.WorkID == workID {
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			return r, true
		}
	}
	return Report{}, false
}

// Cancel aborts the named subagent. Safe to call multiple times;
// the second call returns an error (no such workID) because the
// goroutine has already cleaned up. The cancellation propagates via
// the child's context; the spawnFn must return promptly when ctx
// is done.
func (m *Manager) Cancel(workID string) error {
	m.mu.Lock()
	ar, ok := m.active[workID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no active subagent with work_id %q", workID)
	}
	ar.cancel()
	return nil
}

// CancelAll fires Cancel on every active subagent. Used by the
// agent's gracefulSave path. Does not wait for goroutines to finish;
// the caller is responsible for that if it cares.
func (m *Manager) CancelAll() {
	m.mu.Lock()
	for _, ar := range m.active {
		ar.cancel()
	}
	m.mu.Unlock()
}

// Run executes a subagent synchronously: blocks until the child
// terminates and returns the resulting Report. Used by the
// subagent_run tool, which wants the report inline as the tool's
// output.
//
// The parent's ctx is honored: if it cancels, the child's ctx
// cancels and Run returns the partial Report with Canceled=true.
func (m *Manager) Run(ctx context.Context, profileName, prompt string) (Report, error) {
	return m.start(ctx, profileName, prompt, false)
}

// Spawn launches a subagent asynchronously. Returns the workID
// immediately; the report is delivered to the inbox when the child
// terminates. Used by the subagent_spawn tool.
func (m *Manager) Spawn(ctx context.Context, profileName, prompt string) (string, error) {
	report, err := m.start(ctx, profileName, prompt, true)
	if err != nil {
		return "", err
	}
	return report.WorkID, nil
}

// start is the shared launch path. For sync runs (async=false) it
// blocks waiting for the goroutine to finish and returns the Report.
// For async runs it returns a stub Report carrying just the workID
// and lets the goroutine deliver to the inbox.
func (m *Manager) start(ctx context.Context, profileName, prompt string, async bool) (Report, error) {
	profile, ok := m.FindProfile(profileName)
	if !ok {
		return Report{}, fmt.Errorf("unknown subagent profile %q", profileName)
	}
	if prompt == "" {
		return Report{}, errors.New("subagent prompt is empty")
	}

	workID, err := newWorkID()
	if err != nil {
		return Report{}, fmt.Errorf("allocate work_id: %w", err)
	}

	// Cap async concurrent subagents. Sync runs are not capped:
	// the parent's tool dispatch is blocked waiting for the result,
	// so there is at most one sync child per parent agent.
	if async {
		m.mu.Lock()
		asyncCount := 0
		for _, ar := range m.active {
			if ar.async {
				asyncCount++
			}
		}
		if asyncCount >= m.maxConcurrent {
			m.mu.Unlock()
			return Report{}, fmt.Errorf("max_concurrent (%d) async subagents already running", m.maxConcurrent)
		}
		m.mu.Unlock()
	}

	childCtx, cancel := context.WithCancel(ctx)
	if m.runTimeout > 0 {
		childCtx, cancel = context.WithTimeout(ctx, m.runTimeout)
	}

	ar := &activeRun{
		workID:  workID,
		profile: profile.Name,
		prompt:  prompt,
		started: time.Now(),
		cancel:  cancel,
		done:    make(chan struct{}),
		async:   async,
	}

	m.mu.Lock()
	m.active[workID] = ar
	m.mu.Unlock()

	go m.runOne(childCtx, ar, profile, prompt)

	if async {
		return Report{WorkID: workID, Profile: profile.Name, StartedAt: ar.started}, nil
	}

	// Sync: block on the goroutine. Honor parent ctx so the tool
	// dispatch returns promptly if the parent shuts down.
	select {
	case <-ar.done:
		return ar.result, nil
	case <-ctx.Done():
		ar.cancel()
		<-ar.done
		return ar.result, nil
	}
}

// runOne is the per-subagent goroutine body. Calls the SpawnFunc,
// captures the Report, removes from active, and either delivers to
// the inbox (async) or signals the sync caller (waiting on done).
func (m *Manager) runOne(ctx context.Context, ar *activeRun, profile Profile, prompt string) {
	defer ar.cancel() // release timeout context resources
	defer close(ar.done)

	report := m.spawn(ctx, ar.workID, profile, prompt, m.maxTurnsPerRun)
	report.WorkID = ar.workID
	report.Profile = profile.Name
	report.StartedAt = ar.started
	if report.FinishedAt.IsZero() {
		report.FinishedAt = time.Now()
	}
	ar.result = report

	m.mu.Lock()
	delete(m.active, ar.workID)
	if ar.async {
		// Drop oldest if inbox is full. Better to lose the eldest
		// report than refuse new ones; the parent has clearly
		// fallen behind on draining.
		for len(m.pending) >= m.maxInbox {
			dropped := m.pending[0]
			m.pending = m.pending[1:]
			m.logger.Warn("subagent inbox full; dropping oldest report",
				"dropped_work_id", dropped.WorkID,
				"dropped_profile", dropped.Profile,
				"max_inbox", m.maxInbox,
			)
		}
		m.pending = append(m.pending, report)
	}
	m.mu.Unlock()

	m.logger.Info("subagent finished",
		"work_id", ar.workID,
		"profile", profile.Name,
		"async", ar.async,
		"canceled", report.Canceled,
		"truncated", report.Truncated,
		"err", report.Err,
		"elapsed", time.Since(ar.started),
	)
}

// newWorkID allocates a 12-hex-char identifier with the "sub-" prefix.
// Matches the skill_execute work_id format for visual consistency.
func newWorkID() (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "sub-" + hex.EncodeToString(buf[:]), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
