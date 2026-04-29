package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDailyFileWriter_WritesToTodaysFile(t *testing.T) {
	dir := t.TempDir()
	w, err := NewDailyFileWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}

	expected := filepath.Join(dir, time.Now().Format("2006-01-02")+".log")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("expected file %s: %v", expected, err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Errorf("file content missing write: %q", string(data))
	}
}

func TestDailyFileWriter_PrefixedFilename(t *testing.T) {
	dir := t.TempDir()
	w, err := NewPrefixedDailyFileWriter(dir, "sandbox-")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}

	expected := filepath.Join(dir, "sandbox-"+time.Now().Format("2006-01-02")+".log")
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("expected prefixed file %s, got error: %v", expected, err)
	}
}

func TestDailyFileWriter_AppendsToExistingFile(t *testing.T) {
	dir := t.TempDir()

	w1, err := NewDailyFileWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w1.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	w1.Close()

	w2, err := NewDailyFileWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	w2.Close()

	expected := filepath.Join(dir, time.Now().Format("2006-01-02")+".log")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "first") || !strings.Contains(string(data), "second") {
		t.Errorf("file should contain both writes, got: %q", string(data))
	}
}

func TestDailyFileWriter_CreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	w, err := NewDailyFileWriter(dir)
	if err != nil {
		t.Fatalf("NewDailyFileWriter: %v", err)
	}
	defer w.Close()

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir not created: %v", err)
	}
}

func TestMultiHandler_FansOutToAll(t *testing.T) {
	var bufA, bufB bytes.Buffer
	hA := slog.NewTextHandler(&bufA, &slog.HandlerOptions{Level: slog.LevelDebug})
	hB := slog.NewTextHandler(&bufB, &slog.HandlerOptions{Level: slog.LevelDebug})

	mh := NewMultiHandler(hA, hB)
	logger := slog.New(mh)
	logger.Info("hello multi", "k", "v")

	if !strings.Contains(bufA.String(), "hello multi") {
		t.Errorf("handler A missing message: %q", bufA.String())
	}
	if !strings.Contains(bufB.String(), "hello multi") {
		t.Errorf("handler B missing message: %q", bufB.String())
	}
}

func TestMultiHandler_RespectsLevelPerHandler(t *testing.T) {
	// Two handlers at different levels: only one should receive a debug record.
	var bufInfo, bufDebug bytes.Buffer
	hInfo := slog.NewTextHandler(&bufInfo, &slog.HandlerOptions{Level: slog.LevelInfo})
	hDebug := slog.NewTextHandler(&bufDebug, &slog.HandlerOptions{Level: slog.LevelDebug})

	mh := NewMultiHandler(hInfo, hDebug)
	logger := slog.New(mh)

	logger.Debug("debug-only message")

	if strings.Contains(bufInfo.String(), "debug-only message") {
		t.Errorf("info handler should not have received debug record: %q", bufInfo.String())
	}
	if !strings.Contains(bufDebug.String(), "debug-only message") {
		t.Errorf("debug handler missing record: %q", bufDebug.String())
	}
}

func TestMultiHandler_EnabledShortCircuit(t *testing.T) {
	var buf bytes.Buffer
	hInfo := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})

	mh := NewMultiHandler(hInfo)

	if mh.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Enabled(Debug) should be false when no handler accepts Debug")
	}
	if !mh.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled(Info) should be true when an info handler is present")
	}
}

func TestMultiHandler_WithAttrsAndGroup(t *testing.T) {
	// Smoke test: WithAttrs and WithGroup return a handler that still works.
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	mh := NewMultiHandler(h)

	logger := slog.New(mh).With("traceID", "abc").WithGroup("grp")
	logger.Info("done")

	out := buf.String()
	if !strings.Contains(out, "traceID=abc") {
		t.Errorf("WithAttrs not propagated: %q", out)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("message missing: %q", out)
	}
}
