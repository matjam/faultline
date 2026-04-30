package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/config"
	"github.com/matjam/faultline/internal/log"
)

// Sandbox manages a Python script execution environment backed by Docker and uv.
// Scripts, inputs, and outputs are flat directories on the host filesystem.
// Docker containers are ephemeral -- spun up per operation and removed immediately.
type Sandbox struct {
	dir         string // absolute path to sandbox/ directory
	image       string
	timeout     time.Duration
	network     bool   // allow network access during script execution
	memoryLimit string // docker --memory value
	logger      *slog.Logger
	execLog     *log.Daily // execution log in the logs directory
	uid         int        // host uid for --user flag
	gid         int        // host gid for --user flag

	// outputLimit caps stdout/stderr returned to the agent for both
	// script execution and shell exec. Zero or negative disables the
	// cap. Set via SetOutputLimit after construction so callers can
	// honor LimitsConfig without changing the constructor signature.
	outputLimit int
}

// defaultSandboxOutputLimit is used when SetOutputLimit hasn't been called
// or was given a non-positive value but the caller still wants a safety net.
// Kept generous so the agent rarely sees clipped output.
const defaultSandboxOutputLimit = 64000

// SetOutputLimit configures the maximum number of characters of combined
// stdout/stderr returned to the agent. A non-positive value disables the
// cap (full output is returned).
func (s *Sandbox) SetOutputLimit(n int) {
	s.outputLimit = n
}

// sandboxFolders are the valid folder names within the sandbox.
var sandboxFolders = map[string]bool{
	"scripts": true,
	"input":   true,
	"output":  true,
}

// filenamePattern validates flat, lowercase filenames.
// Allows lowercase letters, digits, hyphens, underscores, and dots.
var filenamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// defaultPyproject is the initial pyproject.toml content.
const defaultPyproject = `[project]
name = "sandbox"
version = "0.1.0"
description = "Faultline agent sandbox environment"
requires-python = ">=3.12"
dependencies = []
`

// NewSandbox creates and initializes a sandbox environment.
// workDir is the agent's working directory (e.g. /data/faultline).
// logDir is the directory for sandbox execution logs (e.g. ./logs).
func NewSandbox(cfg config.SandboxConfig, workDir, logDir string, logger *slog.Logger) (*Sandbox, error) {
	dir := cfg.Dir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(workDir, dir)
	}

	if !filepath.IsAbs(logDir) {
		logDir = filepath.Join(workDir, logDir)
	}

	execLog, err := log.NewDailyPrefixed(logDir, "sandbox-")
	if err != nil {
		return nil, fmt.Errorf("create sandbox log: %w", err)
	}

	// Verify Docker is available before creating any directories
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker not found in PATH: %w", err)
	}

	s := &Sandbox{
		dir:         dir,
		image:       cfg.Image,
		timeout:     cfg.Timeout.Duration(),
		network:     cfg.Network,
		memoryLimit: cfg.MemoryLimit,
		logger:      logger,
		execLog:     execLog,
		uid:         os.Getuid(),
		gid:         os.Getgid(),
	}

	if err := s.init(); err != nil {
		return nil, fmt.Errorf("sandbox init: %w", err)
	}

	return s, nil
}

// init creates the sandbox directory structure and seed files.
// Close releases resources held by the sandbox (e.g. the execution log file).
func (s *Sandbox) Close() error {
	if s.execLog != nil {
		return s.execLog.Close()
	}
	return nil
}

// logExec writes a structured entry to the sandbox execution log.
func (s *Sandbox) logExec(operation string, detail string, duration time.Duration, output string, err error) {
	if s.execLog == nil {
		return
	}

	status := "OK"
	if err != nil {
		status = fmt.Sprintf("ERROR: %s", err)
	}

	entry := fmt.Sprintf(
		"=== %s | %s | %s | %s ===\n%s\n%s\n\n",
		time.Now().Format("2006-01-02 15:04:05"),
		operation,
		detail,
		duration.Round(time.Millisecond),
		status,
		output,
	)

	if _, err := s.execLog.Write([]byte(entry)); err != nil {
		s.logger.Warn("failed to write sandbox exec log", "error", err)
	}
}

func (s *Sandbox) init() error {
	// Create directories
	for _, sub := range []string{"scripts", "input", "output", "venv", "cache"} {
		if err := os.MkdirAll(filepath.Join(s.dir, sub), 0755); err != nil {
			return fmt.Errorf("create %s: %w", sub, err)
		}
	}

	// Create pyproject.toml if missing
	pyprojectPath := filepath.Join(s.dir, "pyproject.toml")
	if _, err := os.Stat(pyprojectPath); os.IsNotExist(err) {
		if err := os.WriteFile(pyprojectPath, []byte(defaultPyproject), 0644); err != nil {
			return fmt.Errorf("create pyproject.toml: %w", err)
		}
		s.logger.Info("created initial pyproject.toml", "path", pyprojectPath)
	}

	// Create a valid initial uv.lock if missing.
	// An empty file causes uv to fail with a parse error, so we write
	// the minimal valid lock content matching our pyproject.toml.
	// uv will overwrite this on the first sync/lock operation.
	lockPath := filepath.Join(s.dir, "uv.lock")
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		initialLock := `version = 1
revision = 1
requires-python = ">=3.12"

[[package]]
name = "sandbox"
version = "0.1.0"
source = { virtual = "." }
`
		if err := os.WriteFile(lockPath, []byte(initialLock), 0644); err != nil {
			return fmt.Errorf("create uv.lock: %w", err)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// File operations (host filesystem, no Docker)
// ---------------------------------------------------------------------------

// validateFolder checks that the folder name is valid.
func (s *Sandbox) validateFolder(folder string) error {
	if !sandboxFolders[folder] {
		return fmt.Errorf("invalid folder %q: must be one of scripts, input, output", folder)
	}
	return nil
}

// validateFilename checks that a filename is flat, lowercase, and safe.
func (s *Sandbox) validateFilename(name string) error {
	if name == "" {
		return fmt.Errorf("filename is required")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("filename must not contain path separators (flat files only)")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("filename must not contain '..'")
	}
	if !filenamePattern.MatchString(name) {
		return fmt.Errorf("filename %q is invalid: must be lowercase, starting with alphanumeric, containing only [a-z0-9._-]", name)
	}
	return nil
}

// resolvePath returns the absolute host path for a file in a given folder,
// after validating both the folder and filename.
func (s *Sandbox) resolvePath(folder, name string) (string, error) {
	if err := s.validateFolder(folder); err != nil {
		return "", err
	}
	if err := s.validateFilename(name); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, folder, name), nil
}

// FileInfo holds metadata about a sandbox file.
type SandboxFileInfo struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// WriteFile creates or overwrites a file in the given folder.
func (s *Sandbox) WriteFile(folder, name, content string) error {
	path, err := s.resolvePath(folder, name)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	s.logger.Info("sandbox file written", "folder", folder, "name", name, "size", len(content))
	return nil
}

// ReadFile reads a file from the given folder, with optional offset and line limit.
// offset is 1-indexed (0 or 1 both mean start from the beginning).
// lines <= 0 means read all lines.
func (s *Sandbox) ReadFile(folder, name string, offset, lines int) (string, error) {
	path, err := s.resolvePath(folder, name)
	if err != nil {
		return "", err
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file %q not found in %s/", name, folder)
		}
		return "", fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	if offset < 1 {
		offset = 1
	}

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	// Increase buffer for long lines
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNum := 0
	collected := 0
	for scanner.Scan() {
		lineNum++
		if lineNum < offset {
			continue
		}
		if lines > 0 && collected >= lines {
			break
		}
		fmt.Fprintf(&sb, "%d: %s\n", lineNum, scanner.Text())
		collected++
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}

	if collected == 0 {
		if lineNum == 0 {
			return "(empty file)", nil
		}
		return fmt.Sprintf("(no lines in range: file has %d lines, requested offset %d)", lineNum, offset), nil
	}

	return sb.String(), nil
}

// DeleteFile removes a file from the given folder.
func (s *Sandbox) DeleteFile(folder, name string) error {
	path, err := s.resolvePath(folder, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file %q not found in %s/", name, folder)
		}
		return fmt.Errorf("delete file: %w", err)
	}
	s.logger.Info("sandbox file deleted", "folder", folder, "name", name)
	return nil
}

// RenameFile renames a file within the same folder.
func (s *Sandbox) RenameFile(folder, oldName, newName string) error {
	oldPath, err := s.resolvePath(folder, oldName)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	newPath, err := s.resolvePath(folder, newName)
	if err != nil {
		return fmt.Errorf("destination: %w", err)
	}

	// Check source exists
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		return fmt.Errorf("file %q not found in %s/", oldName, folder)
	}
	// Check destination doesn't exist
	if _, err := os.Stat(newPath); err == nil {
		return fmt.Errorf("file %q already exists in %s/", newName, folder)
	}

	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	s.logger.Info("sandbox file renamed", "folder", folder, "from", oldName, "to", newName)
	return nil
}

// EditFile performs an exact string find-and-replace within a sandbox file.
// If replaceAll is false, oldString must appear exactly once.
func (s *Sandbox) EditFile(folder, name, oldString, newString string, replaceAll bool) (int, error) {
	path, err := s.resolvePath(folder, name)
	if err != nil {
		return 0, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("file %q not found in %s/", name, folder)
		}
		return 0, fmt.Errorf("read file: %w", err)
	}

	content := string(data)
	count := strings.Count(content, oldString)

	if count == 0 {
		return 0, fmt.Errorf("old_string not found in %s/%s", folder, name)
	}
	if count > 1 && !replaceAll {
		return 0, fmt.Errorf("found %d matches in %s/%s; use replace_all to replace all, or provide more context to make the match unique", count, folder, name)
	}

	var result string
	if replaceAll {
		result = strings.ReplaceAll(content, oldString, newString)
	} else {
		result = strings.Replace(content, oldString, newString, 1)
	}

	if err := os.WriteFile(path, []byte(result), 0644); err != nil {
		return 0, fmt.Errorf("write file: %w", err)
	}

	s.logger.Info("sandbox file edited", "folder", folder, "name", name, "replacements", count)
	return count, nil
}

// AppendFile appends content to the end of a sandbox file.
// Creates the file if it does not exist.
func (s *Sandbox) AppendFile(folder, name, content string) error {
	path, err := s.resolvePath(folder, name)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open for append: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("append: %w", err)
	}

	s.logger.Info("sandbox file appended", "folder", folder, "name", name, "size", len(content))
	return nil
}

// InsertFile inserts content at a specific line number in a sandbox file.
// line is 1-indexed. Content is inserted before the specified line.
// If line exceeds file length, content is appended at the end.
func (s *Sandbox) InsertFile(folder, name string, line int, content string) (int, error) {
	path, err := s.resolvePath(folder, name)
	if err != nil {
		return 0, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("file %q not found in %s/", name, folder)
		}
		return 0, fmt.Errorf("read file: %w", err)
	}

	if line < 1 {
		line = 1
	}

	lines := strings.Split(string(data), "\n")

	// strings.Split produces a trailing empty element for files ending in \n.
	trailingNewline := len(data) > 0 && data[len(data)-1] == '\n'
	if trailingNewline && len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	totalLines := len(lines)

	insertIdx := line - 1
	if insertIdx > totalLines {
		insertIdx = totalLines
	}

	insertLines := strings.Split(content, "\n")

	newLines := make([]string, 0, totalLines+len(insertLines))
	newLines = append(newLines, lines[:insertIdx]...)
	newLines = append(newLines, insertLines...)
	newLines = append(newLines, lines[insertIdx:]...)

	result := strings.Join(newLines, "\n")
	if trailingNewline && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}

	if err := os.WriteFile(path, []byte(result), 0644); err != nil {
		return 0, fmt.Errorf("write file: %w", err)
	}

	s.logger.Info("sandbox file insert", "folder", folder, "name", name, "at_line", line)
	return len(newLines), nil
}

// ListFiles returns metadata for all files in the given folder.
func (s *Sandbox) ListFiles(folder string) ([]SandboxFileInfo, error) {
	if err := s.validateFolder(folder); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(filepath.Join(s.dir, folder))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", folder, err)
	}

	var files []SandboxFileInfo
	for _, e := range entries {
		if e.IsDir() {
			continue // flat structure, skip subdirectories
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, SandboxFileInfo{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	return files, nil
}

// ---------------------------------------------------------------------------
// Docker operations
// ---------------------------------------------------------------------------

// randomID returns a short random hex string for container naming.
func randomID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// dockerArgs builds the common docker run arguments (mounts, limits, user, etc).
// needsNetwork overrides the config to allow network access (for package operations).
// containerName is used for cleanup on timeout.
func (s *Sandbox) dockerArgs(needsNetwork bool, containerName string) []string {
	args := []string{
		"run", "--rm",
		"--name", containerName,
		// Mount directories
		"-v", filepath.Join(s.dir, "scripts") + ":/scripts:ro",
		"-v", filepath.Join(s.dir, "input") + ":/input:ro",
		"-v", filepath.Join(s.dir, "output") + ":/output:rw",
		"-v", filepath.Join(s.dir, "venv") + ":/venv:rw",
		// Mount project files
		"-v", filepath.Join(s.dir, "pyproject.toml") + ":/pyproject.toml:rw",
		"-v", filepath.Join(s.dir, "uv.lock") + ":/uv.lock:rw",
		// Cache directory (persistent, avoids permission issues with --user)
		"-v", filepath.Join(s.dir, "cache") + ":/cache:rw",
		// Environment
		"-e", "UV_CACHE_DIR=/cache",
		"-e", "UV_LINK_MODE=copy",
		"-e", "UV_PROJECT_ENVIRONMENT=/venv",
		// Working directory
		"-w", "/",
		// Resource limits
		"--memory", s.memoryLimit,
		// Run as host user to avoid root-owned files
		"--user", fmt.Sprintf("%d:%d", s.uid, s.gid),
	}

	if !needsNetwork && !s.network {
		args = append(args, "--network=none")
	}

	args = append(args, s.image)
	return args
}

// dockerRun executes a docker command and returns combined stdout/stderr.
// On timeout, the container is explicitly killed to prevent orphaned containers.
func (s *Sandbox) dockerRun(ctx context.Context, needsNetwork bool, command ...string) (string, error) {
	containerName := "faultline-sandbox-" + randomID()
	args := s.dockerArgs(needsNetwork, containerName)
	args = append(args, command...)

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	s.logger.Debug("docker run", "container", containerName, "args", args)
	cmd := exec.CommandContext(ctx, "docker", args...)

	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		// Kill the container explicitly -- exec.CommandContext kills the docker CLI
		// process, but the container itself may keep running.
		s.logger.Warn("sandbox timeout, killing container", "container", containerName)
		killCmd := exec.Command("docker", "kill", containerName)
		// Best-effort: the container may already be gone, or docker may be
		// unreachable. We've already timed out; nothing useful to do here.
		_ = killCmd.Run()
		return string(output), fmt.Errorf("execution timed out after %s", s.timeout)
	}
	if err != nil {
		return string(output), fmt.Errorf("docker run failed: %w\nOutput: %s", err, string(output))
	}
	return string(output), nil
}

// ---------------------------------------------------------------------------
// Script execution
// ---------------------------------------------------------------------------

// Execute runs a Python script inside the sandbox container.
// Performs uv sync first to ensure dependencies are installed, then runs the script.
// args are passed as command-line arguments to the script.
func (s *Sandbox) Execute(ctx context.Context, script string, args []string) (string, error) {
	if err := s.validateFilename(script); err != nil {
		return "", fmt.Errorf("script name: %w", err)
	}

	// Verify script exists on host before starting a container
	scriptPath := filepath.Join(s.dir, "scripts", script)
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return "", fmt.Errorf("script %q not found in scripts/", script)
	}

	// Build the shell command: sync then run
	// uv sync is quiet (--quiet) to keep output clean
	cmdParts := []string{
		"sh", "-c",
		s.buildExecCommand(script, args),
	}

	start := time.Now()
	output, err := s.dockerRun(ctx, false, cmdParts...)
	elapsed := time.Since(start)

	// Log before truncation so the full output is captured
	detail := script
	if len(args) > 0 {
		detail += " " + strings.Join(args, " ")
	}
	s.logExec("execute", detail, elapsed, output, err)

	output = s.truncateOutput(output, "Write large output to /output/ from your script and read it back with sandbox_read.")

	if err != nil {
		return fmt.Sprintf("Error: %s\n\nOutput:\n%s", err, output), nil
	}

	if output == "" {
		return "(script produced no output)", nil
	}

	return output, nil
}

// truncateOutput clips output at the configured limit and appends a hint
// telling the agent how much was lost and what to do about it. The hint
// suggestion is caller-supplied so script vs. shell paths can give
// different advice.
func (s *Sandbox) truncateOutput(output, suggestion string) string {
	limit := s.outputLimit
	if limit <= 0 {
		limit = defaultSandboxOutputLimit
	}
	if len(output) <= limit {
		return output
	}
	return output[:limit] + fmt.Sprintf(
		"\n\n[Output truncated: showing first %d of %d chars. %s]",
		limit, len(output), suggestion)
}

// buildExecCommand constructs the shell command string for executing a script.
func (s *Sandbox) buildExecCommand(script string, args []string) string {
	// uv sync --quiet to install deps, then uv run to execute the script
	cmd := "uv sync --quiet 2>&1 && uv run python /scripts/" + shellQuote(script)
	for _, arg := range args {
		// Shell-escape each argument
		cmd += " " + shellQuote(arg)
	}
	return cmd
}

// shellQuote wraps a string in single quotes for safe shell interpolation.
func shellQuote(s string) string {
	// Replace single quotes with '\'' (end quote, escaped quote, start quote)
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---------------------------------------------------------------------------
// Shell execution
// ---------------------------------------------------------------------------

// ShellExec runs an arbitrary shell command inside the sandbox container.
// This gives the agent access to tools like git, ls, cat, wc, grep, etc.
// without needing to write Python wrapper scripts.
// The command runs in the same Docker image with the same mounts and limits
// as sandbox script execution.
func (s *Sandbox) ShellExec(ctx context.Context, command string) (string, error) {
	if command == "" {
		return "", fmt.Errorf("command is required")
	}

	// Safety: reject commands that try to break out of the sandbox
	if len(command) > 4096 {
		return "", fmt.Errorf("command too long (max 4096 chars)")
	}

	start := time.Now()
	output, err := s.dockerRun(ctx, false, "sh", "-c", command)
	elapsed := time.Since(start)

	detail := command[:min(len(command), 100)]
	s.logExec("shell_exec", detail, elapsed, output, err)

	output = s.truncateOutput(output, "Redirect large output to a file in /output/ (e.g. `cmd > /output/result.txt`) and read it back with sandbox_read.")

	if err != nil {
		return fmt.Sprintf("Error: %s\n\nOutput:\n%s", err, output), nil
	}

	if output == "" {
		return "(command produced no output)", nil
	}

	return output, nil
}

// min returns the smaller of a and b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Package management
// ---------------------------------------------------------------------------

// InstallPackage adds a package to the sandbox environment via uv add.
func (s *Sandbox) InstallPackage(ctx context.Context, pkg string) (string, error) {
	if pkg == "" {
		return "", fmt.Errorf("package name is required")
	}
	start := time.Now()
	output, err := s.dockerRun(ctx, true, "uv", "add", pkg)
	s.logExec("install_package", pkg, time.Since(start), output, err)
	if err != nil {
		return "", fmt.Errorf("install %q: %w", pkg, err)
	}
	s.logger.Info("sandbox package installed", "package", pkg)
	return output, nil
}

// UpgradePackage upgrades a package in the sandbox environment.
func (s *Sandbox) UpgradePackage(ctx context.Context, pkg string) (string, error) {
	if pkg == "" {
		return "", fmt.Errorf("package name is required")
	}
	// uv lock --upgrade-package then uv sync
	cmd := fmt.Sprintf("uv lock --upgrade-package %s 2>&1 && uv sync 2>&1", shellQuote(pkg))
	start := time.Now()
	output, err := s.dockerRun(ctx, true, "sh", "-c", cmd)
	s.logExec("upgrade_package", pkg, time.Since(start), output, err)
	if err != nil {
		return "", fmt.Errorf("upgrade %q: %w", pkg, err)
	}
	s.logger.Info("sandbox package upgraded", "package", pkg)
	return output, nil
}

// RemovePackage removes a package from the sandbox environment via uv remove.
func (s *Sandbox) RemovePackage(ctx context.Context, pkg string) (string, error) {
	if pkg == "" {
		return "", fmt.Errorf("package name is required")
	}
	start := time.Now()
	output, err := s.dockerRun(ctx, true, "uv", "remove", pkg)
	s.logExec("remove_package", pkg, time.Since(start), output, err)
	if err != nil {
		return "", fmt.Errorf("remove %q: %w", pkg, err)
	}
	s.logger.Info("sandbox package removed", "package", pkg)
	return output, nil
}

// ListPackages reads the pyproject.toml and returns the dependencies section.
func (s *Sandbox) ListPackages() (string, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, "pyproject.toml"))
	if err != nil {
		return "", fmt.Errorf("read pyproject.toml: %w", err)
	}

	// Parse out the dependencies list for a clean display.
	// This is a simple extraction -- not a full TOML parser -- but sufficient
	// since uv manages the file and keeps the format predictable.
	content := string(data)
	lines := strings.Split(content, "\n")

	var deps []string
	inDeps := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "dependencies = []" {
			return "No packages installed.", nil
		}
		if strings.HasPrefix(trimmed, "dependencies") && strings.Contains(trimmed, "[") {
			inDeps = true
			// Check for single-line dependencies = ["foo", "bar"]
			if strings.Contains(trimmed, "]") {
				// Extract inline list
				start := strings.Index(trimmed, "[")
				end := strings.LastIndex(trimmed, "]")
				inner := trimmed[start+1 : end]
				if strings.TrimSpace(inner) == "" {
					return "No packages installed.", nil
				}
				for _, d := range strings.Split(inner, ",") {
					d = strings.TrimSpace(d)
					d = strings.Trim(d, "\"")
					if d != "" {
						deps = append(deps, d)
					}
				}
				break
			}
			continue
		}
		if inDeps {
			if trimmed == "]" {
				break
			}
			d := strings.TrimSpace(trimmed)
			d = strings.Trim(d, ",\"")
			if d != "" {
				deps = append(deps, d)
			}
		}
	}

	if len(deps) == 0 {
		return "No packages installed.", nil
	}

	var sb strings.Builder
	sb.WriteString("Installed packages:\n")
	for _, d := range deps {
		fmt.Fprintf(&sb, "  - %s\n", d)
	}
	return sb.String(), nil
}
