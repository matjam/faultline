package docker

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitCreatesNodePackageManifest(t *testing.T) {
	s := &Sandbox{
		dir:    t.TempDir(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := s.init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(s.dir, "node", "package.json"))
	if err != nil {
		t.Fatalf("read node/package.json: %v", err)
	}
	for _, want := range []string{`"name": "faultline-node-sandbox"`, `"private": true`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("package.json missing %q:\n%s", want, data)
		}
	}
}

func TestDockerArgsMountNodeEnvironment(t *testing.T) {
	s := &Sandbox{
		dir:         "/tmp/faultline/sandbox",
		image:       "faultline-sandbox",
		memoryLimit: "128m",
		uid:         1000,
		gid:         1000,
	}

	args := s.dockerArgs(false, "faultline-sandbox-test")
	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"-v\x00/tmp/faultline/sandbox/node:/node:rw",
		"-v\x00/tmp/faultline/sandbox/mcp:/mcp:rw",
		"-e\x00PATH=/node/node_modules/.bin:/usr/local/bin:/usr/bin:/bin",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker args missing %q in %v", want, args)
		}
	}
}

func TestMCPStdioArgsExposeNodeEnvironment(t *testing.T) {
	s := &Sandbox{
		dir:         "/tmp/faultline/sandbox",
		image:       "faultline-sandbox",
		memoryLimit: "128m",
		uid:         1000,
		gid:         1000,
	}

	args := s.mcpStdioArgs("faultline-mcp-test", "/mcp/playwright", nil, "playwright-mcp", nil)
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "-e\x00PATH=/node/node_modules/.bin:/usr/local/bin:/usr/bin:/bin") {
		t.Fatalf("mcp stdio args missing node PATH in %v", args)
	}
}
