package tools

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	skillsfs "github.com/matjam/faultline/internal/adapters/skills/fs"
	"github.com/matjam/faultline/internal/llm"
	"github.com/matjam/faultline/internal/skills"
)

// Install caps. The agent should never need to fetch a skill larger
// than a few MB; capping the download protects against a buggy or
// malicious URL pointing at a multi-gigabyte payload.
const (
	skillInstallMaxBytes  = 50 * 1024 * 1024 // 50 MiB
	skillInstallTimeout   = 5 * time.Minute  // covers slow git clones over poor links
	skillInstallUserAgent = "faultline-skill-install/1"
)

// Recognized tarball/zip extensions. Order matters: longest suffixes
// first so .tar.gz isn't mis-detected as .gz.
var archiveExtensions = []struct {
	suffix string
	kind   string
}{
	{".tar.gz", "tarball"},
	{".tgz", "tarball"},
	{".tar", "tarball"},
	{".zip", "zip"},
}

// skillInstallToolDef returns the tool definition advertised when
// install_enabled is on. Kept separate from skillToolDefs so the
// default path (install disabled) doesn't accidentally surface the
// tool through a code-path mistake.
func (te *Executor) skillInstallToolDef() llm.Tool {
	return llm.Tool{
		Type: llm.ToolTypeFunction,
		Function: &llm.FunctionDef{
			Name: "skill_install",
			Description: "Install a new Agent Skill from a remote source into the configured skills directory. " +
				"Sources: a tarball URL (.tar.gz, .tgz, .tar, .zip), a git URL (ends in .git, starts with git@, or git+https://), or a plain GitHub repo URL. " +
				"The download is extracted to a temp directory, validated (must contain a SKILL.md), then moved into the skills root. " +
				"Existing skills are NOT overwritten -- delete first if you want to update. The catalog is reloaded automatically so the skill is immediately available via skill_activate.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"source": map[string]interface{}{
						"type":        "string",
						"description": "URL to fetch. Examples: 'https://example.com/skills/pdf.tar.gz', 'https://github.com/owner/repo.git', 'https://github.com/owner/repo'.",
					},
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Override the destination directory name (must match the SKILL.md frontmatter's name field). When omitted, inferred from the source URL.",
					},
					"subpath": map[string]interface{}{
						"type":        "string",
						"description": "When the SKILL.md is not at the root of the archive/repo (e.g. a monorepo containing many skills), the path within the extracted tree to the skill directory. Examples: 'skills/pdf-processing'.",
					},
					"kind": map[string]interface{}{
						"type":        "string",
						"description": "Optional explicit source kind. Useful when auto-detection would pick the wrong path. One of: 'tarball', 'zip', 'git'.",
						"enum":        []string{"tarball", "zip", "git"},
					},
				},
				"required": []string{"source"},
			},
		},
	}
}

// skillInstall is the tool handler for skill_install. Returns a
// human-readable result string for the LLM.
func (te *Executor) skillInstall(ctx context.Context, argsJSON string) string {
	if te.skills == nil {
		return "Error: skills support is not enabled."
	}
	if !te.skillInstallEnabled {
		return "Error: skill_install is disabled. Enable [skills] install_enabled in config to use this tool."
	}
	if te.skillsRoot == "" {
		return "Error: skills root is not configured."
	}

	var p struct {
		Source  string `json:"source"`
		Name    string `json:"name"`
		Subpath string `json:"subpath"`
		Kind    string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &p); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	p.Source = strings.TrimSpace(p.Source)
	p.Name = strings.TrimSpace(p.Name)
	p.Subpath = strings.TrimSpace(p.Subpath)
	p.Kind = strings.TrimSpace(p.Kind)

	if p.Source == "" {
		return "Error: 'source' is required."
	}

	kind, normalisedSource, err := detectSourceKind(p.Source, p.Kind)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	// Decide on the destination name now (before the network call) so
	// we can reject obvious "already exists" / "invalid name" cases
	// without burning a download.
	desiredName := p.Name
	if desiredName == "" {
		desiredName = inferSkillName(normalisedSource, p.Subpath)
	}
	if err := skills.ValidateName(desiredName); err != nil {
		return fmt.Sprintf("Error: derived name %q is invalid: %s. Pass an explicit `name` parameter that matches the spec (lowercase letters/digits/hyphens, no leading/trailing/consecutive hyphens).", desiredName, err)
	}
	dest := filepath.Join(te.skillsRoot, desiredName)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Sprintf("Error: a skill named %q already exists at %s. Delete it manually first if you want to reinstall.", desiredName, dest)
	}

	// Bounded context for the whole fetch+extract+validate dance.
	installCtx, cancel := context.WithTimeout(ctx, skillInstallTimeout)
	defer cancel()

	// Stage in a temp directory under the skills root so move-into-
	// place is a same-filesystem rename (atomic on most filesystems).
	tempDir, err := os.MkdirTemp(te.skillsRoot, ".install-*")
	if err != nil {
		return fmt.Sprintf("Error creating temp dir: %s", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tempDir)
		}
	}()

	te.logger.Info("skill_install: starting",
		slog.String("source", normalisedSource),
		slog.String("kind", kind),
		slog.String("name", desiredName))

	switch kind {
	case "tarball":
		err = downloadAndExtractTar(installCtx, te.http, normalisedSource, tempDir)
	case "zip":
		err = downloadAndExtractZip(installCtx, te.http, normalisedSource, tempDir)
	case "git":
		err = gitClone(installCtx, normalisedSource, tempDir, te.logger)
	default:
		err = fmt.Errorf("unsupported source kind: %s", kind)
	}
	if err != nil {
		return fmt.Sprintf("Error fetching skill: %s", err)
	}

	// Resolve the actual skill directory inside what we extracted.
	// Many tarballs wrap their contents in a single top-level
	// directory (the GitHub archive convention), and operators may
	// also point at a subpath within a monorepo via the `subpath`
	// argument.
	skillRoot, err := resolveSkillRoot(tempDir, p.Subpath)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	// Validate before moving. We parse the SKILL.md the same way the
	// runtime catalog does so a skill that won't load is rejected
	// here rather than silently sitting broken in the skills root.
	mdPath := filepath.Join(skillRoot, "SKILL.md")
	mdBytes, err := os.ReadFile(mdPath)
	if err != nil {
		return fmt.Sprintf("Error: SKILL.md not found at %s. If the SKILL.md is in a subdirectory, pass it via the `subpath` parameter.", relPathOrSelf(tempDir, mdPath))
	}
	parsed, err := skillsfs.LoadSkillForValidation(mdBytes, skillRoot, mdPath, desiredName)
	if err != nil {
		return fmt.Sprintf("Error: SKILL.md is malformed: %s", err)
	}
	if parsed.Name != desiredName {
		// LoadSkillForValidation uses dirName as the canonical name
		// when frontmatter disagrees, so this branch only fires when
		// the operator passed a name that disagrees with what the
		// frontmatter specifies. Surface it clearly.
		return fmt.Sprintf("Error: SKILL.md frontmatter name %q does not match destination directory name %q. Pass --name=%q (or rename the destination) to install with the spec-mandated matching name.", parsed.Name, desiredName, parsed.Name)
	}

	// All-clear: move skillRoot into place. If skillRoot is the temp
	// dir itself, rename it; otherwise rename the subdir into place
	// and clean up the temp dir.
	if skillRoot == tempDir {
		if err := os.Rename(tempDir, dest); err != nil {
			return fmt.Sprintf("Error moving skill into place: %s", err)
		}
		cleanup = false
	} else {
		if err := os.Rename(skillRoot, dest); err != nil {
			return fmt.Sprintf("Error moving skill into place: %s", err)
		}
		// Best-effort temp-dir cleanup; the rest of the archive
		// contents are not the agent's problem.
	}

	if err := te.skills.Reload(); err != nil {
		te.logger.Warn("skill_install: catalog reload failed; will pick up at next rebuild",
			slog.String("err", err.Error()))
	}

	resourceCount, _ := countResources(dest)
	te.logger.Info("skill_install: ok",
		slog.String("name", desiredName),
		slog.String("dest", dest))
	return fmt.Sprintf(
		"Installed skill %q into %s (%d bundled resource files). It is now available via skill_activate.",
		desiredName, dest, resourceCount)
}

// detectSourceKind inspects the source URL and the optional explicit
// kind override and returns the kind to use plus a normalised source
// URL. Recognized kinds: "tarball", "zip", "git". A plain GitHub
// repo URL is normalised to the .git form so it can be cloned.
func detectSourceKind(source, override string) (string, string, error) {
	switch override {
	case "tarball", "zip", "git":
		return override, source, nil
	case "":
		// auto-detect below
	default:
		return "", source, fmt.Errorf("unknown kind %q (expected one of: tarball, zip, git)", override)
	}

	low := strings.ToLower(source)
	for _, ext := range archiveExtensions {
		if strings.Contains(low, ext.suffix+"?") || strings.HasSuffix(low, ext.suffix) {
			return ext.kind, source, nil
		}
	}

	// Git suffix forms.
	if strings.HasSuffix(low, ".git") || strings.HasPrefix(low, "git@") || strings.HasPrefix(low, "git+") {
		return "git", strings.TrimPrefix(source, "git+"), nil
	}

	// Plain GitHub repo URL: https://github.com/owner/repo with no
	// further path. Normalise to .git so git clone handles it.
	if u, err := url.Parse(source); err == nil && u.Scheme != "" {
		host := strings.ToLower(u.Host)
		if (host == "github.com" || host == "gitlab.com" || host == "codeberg.org") && countSegments(u.Path) == 2 {
			return "git", strings.TrimRight(source, "/") + ".git", nil
		}
	}

	return "", source, fmt.Errorf("could not infer source kind from %q. Pass `kind` (one of: tarball, zip, git) explicitly, or provide a URL ending in .tar.gz/.tgz/.tar/.zip/.git", source)
}

// countSegments returns the number of non-empty path segments in p.
// Used to recognize "owner/repo" GitHub-style URLs that don't have
// further path components like /tree/main/whatever.
func countSegments(p string) int {
	n := 0
	for _, seg := range strings.Split(p, "/") {
		if seg != "" {
			n++
		}
	}
	return n
}

// inferSkillName derives a destination directory name from the source
// URL when the caller didn't supply one. Strips common archive and
// git suffixes, lowercases, and applies hyphen-only normalisation so
// the result has the best shot at matching the spec's name rules.
func inferSkillName(source, subpath string) string {
	if subpath != "" {
		return sanitiseName(path.Base(filepath.ToSlash(subpath)))
	}
	u, err := url.Parse(source)
	if err != nil {
		return ""
	}
	base := path.Base(strings.TrimRight(u.Path, "/"))
	for _, suffix := range []string{".tar.gz", ".tgz", ".tar", ".zip", ".git"} {
		if strings.HasSuffix(strings.ToLower(base), suffix) {
			base = base[:len(base)-len(suffix)]
			break
		}
	}
	return sanitiseName(base)
}

// sanitiseName lowercases and replaces invalid characters with
// hyphens, collapsing runs and trimming leading/trailing hyphens.
// Best-effort -- the caller still validates against the spec.
func sanitiseName(in string) string {
	if in == "" {
		return ""
	}
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(in) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r == '-' || r == '_' || r == ' ' || r == '.':
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	return out
}

// downloadAndExtractTar streams a (possibly gzipped) tar from URL into
// dst. Caps the total extracted bytes at skillInstallMaxBytes to
// guard against zip-bomb-style attacks; rejects entries with absolute
// paths or ".." segments.
func downloadAndExtractTar(ctx context.Context, client *http.Client, source, dst string) error {
	body, closeBody, err := openHTTP(ctx, client, source)
	if err != nil {
		return err
	}
	defer closeBody()

	var reader io.Reader
	if isGzipped(source) {
		gz, err := gzip.NewReader(io.LimitReader(body, skillInstallMaxBytes+1))
		if err != nil {
			return fmt.Errorf("gunzip: %w", err)
		}
		defer gz.Close()
		reader = gz
	} else {
		reader = io.LimitReader(body, skillInstallMaxBytes+1)
	}

	tr := tar.NewReader(reader)
	var totalBytes int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode(hdr.Mode))
			if err != nil {
				return err
			}
			n, err := io.CopyN(out, tr, skillInstallMaxBytes-totalBytes+1)
			out.Close()
			if err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("write %s: %w", target, err)
			}
			totalBytes += n
			if totalBytes > skillInstallMaxBytes {
				return fmt.Errorf("archive exceeds %d bytes; refusing to extract", skillInstallMaxBytes)
			}
		default:
			// Skip symlinks, devices, etc. -- no legitimate skill
			// needs them, and they're a sandbox-escape risk.
			continue
		}
	}
	return nil
}

// downloadAndExtractZip streams a zip file from URL into dst. Same
// safety guards as downloadAndExtractTar.
func downloadAndExtractZip(ctx context.Context, client *http.Client, source, dst string) error {
	body, closeBody, err := openHTTP(ctx, client, source)
	if err != nil {
		return err
	}
	defer closeBody()

	// archive/zip needs a ReaderAt; buffer to a temp file rather than
	// memory so a large-but-legal zip doesn't blow up RSS.
	tmp, err := os.CreateTemp("", "faultline-zip-*.zip")
	if err != nil {
		return fmt.Errorf("buffer zip: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	written, err := io.CopyN(tmp, body, skillInstallMaxBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("download zip: %w", err)
	}
	if written > skillInstallMaxBytes {
		return fmt.Errorf("archive exceeds %d bytes; refusing to extract", skillInstallMaxBytes)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	zr, err := zip.NewReader(tmp, written)
	if err != nil {
		return fmt.Errorf("zip read: %w", err)
	}
	var totalBytes int64
	for _, f := range zr.File {
		target, err := safeJoin(dst, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open %s in zip: %w", f.Name, err)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode(int64(f.Mode().Perm())))
		if err != nil {
			rc.Close()
			return err
		}
		n, copyErr := io.CopyN(out, rc, skillInstallMaxBytes-totalBytes+1)
		rc.Close()
		out.Close()
		if copyErr != nil && !errors.Is(copyErr, io.EOF) {
			return fmt.Errorf("write %s: %w", target, copyErr)
		}
		totalBytes += n
		if totalBytes > skillInstallMaxBytes {
			return fmt.Errorf("archive exceeds %d bytes; refusing to extract", skillInstallMaxBytes)
		}
	}
	return nil
}

// gitClone shells out to the local git binary to do a shallow clone
// into dst. Requires git on PATH; surfaced as a tool-result error if
// not present.
func gitClone(ctx context.Context, source, dst string, logger *slog.Logger) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git binary not found in PATH; install git or use a tarball URL instead")
	}
	// Empty the temp dir so git clone treats it as a fresh
	// destination (clone refuses to write into a non-empty dir).
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--single-branch", source, dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	logger.Debug("skill_install: git clone ok", slog.String("source", source))

	// Strip the .git directory so the extracted tree matches what a
	// tarball would have given us.
	_ = os.RemoveAll(filepath.Join(dst, ".git"))
	return nil
}

// resolveSkillRoot picks the directory inside `extracted` that
// actually contains SKILL.md. Three cases:
//
//  1. Caller specified `subpath`: trust them and use it.
//  2. SKILL.md exists at the root: use the root.
//  3. The extracted tree has exactly one top-level directory and
//     that directory contains SKILL.md (or the recursive expansion
//     of case 2 inside it): GitHub archive convention -- unwrap.
//
// Anything else returns an error pointing the caller at `subpath`.
func resolveSkillRoot(extracted, subpath string) (string, error) {
	if subpath != "" {
		// Reject path-escape attempts in user-supplied subpath.
		clean := filepath.Clean(subpath)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return "", fmt.Errorf("subpath %q escapes the extracted directory", subpath)
		}
		candidate := filepath.Join(extracted, clean)
		if _, err := os.Stat(filepath.Join(candidate, "SKILL.md")); err == nil {
			return candidate, nil
		}
		return "", fmt.Errorf("subpath %q does not contain a SKILL.md", subpath)
	}

	if _, err := os.Stat(filepath.Join(extracted, "SKILL.md")); err == nil {
		return extracted, nil
	}

	// Fall back to single-top-level-dir unwrapping.
	entries, err := os.ReadDir(extracted)
	if err != nil {
		return "", err
	}
	var dirs []os.DirEntry
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, e)
		}
	}
	if len(dirs) == 1 {
		inner := filepath.Join(extracted, dirs[0].Name())
		if _, err := os.Stat(filepath.Join(inner, "SKILL.md")); err == nil {
			return inner, nil
		}
	}

	return "", fmt.Errorf("SKILL.md not found at the archive root or inside a single top-level directory; pass `subpath` if the SKILL.md lives deeper")
}

// openHTTP issues a GET with the install user-agent and a
// caller-supplied context. Returns the body plus a closer to be
// deferred; non-2xx responses are surfaced as errors.
func openHTTP(ctx context.Context, client *http.Client, source string) (io.Reader, func(), error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", skillInstallUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("http get: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("http %s: %s", resp.Status, source)
	}
	return resp.Body, func() { _ = resp.Body.Close() }, nil
}

// isGzipped is a poor-man's content type sniff: tarballs are always
// served either with a .gz suffix or as a plain .tar. We rely on the
// URL's apparent extension since most servers don't bother with
// honest Content-Type for archives.
func isGzipped(source string) bool {
	low := strings.ToLower(source)
	return strings.Contains(low, ".tar.gz") || strings.Contains(low, ".tgz")
}

// safeJoin resolves a joined path and rejects results that escape
// the parent directory via ".." or absolute paths. Mirrors the
// guard the skills adapter applies for resource reads, but for
// archive extraction.
func safeJoin(parent, child string) (string, error) {
	if filepath.IsAbs(child) {
		return "", fmt.Errorf("archive entry has absolute path: %s", child)
	}
	clean := filepath.Clean(filepath.Join(parent, child))
	rel, err := filepath.Rel(parent, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry escapes target dir: %s", child)
	}
	return clean, nil
}

// fileMode masks the on-disk permission bits we will accept from an
// archive. Strips setuid/setgid/sticky and never grants execute to
// world; the agent's writable filesystem doesn't need them.
func fileMode(mode int64) os.FileMode {
	return os.FileMode(mode) & 0o755
}

// relPathOrSelf returns p relative to base if it can; otherwise
// returns p as-is. Used to keep error messages compact.
func relPathOrSelf(base, p string) string {
	if rel, err := filepath.Rel(base, p); err == nil {
		return rel
	}
	return p
}

// countResources is a thin wrapper that returns just the count of
// non-directory entries in a freshly-installed skill, used in the
// success message. Errors are swallowed -- the count is informational.
func countResources(skillDir string) (int, error) {
	var n int
	err := filepath.WalkDir(skillDir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return n, err
}
