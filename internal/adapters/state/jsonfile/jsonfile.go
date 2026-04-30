// Package jsonfile is the conversation-state persistence adapter that
// writes the message log to a single JSON file with atomic rename-based
// updates.
package jsonfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/matjam/faultline/internal/llm"
)

// stateFileVersion is bumped when the on-disk format changes in a way that
// makes older files unreadable. On mismatch the loader renames the bad
// file aside and starts fresh rather than refusing to come up.
const stateFileVersion = 1

// persistedState is the on-disk representation of the agent's conversation
// log. Only the message slice is preserved verbatim; the system message is
// always rebuilt from current prompts and memories on load, so the saved
// system message is replaced rather than reused. Tool/web caches and
// sandbox state are intentionally not persisted -- they are either
// reconstructible (web cache) or already on disk (sandbox dir).
type persistedState struct {
	Version    int           `json:"version"`
	SavedAt    time.Time     `json:"saved_at"`
	IdleStreak int           `json:"idle_streak"`
	Messages   []llm.Message `json:"messages"`
}

// Save writes the current message log to path atomically: write to a
// temp file in the same directory, fsync, rename. The rename is atomic on
// POSIX, so a crash mid-save leaves either the previous good file or the
// new good file -- never a half-written one.
//
// Path may be empty, in which case this is a no-op (persistence disabled).
func Save(path string, messages []llm.Message, idleStreak int) error {
	if path == "" {
		return nil
	}

	state := persistedState{
		Version:    stateFileVersion,
		SavedAt:    time.Now(),
		IdleStreak: idleStreak,
		Messages:   messages,
	}

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpPath := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close state: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename state: %w", err)
	}

	return nil
}

// Load reads a saved state file. Returns (nil, 0, nil) when the file
// does not exist (a fresh start is normal). On parse error or version
// mismatch, the bad file is renamed aside as state.json.bad-<unix> so it
// can be inspected, and (nil, 0, nil) is returned -- the daemon comes up
// with a fresh context rather than refusing to start.
//
// Returns the messages, the saved idle-streak counter, and any unexpected
// I/O error. Path may be empty (persistence disabled), in which case this
// returns (nil, 0, nil).
func Load(path string, logger *slog.Logger) ([]llm.Message, int, error) {
	if path == "" {
		return nil, 0, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("read state: %w", err)
	}

	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		quarantineBadStateFile(path, fmt.Sprintf("parse error: %v", err), logger)
		return nil, 0, nil
	}

	if state.Version != stateFileVersion {
		quarantineBadStateFile(path,
			fmt.Sprintf("version mismatch: file=%d expected=%d", state.Version, stateFileVersion),
			logger)
		return nil, 0, nil
	}

	messages := sanitizeMessages(state.Messages)
	return messages, state.IdleStreak, nil
}

// quarantineBadStateFile renames a state file we can't load to a sibling
// name with a unix timestamp, so the operator can inspect it. If the
// rename itself fails we just log -- coming up with a fresh context is
// more important than preserving the bad file.
func quarantineBadStateFile(path, reason string, logger *slog.Logger) {
	bad := fmt.Sprintf("%s.bad-%d", path, time.Now().Unix())
	if err := os.Rename(path, bad); err != nil {
		logger.Error("could not quarantine bad state file",
			"path", path, "reason", reason, "rename_error", err)
		return
	}
	logger.Warn("state file unreadable, quarantined and starting fresh",
		"path", path, "moved_to", bad, "reason", reason)
}

// sanitizeMessages walks a saved message log and trims any trailing turns
// that would make the log unsendable to an OpenAI-compatible API. Two
// invariants matter:
//
//  1. Every assistant message with tool_calls must be followed by a tool
//     message for each tool_call_id. A crash between dispatch and the
//     final tool result append could leave dangling IDs.
//
//  2. The log must not end with a tool message that has no preceding
//     assistant tool_call (impossible in normal operation, but cheap to
//     guard against).
//
// This is intentionally conservative: we walk from the tail and strip
// only the trailing partial turn. Anything earlier is assumed valid
// because top-of-loop saving captures only complete turn boundaries.
func sanitizeMessages(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return messages
	}

	// Walk backwards looking for the last assistant turn with tool_calls
	// and verify each of its IDs has a matching tool message after it.
	// If any are missing, drop everything from that assistant turn
	// onwards.
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != llm.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}

		needed := make(map[string]bool, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			needed[tc.ID] = true
		}
		for j := i + 1; j < len(messages); j++ {
			if messages[j].Role == llm.RoleTool {
				delete(needed, messages[j].ToolCallID)
			}
		}
		if len(needed) > 0 {
			// Trailing assistant turn has unsatisfied tool_call_ids.
			// Drop it and everything after.
			return messages[:i]
		}
		// Most recent tool-using assistant turn is complete; we're done.
		break
	}

	return messages
}
