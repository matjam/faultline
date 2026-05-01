package adminhttp

import (
	"context"
	"sync"
	"time"

	skillsfs "github.com/matjam/faultline/internal/adapters/skills/fs"
	"github.com/matjam/faultline/internal/agent"
	"github.com/matjam/faultline/internal/subagent"
	"github.com/matjam/faultline/internal/tools"
	"github.com/matjam/faultline/internal/update"
)

// AgentInspector is the read-only port the admin server uses to
// snapshot the primary agent's state. Consumer-defined: the agent
// package exposes the concrete *Agent which satisfies it
// structurally. Driving adapters may depend on the domain (the rule
// is the domain mustn't depend on the adapter), so importing
// internal/agent for the snapshot type is fine.
//
// Nil-allowed: when stage 2 left this unset the dashboard rendered
// empty placeholders. With stage 3 wired the composition root passes
// the live agent.
type AgentInspector interface {
	Snapshot() agent.AgentSnapshot
}

// SubagentInspector exposes the primary's subagent.Manager state
// to the admin server. Profiles is the static catalog from config
// (rendered once on the dashboard); Status is the live list of
// running children, refreshed at every dashboard poll.
type SubagentInspector interface {
	Status() []subagent.ActiveStatus
	Profiles() []subagent.Profile
}

// SkillsAdmin is the read+write port the admin server uses to list
// every skill in the catalog (including operator-disabled ones) and
// toggle their state. Implemented by skills/fs.Store. nil-allowed:
// when not wired, the Skills card on the dashboard renders a
// disabled-feature placeholder.
type SkillsAdmin interface {
	ListAll() []skillsfs.AllSkill
	SetEnabled(name string, enabled bool) error
}

// UpdateInspector is the read+write port for the self-update pane.
// Implemented by *update.Updater. Reads (Enabled, CurrentVersion,
// State) are pure in-memory lookups; State exposes the most recent
// poll result without hitting GitHub. Apply is the destructive
// "update now" action and is only invoked from the dedicated
// endpoint behind a CSRF check.
//
// nil-allowed: when no updater is wired, the Update card on the
// dashboard renders a disabled-feature placeholder.
type UpdateInspector interface {
	Enabled() bool
	CurrentVersion() string
	State() update.State
	Apply(ctx context.Context) (*update.Result, error)
}

// ToolBuffer is the in-memory ring buffer of recent tool-call events.
// Implements tools.Observer so a single instance plugs straight into
// the primary's Executor (and into each subagent's Executor too,
// when the composition root chooses to wire it).
//
// Capped capacity; once full, the oldest event is overwritten. The
// admin server reads the full snapshot under a brief read-lock per
// dashboard poll. At the chosen 500-event default with ~600 bytes
// per event the buffer is well under 1 MiB.
type ToolBuffer struct {
	mu  sync.RWMutex
	buf []tools.ToolCallEvent
	// next is the slot that will receive the NEXT write. When buf
	// is partially full, len(buf) <= next; when full, all slots
	// are valid and writes wrap modulo cap.
	next int
	full bool
	cap  int
}

// NewToolBuffer constructs a ring buffer with the given capacity.
// A non-positive capacity falls back to 500 (the default agreed in
// stage 1 design).
func NewToolBuffer(capacity int) *ToolBuffer {
	if capacity <= 0 {
		capacity = 500
	}
	return &ToolBuffer{
		buf: make([]tools.ToolCallEvent, 0, capacity),
		cap: capacity,
	}
}

// OnToolCall implements tools.Observer. The recorded slice grows up
// to cap; further writes overwrite the oldest entry. Constant-time
// per call — observers must not block the agent loop.
func (b *ToolBuffer) OnToolCall(ev tools.ToolCallEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.full {
		b.buf = append(b.buf, ev)
		b.next = len(b.buf) % b.cap
		if len(b.buf) == b.cap {
			b.full = true
		}
		return
	}
	b.buf[b.next] = ev
	b.next = (b.next + 1) % b.cap
}

// Snapshot returns a copy of the events in chronological order
// (oldest first). The returned slice is owned by the caller; the
// internal buffer is not aliased.
func (b *ToolBuffer) Snapshot() []tools.ToolCallEvent {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.full {
		out := make([]tools.ToolCallEvent, len(b.buf))
		copy(out, b.buf)
		return out
	}
	out := make([]tools.ToolCallEvent, b.cap)
	// b.next is the oldest slot (the next to be overwritten); copy
	// from there to the end, then 0..next.
	copy(out, b.buf[b.next:])
	copy(out[b.cap-b.next:], b.buf[:b.next])
	return out
}

// SnapshotRecent returns the most recent n events, newest last,
// trimming the front of the chronological snapshot. If n <= 0 or
// n >= total, returns the full snapshot.
func (b *ToolBuffer) SnapshotRecent(n int) []tools.ToolCallEvent {
	all := b.Snapshot()
	if n <= 0 || n >= len(all) {
		return all
	}
	out := make([]tools.ToolCallEvent, n)
	copy(out, all[len(all)-n:])
	return out
}

// Len reports the current number of events in the buffer.
func (b *ToolBuffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.full {
		return b.cap
	}
	return len(b.buf)
}

// Cap reports the buffer's capacity.
func (b *ToolBuffer) Cap() int {
	return b.cap
}

// --- presentation helpers ------------------------------------------
//
// These are tiny formatters used by the dashboard templates. Living
// here rather than in the template lets us unit-test them and keeps
// the templates easier to read.

// FormatDuration formats d for the dashboard's "Phase since",
// "Latency", and "Uptime" fields. Sub-second values get millisecond
// precision; second-and-above values get a tighter human label.
func FormatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		// Round to second granularity but pretty-print as
		// h:mm:ss. time.Duration's default String() does
		// 1h2m3s; close enough for the dashboard.
		return d.Round(time.Second).String()
	}
}

// FormatRelative formats t relative to time.Now() for "last chat
// 12s ago" display. Returns "—" for the zero value.
func FormatRelative(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return FormatDuration(time.Since(t)) + " ago"
}
