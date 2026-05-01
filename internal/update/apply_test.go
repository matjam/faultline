package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractBinary(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "fixture.tar.gz")
	wantContent := []byte("#!/bin/sh\necho old\n")

	writeTarball(t, tarPath, map[string][]byte{
		"LICENSE":             []byte("license text"),
		"README.md":           []byte("readme"),
		"faultline":           wantContent,
		"config.example.toml": []byte("# example"),
	})

	outPath := filepath.Join(dir, "faultline.new")
	if err := extractBinary(tarPath, outPath); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantContent) {
		t.Errorf("extracted content mismatch\n got: %q\nwant: %q", got, wantContent)
	}
}

func TestExtractBinary_MissingBinary(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "no-binary.tar.gz")
	writeTarball(t, tarPath, map[string][]byte{
		"LICENSE":   []byte("license"),
		"README.md": []byte("readme"),
	})

	err := extractBinary(tarPath, filepath.Join(dir, "out"))
	if err == nil || !strings.Contains(err.Error(), "did not contain") {
		t.Errorf("expected 'did not contain' error, got %v", err)
	}
}

func TestFetchExpectedSum(t *testing.T) {
	body := `abc123def456  faultline_1.0.0_linux_x86_64.tar.gz
fedcba654321  faultline_1.0.0_linux_arm64.tar.gz
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	gh := newGitHubClient("o/r")
	gh.apiBase = srv.URL

	got, err := fetchExpectedSum(t.Context(), gh, srv.URL, "faultline_1.0.0_linux_arm64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if got != "fedcba654321" {
		t.Errorf("got %q, want fedcba654321", got)
	}
}

func TestFetchExpectedSum_NotListed(t *testing.T) {
	body := `abc123  some-other-file.tar.gz`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	gh := newGitHubClient("o/r")
	gh.apiBase = srv.URL

	_, err := fetchExpectedSum(t.Context(), gh, srv.URL, "missing.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "not listed") {
		t.Errorf("expected 'not listed' error, got %v", err)
	}
}

func TestFileSha256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	content := []byte("hello world")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := fileSha256(path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256Hex(content)
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// writeTarball creates a .tar.gz at path containing the given files.
func writeTarball(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gzw := gzip.NewWriter(f)
	tw := tar.NewWriter(gzw)
	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
