// Package update polls GitHub releases for newer faultline versions,
// downloads and verifies new release binaries, atomically swaps them
// into place, and signals the agent loop to shut down so the new
// binary takes effect.
//
// All decisions are baked into the code -- the LLM does not drive
// updates. The agent's update_check / update_apply tools just kick
// off the same code path the polling goroutine runs.
package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/matjam/faultline/internal/version"
)

// Memory is the subset of the agent's memory store the updater uses.
// *fs.Store satisfies it. Defined here so the updater package doesn't
// import the memory adapter.
type Memory interface {
	Append(path, content string) error
}

// Result describes the outcome of a successful apply. The agent loop
// receives a pointer to this through its shutdown channel, and main
// uses it to decide what to do post-shutdown (exit / self-exec /
// command).
type Result struct {
	FromVersion string
	ToVersion   string
	BinaryPath  string // absolute path of the now-installed new binary
	AppliedAt   time.Time
}

// State is a snapshot of the updater's view of the world. Returned by
// Check and State() for the update_check tool.
type State struct {
	LastChecked     time.Time
	CurrentVersion  string
	LatestVersion   string // empty if no eligible release found
	UpdateAvailable bool
	Note            string // human-readable; empty unless something noteworthy happened
	Err             error  // non-nil when the last check failed
}

// Config bundles the runtime knobs. Mirrors config.UpdateConfig but
// uses concrete time.Duration so the update package doesn't import the
// config package.
type Config struct {
	Enabled         bool
	CheckInterval   time.Duration
	GitHubRepo      string
	AllowPrerelease bool
	BinaryPath      string
	// RestartMode and RestartCommand are read by main.go after Result
	// returns; the updater itself doesn't run them.
}

// TriggerShutdown is the callback main.go provides so the updater can
// initiate graceful shutdown. The Result tells main which restart mode
// to dispatch to. nil means "shut down without an update reason"
// (used for signal-driven shutdown elsewhere; updater never passes
// nil).
type TriggerShutdown func(*Result)

// Updater orchestrates polling and applies. Constructed once in main,
// shared with tools via its public Check/Apply methods.
type Updater struct {
	cfg     Config
	logger  *slog.Logger
	memory  Memory
	gh      *githubClient
	trigger TriggerShutdown

	// state is the most recent Check result. atomic.Pointer so
	// State() and Check() callers don't need a lock.
	state atomic.Pointer[State]

	// mu serializes apply attempts. Polling and operator-triggered
	// applies funnel through here so two updates can't race.
	mu sync.Mutex

	// applied is set once an apply has succeeded. After this point
	// further apply attempts no-op until restart, because the binary
	// has already been swapped and we're about to shut down.
	applied atomic.Bool
}

// New constructs an Updater. cfg.Enabled = false is allowed; tools
// querying State() still work, but Run, Check, and Apply are no-ops.
func New(cfg Config, memory Memory, trigger TriggerShutdown, logger *slog.Logger) *Updater {
	return &Updater{
		cfg:     cfg,
		logger:  logger,
		memory:  memory,
		gh:      newGitHubClient(cfg.GitHubRepo),
		trigger: trigger,
	}
}

// Enabled reports whether the updater will do work. Tools use this to
// decide whether to advertise update_check / update_apply.
func (u *Updater) Enabled() bool { return u.cfg.Enabled }

// CurrentVersion returns the version baked into this binary at build
// time. Convenience wrapper for the get_version tool.
func (u *Updater) CurrentVersion() string { return u.currentVersion() }

func (u *Updater) currentVersion() string { return version.Version }

// State returns the last cached check result without doing I/O.
// Returns a zero State (with empty version fields) if no check has
// run yet.
func (u *Updater) State() State {
	if s := u.state.Load(); s != nil {
		return *s
	}
	return State{CurrentVersion: u.currentVersion()}
}

// Run starts the polling loop. Blocks until ctx is canceled. Safe to
// call when Enabled() is false; returns immediately in that case.
func (u *Updater) Run(ctx context.Context) {
	if !u.cfg.Enabled {
		u.logger.Info("updater disabled, skipping poll loop")
		return
	}
	u.logger.Info("updater started",
		"repo", u.cfg.GitHubRepo,
		"interval", u.cfg.CheckInterval,
		"current_version", u.currentVersion(),
	)

	// First check happens after a short delay rather than immediately,
	// to avoid hammering GitHub in tight start/restart loops.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if u.applied.Load() {
				return // an apply already triggered shutdown; stop polling
			}
			// Errors here are already logged inside checkAndMaybeApply
			// via u.logger; the polling loop just retries on the next
			// tick.
			_, _ = u.checkAndMaybeApply(ctx, false)
			timer.Reset(u.cfg.CheckInterval)
		}
	}
}

// Check forces an immediate poll of GitHub releases without applying
// anything. Used by the update_check tool. Updates State() with the
// result.
func (u *Updater) Check(ctx context.Context) State {
	if !u.cfg.Enabled {
		return State{
			CurrentVersion: u.currentVersion(),
			Note:           "updater disabled in config",
		}
	}
	return u.check(ctx)
}

// Apply forces an immediate apply attempt. Used by the update_apply
// tool. Returns the Result on success; an error on failure or when
// no update is available.
func (u *Updater) Apply(ctx context.Context) (*Result, error) {
	if !u.cfg.Enabled {
		return nil, errors.New("updater is disabled in config")
	}
	if u.applied.Load() {
		return nil, errors.New("an update was already applied this session; agent is shutting down")
	}
	return u.checkAndMaybeApply(ctx, true)
}

// check performs the GitHub fetch and updates u.state. Returns the
// State that was stored.
func (u *Updater) check(ctx context.Context) State {
	now := time.Now()
	rel, err := u.gh.Latest(ctx, u.cfg.AllowPrerelease)
	s := State{
		LastChecked:    now,
		CurrentVersion: u.currentVersion(),
	}
	if err != nil {
		s.Err = err
		s.Note = fmt.Sprintf("check failed: %s", err)
		u.logger.Warn("update check failed", "error", err)
		u.state.Store(&s)
		return s
	}
	if rel == nil {
		s.Note = "no eligible release available"
		u.state.Store(&s)
		return s
	}
	s.LatestVersion = rel.TagName
	s.UpdateAvailable = IsNewer(rel.TagName, u.currentVersion())
	if s.UpdateAvailable {
		s.Note = fmt.Sprintf("update available: %s", rel.TagName)
	} else {
		s.Note = "up to date"
	}
	u.state.Store(&s)
	return s
}

// checkAndMaybeApply runs a check and, if force=true OR a newer
// version is available during a scheduled poll, runs apply. Returns
// the apply Result when it ran; returns (nil, nil) for a check that
// did not need to apply; returns (nil, err) on apply failure.
func (u *Updater) checkAndMaybeApply(ctx context.Context, force bool) (*Result, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.applied.Load() {
		return nil, errors.New("update already applied; pending restart")
	}

	state := u.check(ctx)
	if state.Err != nil {
		return nil, state.Err
	}
	if !state.UpdateAvailable && !force {
		return nil, nil
	}
	if !state.UpdateAvailable && force {
		return nil, fmt.Errorf("no update available (current: %s, latest: %s)",
			state.CurrentVersion, state.LatestVersion)
	}

	// Re-fetch the full release for asset URLs. Latest() already gave
	// us one, but we need a Release pointer in scope for apply().
	rel, err := u.gh.Latest(ctx, u.cfg.AllowPrerelease)
	if err != nil {
		return nil, fmt.Errorf("re-fetch release for apply: %w", err)
	}
	if rel == nil {
		return nil, errors.New("release disappeared between check and apply")
	}

	u.logger.Info("applying update",
		"from", state.CurrentVersion,
		"to", rel.TagName,
		"asset", AssetName(rel.TagName))

	res, err := u.apply(ctx, rel)
	if err != nil {
		u.logger.Error("update apply failed", "error", err)
		return nil, err
	}
	res.AppliedAt = time.Now()

	u.recordHistory(res)
	u.applied.Store(true)

	u.logger.Info("update applied; signaling shutdown",
		"from", res.FromVersion,
		"to", res.ToVersion)
	if u.trigger != nil {
		u.trigger(res)
	}
	return res, nil
}

// recordHistory appends a one-paragraph entry to meta/version-history.md
// in the memory store so the agent (post-restart) can discover that
// it just updated. Best-effort; log on failure but don't roll back the
// update for it.
func (u *Updater) recordHistory(res *Result) {
	if u.memory == nil {
		return
	}
	entry := fmt.Sprintf(`
## %s -> %s (%s)

- Applied: %s
- Binary: %s
`, res.FromVersion, res.ToVersion, res.AppliedAt.UTC().Format(time.RFC3339),
		res.AppliedAt.UTC().Format(time.RFC1123),
		res.BinaryPath)
	if err := u.memory.Append("meta/version-history.md", entry); err != nil {
		u.logger.Warn("could not append to version-history.md", "error", err)
	}
}
