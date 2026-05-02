package docker

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPStdioArgsUseRegularSandboxMounts(t *testing.T) {
	s := &Sandbox{
		dir:         "/tmp/faultline/sandbox",
		image:       "faultline-sandbox",
		memoryLimit: "128m",
		uid:         1000,
		gid:         1000,
	}

	args := s.mcpStdioArgs("faultline-mcp-test", "/mcp/nerdoracle", map[string]string{
		"NERDORACLE_API_URL": "https://example.invalid",
	}, "npx", []string{"ts-node", "src/index.ts"})
	joined := strings.Join(args, "\x00")

	for _, want := range []string{
		"run", "--rm", "-i",
		"--name\x00faultline-mcp-test",
		"-w\x00/mcp/nerdoracle",
		"--memory\x00128m",
		"--user\x001000:1000",
		"--security-opt\x00no-new-privileges",
		"-v\x00/tmp/faultline/sandbox/scripts:/scripts:ro",
		"-v\x00/tmp/faultline/sandbox/input:/input:ro",
		"-v\x00/tmp/faultline/sandbox/output:/output:rw",
		"-v\x00/tmp/faultline/sandbox/venv:/venv:rw",
		"-v\x00/tmp/faultline/sandbox/node:/node:rw",
		"-v\x00/tmp/faultline/sandbox/mcp:/mcp:rw",
		"-v\x00/tmp/faultline/sandbox/cache:/cache:rw",
		"--network=none",
		"-e\x00NERDORACLE_API_URL=https://example.invalid",
		"-e\x00npm_config_cache=/cache/npm",
		"-e\x00HOME=/cache/home",
		"-e\x00XDG_CACHE_HOME=/cache",
		"-e\x00UV_PROJECT_ENVIRONMENT=/venv",
		"-e\x00PATH=/node/node_modules/.bin:/usr/local/bin:/usr/bin:/bin",
		"faultline-sandbox\x00npx\x00ts-node\x00src/index.ts",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker args missing %q in %v", want, args)
		}
	}
	if strings.Contains(joined, ":/sandbox:") {
		t.Fatalf("docker args should not include separate /sandbox root mount: %v", args)
	}
}

func TestMCPStdioArgsUsesSandboxNetworkSetting(t *testing.T) {
	s := &Sandbox{
		dir:         "/tmp/faultline/sandbox",
		image:       "faultline-sandbox",
		memoryLimit: "128m",
		uid:         1000,
		gid:         1000,
		network:     true,
	}

	args := s.mcpStdioArgs("faultline-mcp-test", "/mcp/atlassian", nil, "npx", []string{"-y", "@atlassian/mcp"})
	if strings.Contains(strings.Join(args, "\x00"), "--network=none") {
		t.Fatalf("docker args should inherit enabled sandbox networking: %v", args)
	}
}

func TestMCPStdioHostWorkDirMapsOnlyMCPPaths(t *testing.T) {
	s := &Sandbox{dir: "/tmp/faultline/sandbox"}

	got, err := s.mcpStdioHostWorkDir("/mcp/playwright")
	if err != nil {
		t.Fatalf("mcpStdioHostWorkDir: %v", err)
	}
	if got != filepath.Join("/tmp/faultline/sandbox", "mcp", "playwright") {
		t.Fatalf("host workdir = %q", got)
	}

	for _, invalid := range []string{"/Users/camilo/playwright", "/sandbox/mcp/playwright", "/output"} {
		if _, err := s.mcpStdioHostWorkDir(invalid); err == nil {
			t.Fatalf("expected workdir %q to be rejected", invalid)
		}
	}
}

func TestMCPStdioDefaultWorkDirUsesMCPRoot(t *testing.T) {
	s := &Sandbox{dir: "/tmp/faultline/sandbox"}

	got, err := s.mcpStdioHostWorkDir("/mcp/playwright")
	if err != nil {
		t.Fatalf("mcpStdioHostWorkDir: %v", err)
	}
	if got != filepath.Join("/tmp/faultline/sandbox", "mcp", "playwright") {
		t.Fatalf("host workdir = %q", got)
	}
	if _, err := s.mcpStdioHostWorkDir("/mcp/../output"); err == nil {
		t.Fatal("expected host path workdir to be rejected")
	}
}

func TestDockerArgsSetWritableNodeCache(t *testing.T) {
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
		"-e\x00HOME=/cache/home",
		"-e\x00npm_config_cache=/cache/npm",
		"-e\x00XDG_CACHE_HOME=/cache",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker args missing %q in %v", want, args)
		}
	}
}
