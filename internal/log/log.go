// Package log provides cross-cutting logging infrastructure: a daily-rotating
// file writer and a multi-handler that fans slog records to multiple sinks.
package log

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Daily is an io.Writer that writes to date-stamped log files, automatically
// rotating to a new file when the date changes.
type Daily struct {
	dir     string
	prefix  string // optional filename prefix, e.g. "sandbox-"
	current *os.File
	date    string
	mu      sync.Mutex
}

// NewDaily creates a writer that outputs to dir/YYYY-MM-DD.log.
func NewDaily(dir string) (*Daily, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	w := &Daily{dir: dir}
	// Open today's file immediately so errors surface at startup
	if _, err := w.ensureFile(); err != nil {
		return nil, err
	}
	return w, nil
}

// NewDailyPrefixed creates a writer that outputs to dir/prefix-YYYY-MM-DD.log.
func NewDailyPrefixed(dir, prefix string) (*Daily, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	w := &Daily{dir: dir, prefix: prefix}
	if _, err := w.ensureFile(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Daily) ensureFile() (*os.File, error) {
	today := time.Now().Format("2006-01-02")
	if today != w.date || w.current == nil {
		if w.current != nil {
			w.current.Close()
		}
		filename := today + ".log"
		if w.prefix != "" {
			filename = w.prefix + today + ".log"
		}
		path := filepath.Join(w.dir, filename)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}
		w.current = f
		w.date = today
	}
	return w.current, nil
}

func (w *Daily) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := w.ensureFile()
	if err != nil {
		return 0, err
	}
	return f.Write(p)
}

// Close closes the current log file.
func (w *Daily) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current != nil {
		return w.current.Close()
	}
	return nil
}

// MultiHandler fans out log records to multiple slog handlers.
// A record is passed to a handler only if that handler is enabled for the record's level.
type MultiHandler struct {
	handlers []slog.Handler
}

func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	return &MultiHandler{handlers: handlers}
}

func (m *MultiHandler) Enabled(_ context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(context.Background(), level) {
			return true
		}
	}
	return false
}

func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: handlers}
}

func (m *MultiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: handlers}
}
