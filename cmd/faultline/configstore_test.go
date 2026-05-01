package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestFileConfigStore_PathIsAbs(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.Join(dir, "config.toml")

	// New rejects nothing about missing files; constructing the
	// store must succeed even if the file isn't there yet.
	store, err := newFileConfigStore(rel, newSilentLogger(), nil)
	if err != nil {
		t.Fatalf("newFileConfigStore: %v", err)
	}
	if !filepath.IsAbs(store.Path()) {
		t.Fatalf("Path() = %q, want absolute", store.Path())
	}
}

func TestFileConfigStore_ReadReturnsFileBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	want := []byte("[api]\nurl = \"http://localhost:5001/v1\"\nmodel = \"q\"\n")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store, err := newFileConfigStore(path, newSilentLogger(), nil)
	if err != nil {
		t.Fatalf("newFileConfigStore: %v", err)
	}

	got, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Read returned %q, want %q", got, want)
	}
}

func TestFileConfigStore_ValidateAcceptsGood(t *testing.T) {
	dir := t.TempDir()
	store, err := newFileConfigStore(filepath.Join(dir, "config.toml"), newSilentLogger(), nil)
	if err != nil {
		t.Fatalf("newFileConfigStore: %v", err)
	}
	good := []byte("[api]\nurl = \"http://localhost:5001/v1\"\nmodel = \"q\"\n")
	if err := store.Validate(good); err != nil {
		t.Fatalf("Validate(good): %v", err)
	}
}

func TestFileConfigStore_ValidateRejectsBad(t *testing.T) {
	dir := t.TempDir()
	store, err := newFileConfigStore(filepath.Join(dir, "config.toml"), newSilentLogger(), nil)
	if err != nil {
		t.Fatalf("newFileConfigStore: %v", err)
	}
	bad := []byte("this is not = valid = toml ====\n")
	if err := store.Validate(bad); err == nil {
		t.Fatal("Validate(bad) returned nil; expected parse error")
	}
}

func TestFileConfigStore_WriteValidatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("# old\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store, err := newFileConfigStore(path, newSilentLogger(), nil)
	if err != nil {
		t.Fatalf("newFileConfigStore: %v", err)
	}

	good := []byte("[api]\nurl = \"http://x:1/v1\"\nmodel = \"q\"\n")
	if err := store.Write(good); err != nil {
		t.Fatalf("Write(good): %v", err)
	}
	// File should now contain the new bytes.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(good) {
		t.Fatalf("on-disk = %q, want %q", got, good)
	}

	// Bad config must be rejected and must NOT clobber the file.
	bad := []byte("not = valid = ====\n")
	if err := store.Write(bad); err == nil {
		t.Fatal("Write(bad) returned nil; expected parse error")
	}
	got2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after rejected write: %v", err)
	}
	if string(got2) != string(good) {
		t.Fatalf("file was clobbered after rejected write\non-disk: %q\nwant: %q", got2, good)
	}
}

func TestFileConfigStore_WritePreservesPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// Seed with 0600 perms; rewriting should keep them.
	if err := os.WriteFile(path, []byte("# old\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store, err := newFileConfigStore(path, newSilentLogger(), nil)
	if err != nil {
		t.Fatalf("newFileConfigStore: %v", err)
	}
	good := []byte("[api]\nurl = \"http://x:1/v1\"\nmodel = \"q\"\n")
	if err := store.Write(good); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("perms = %o, want 0600", got)
	}
}

func TestFileConfigStore_RestartFiresShutdown(t *testing.T) {
	dir := t.TempDir()
	called := 0
	store, err := newFileConfigStore(filepath.Join(dir, "config.toml"),
		newSilentLogger(),
		func() { called++ })
	if err != nil {
		t.Fatalf("newFileConfigStore: %v", err)
	}
	store.Restart()
	store.Restart()
	if called != 2 {
		t.Fatalf("shutdown called %d times, want 2", called)
	}
}

func TestFileConfigStore_RestartNilSafe(t *testing.T) {
	dir := t.TempDir()
	store, err := newFileConfigStore(filepath.Join(dir, "config.toml"), newSilentLogger(), nil)
	if err != nil {
		t.Fatalf("newFileConfigStore: %v", err)
	}
	// Must not panic.
	store.Restart()
}
