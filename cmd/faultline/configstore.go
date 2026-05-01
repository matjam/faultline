package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/matjam/faultline/internal/config"
)

// fileConfigStore satisfies adminhttp.ConfigStore by reading and
// writing the TOML config file the agent was started with. Validation
// runs the bytes through config.Load against a temp path so the same
// defaults backfill / sanity passes apply as on real startup.
//
// Restart funnels through the same closeShutdown closure the signal
// handler and updater use, so a config-driven restart looks identical
// to a SIGINT to the rest of the system: agent saves state, returns,
// main.go's restart_mode dispatches.
type fileConfigStore struct {
	path     string
	logger   *slog.Logger
	shutdown func()

	// mu serializes Write to avoid racing operators clobbering each
	// other's edits. Read is safe to call without it (os.ReadFile is
	// atomic enough for our purposes).
	mu sync.Mutex
}

func newFileConfigStore(path string, logger *slog.Logger, shutdown func()) (*fileConfigStore, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	return &fileConfigStore{
		path:     abs,
		logger:   logger,
		shutdown: shutdown,
	}, nil
}

func (f *fileConfigStore) Path() string { return f.path }

func (f *fileConfigStore) Read() ([]byte, error) {
	return os.ReadFile(f.path)
}

// Validate writes the bytes to a temp file and runs config.Load on
// it. The temp file is removed regardless of outcome. Returns nil iff
// the bytes parse cleanly and produce a usable Config.
//
// Note: config.Load is forgiving on missing fields (defaults are
// backfilled), so an empty file or a one-line override both validate.
// The point of this pass is to catch syntax errors and bad enum-ish
// values before they hit disk.
func (f *fileConfigStore) Validate(raw []byte) error {
	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".faultline-config-validate-*.toml")
	if err != nil {
		// Fall back to system temp dir if the config dir isn't
		// writable; we still want validation to work.
		tmp, err = os.CreateTemp("", ".faultline-config-validate-*.toml")
		if err != nil {
			return fmt.Errorf("create temp file: %w", err)
		}
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if _, err := config.Load(tmpPath); err != nil {
		return err
	}
	return nil
}

func (f *fileConfigStore) Write(raw []byte) error {
	if err := f.Validate(raw); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".faultline-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if we return before rename. The rename
	// branch sets renamed = true so this defer becomes a no-op
	// (os.Remove on a path that's been renamed away is a quiet
	// not-exist).
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Preserve the original permissions when possible so the
	// rewrite doesn't downgrade a 0600 secret-bearing file to
	// 0644.
	if info, err := os.Stat(f.path); err == nil {
		if perr := os.Chmod(tmpPath, info.Mode().Perm()); perr != nil {
			f.logger.Warn("config save: chmod on temp failed; continuing",
				"path", tmpPath, "error", perr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		// Non-not-found stat error is unusual; don't block the
		// write but log it.
		f.logger.Warn("config save: stat existing file failed; continuing",
			"path", f.path, "error", err)
	}

	if err := os.Rename(tmpPath, f.path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	renamed = true
	return nil
}

func (f *fileConfigStore) Restart() {
	if f.shutdown != nil {
		f.shutdown()
	}
}
