package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/matjam/faultline/internal/adapters/sandbox/docker"
	skillsfs "github.com/matjam/faultline/internal/adapters/skills/fs"
	"github.com/matjam/faultline/internal/llm"
)

// skillsAvailable returns true when the Skills feature is wired up and
// the catalog currently contains at least one skill. Tier-1 disclosure
// is conditional on this -- the spec is explicit that an empty
// <available_skills/> block confuses the model.
func (te *Executor) skillsAvailable() bool {
	return te.skills != nil && len(te.skills.List()) > 0
}

// skillToolDefs returns the skill_* tool definitions when skills are
// configured. The activate/read/execute/work_read tools require at
// least one skill to be in the catalog (an empty skill_activate
// surface confuses the model per the spec). skill_install -- when
// enabled -- is advertised even with an empty catalog so the agent
// can bootstrap the first skill.
//
// The `name` parameter on activate/read/execute is constrained to the
// current set of skill names via an enum, so the model can't
// hallucinate a non-existent skill -- per the agentskills.io
// implementation guidance.
func (te *Executor) skillToolDefs() []llm.Tool {
	var defs []llm.Tool

	// skill_install can be advertised independently of the catalog
	// being non-empty, so the agent can install the first skill
	// from a fresh deployment. Still gated on Skills being
	// configured at all.
	if te.skills != nil && te.skillInstallEnabled {
		defs = append(defs, te.skillInstallToolDef())
	}

	if !te.skillsAvailable() {
		return defs
	}

	names := make([]string, 0)
	for _, s := range te.skills.List() {
		names = append(names, s.Name)
	}
	sort.Strings(names)

	nameSchema := map[string]interface{}{
		"type":        "string",
		"description": "The skill's name as listed in the system prompt's Available Skills section.",
		"enum":        names,
	}

	defs = append(defs, []llm.Tool{
		{
			Type: llm.ToolTypeFunction,
			Function: &llm.FunctionDef{
				Name:        "skill_activate",
				Description: "Load the full instructions for a skill listed in the system prompt's Available Skills section. Returns the SKILL.md body (frontmatter stripped) wrapped in <skill_content> tags, plus a list of bundled scripts and references the skill exposes. Call this when a task matches a skill's description; the returned instructions tell you how to perform the task.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": nameSchema,
					},
					"required": []string{"name"},
				},
			},
		},
		{
			Type: llm.ToolTypeFunction,
			Function: &llm.FunctionDef{
				Name:        "skill_read",
				Description: "Read a bundled resource file from a skill's directory (e.g. references/REFERENCE.md, assets/template.txt, or a script's source for inspection). Path is relative to the skill's root and must not escape it. Use this on demand when a skill's instructions reference a supporting file.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": nameSchema,
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Resource path relative to the skill directory. Examples: 'scripts/extract.py', 'references/REFERENCE.md', 'assets/template.txt'.",
						},
					},
					"required": []string{"name", "path"},
				},
			},
		},
		{
			Type: llm.ToolTypeFunction,
			Function: &llm.FunctionDef{
				Name:        "skill_execute",
				Description: "Run a shell command inside an isolated Docker container with the skill's directory mounted at /skill (read-only) and a fresh per-call /work directory mounted read-write. The skill cannot see the agent's memory, the regular sandbox, or any other skill's directory. Working directory is /skill. Returns combined stdout/stderr plus a manifest of files the command created in /work; fetch any of those with skill_work_read using the returned work_id. Use this to run scripts the skill bundles.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": nameSchema,
						"command": map[string]interface{}{
							"type":        "string",
							"description": "Shell command to execute. The skill's SKILL.md tells you how to invoke its scripts. Examples: 'python scripts/extract.py /work/input.pdf', 'bun scripts/run.ts --output /work/result.json', 'go run scripts/main.go'.",
						},
						"network": map[string]interface{}{
							"type":        "boolean",
							"description": "Allow network access during execution. Defaults to false (network disabled). Some skills genuinely need internet access -- the SKILL.md's compatibility field will say so.",
						},
					},
					"required": []string{"name", "command"},
				},
			},
		},
		{
			Type: llm.ToolTypeFunction,
			Function: &llm.FunctionDef{
				Name:        "skill_work_read",
				Description: "Read a file from a /work directory produced by a previous skill_execute call. The work_id is returned in skill_execute's manifest. Use this to fetch files the skill produced as side effects (extracted text, generated reports, etc.) instead of relying on stdout for large output.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"work_id": map[string]interface{}{
							"type":        "string",
							"description": "The work_id returned by a previous skill_execute call.",
						},
						"path": map[string]interface{}{
							"type":        "string",
							"description": "File path within the /work directory. Examples: 'output.txt', 'results/data.json'.",
						},
					},
					"required": []string{"work_id", "path"},
				},
			},
		},
	}...)
	return defs
}

// skillActivate returns the SKILL.md body (frontmatter already
// stripped during parsing) wrapped in <skill_content> tags, plus a
// list of bundled resources from the conventional scripts/,
// references/, and assets/ subdirectories. Errors are returned as
// human-readable strings (no separate error channel for tool
// results).
func (te *Executor) skillActivate(args string) string {
	if te.skills == nil {
		return "Error: skills support is not enabled."
	}

	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if p.Name == "" {
		return "Error: 'name' is required."
	}

	sk, err := te.skills.Get(p.Name)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	resources, truncated, err := te.skills.Resources(sk.Name)
	if err != nil {
		te.logger.Warn("skills: resources enumeration failed",
			slog.String("name", sk.Name),
			slog.String("err", err.Error()))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "<skill_content name=%q>\n\n", sk.Name)
	sb.WriteString(sk.Body)
	sb.WriteString("\n\n")
	fmt.Fprintf(&sb, "Skill directory: %s\n", sk.Dir)
	sb.WriteString("Relative paths in this skill resolve against the skill directory.\n\n")

	if len(resources) > 0 {
		sb.WriteString("<skill_resources>\n")
		for _, r := range resources {
			if r.IsDir {
				continue
			}
			fmt.Fprintf(&sb, "  <file size=\"%d\">%s</file>\n", r.Size, r.Path)
		}
		if truncated {
			fmt.Fprintf(&sb, "  <!-- listing truncated at %d entries; the skill directory has more files than shown -->\n", skillsfs.MaxResourceListing)
		}
		sb.WriteString("</skill_resources>\n\n")
	}

	if sk.Compatibility != "" {
		fmt.Fprintf(&sb, "Compatibility: %s\n\n", sk.Compatibility)
	}

	sb.WriteString("</skill_content>\n")
	return sb.String()
}

// skillRead reads one resource file from a skill's directory.
func (te *Executor) skillRead(args string) string {
	if te.skills == nil {
		return "Error: skills support is not enabled."
	}

	var p struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if p.Name == "" || p.Path == "" {
		return "Error: 'name' and 'path' are both required."
	}

	content, err := te.skills.Read(p.Name, p.Path)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return content
}

// skillExecute runs a shell command inside the docker sandbox with
// only the named skill's directory mounted at /skill (ro) plus a
// per-call /work scratch dir (rw). Returns combined stdout/stderr,
// the work_id (so the agent can fetch /work files via
// skill_work_read), and a manifest of files left in /work.
func (te *Executor) skillExecute(ctx context.Context, args string) string {
	if te.skills == nil {
		return "Error: skills support is not enabled."
	}
	if te.sandbox == nil {
		return "Error: sandbox is not enabled; skill_execute requires a working Docker sandbox."
	}

	var p struct {
		Name    string `json:"name"`
		Command string `json:"command"`
		Network bool   `json:"network"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if p.Name == "" || p.Command == "" {
		return "Error: 'name' and 'command' are both required."
	}

	sk, err := te.skills.Get(p.Name)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}

	// Per-call /work dir under <sandbox>/skill-work/<call-id>/. The
	// agent loop wiped the parent at startup, so we never collide
	// with stale entries.
	workID := newWorkID()
	workDir := filepath.Join(te.sandbox.SkillWorkRoot(), workID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return fmt.Sprintf("Error preparing /work directory: %s", err)
	}

	// uv-using skills need a writable UV_PROJECT_ENVIRONMENT path
	// because /skill is read-only. /work/.venv puts the venv inside
	// the per-call dir, so it's wiped at session reset and doesn't
	// leak between skill invocations.
	env := map[string]string{
		"UV_PROJECT_ENVIRONMENT": "/work/.venv",
	}
	mounts := []docker.Mount{
		{HostPath: sk.Dir, ContainerPath: "/skill", ReadOnly: true},
		{HostPath: workDir, ContainerPath: "/work", ReadOnly: false},
		// Share /cache with the regular sandbox so package downloads
		// (uv, pip, npm, etc.) are reused across runs. Skills are
		// trusted, and uv/npm caches are content-addressable so
		// inter-skill collisions are not a concern.
		{HostPath: filepath.Join(te.sandbox.Dir(), "cache"), ContainerPath: "/cache", ReadOnly: false},
	}

	output, runErr := te.sandbox.ExecuteIsolated(ctx, p.Command, mounts, p.Network, "/skill", env)

	manifest := buildWorkManifest(workDir)
	output = te.sandbox.Truncate(output, "Write large output to /work/ from your script and read it back with skill_work_read.")

	var sb strings.Builder
	fmt.Fprintf(&sb, "work_id: %s\n\n", workID)
	if runErr != nil {
		fmt.Fprintf(&sb, "Error: %s\n\n", runErr)
	}
	if output == "" {
		sb.WriteString("(no stdout/stderr)\n\n")
	} else {
		// Skill stdout/stderr can include data the skill fetched
		// from the network; treat as untrusted.
		sb.WriteString(wrapUntrusted(fmt.Sprintf("skill %s stdout/stderr", p.Name), output))
		sb.WriteString("\n\n")
	}
	if len(manifest) == 0 {
		sb.WriteString("/work is empty; the skill produced no files.\n")
	} else {
		sb.WriteString("Files in /work:\n")
		for _, e := range manifest {
			fmt.Fprintf(&sb, "  - %s (%d bytes)\n", e.Path, e.Size)
		}
		fmt.Fprintf(&sb, "\nUse skill_work_read with work_id=%q to read any of these.\n", workID)
	}
	return sb.String()
}

// skillWorkRead returns the contents of a file inside a previous
// skill_execute call's /work directory.
func (te *Executor) skillWorkRead(args string) string {
	if te.sandbox == nil {
		return "Error: sandbox is not enabled."
	}

	var p struct {
		WorkID string `json:"work_id"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if p.WorkID == "" || p.Path == "" {
		return "Error: 'work_id' and 'path' are both required."
	}

	// Validate work_id shape so a malicious value can't traverse out
	// of the skill-work root via .. or absolute paths.
	if !validWorkID(p.WorkID) {
		return "Error: invalid work_id."
	}

	root := filepath.Join(te.sandbox.SkillWorkRoot(), p.WorkID)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("Error: work_id %q not found (it may have been wiped on agent restart).", p.WorkID)
		}
		return fmt.Sprintf("Error: stat work dir: %s", err)
	}

	target, err := resolveWorkPath(root, p.Path)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return fmt.Sprintf("Error: read %s: %s", p.Path, err)
	}
	out := string(data)
	if te.sandbox != nil {
		out = te.sandbox.Truncate(out, "File too large for one read; rerun the skill with output split or written in chunks.")
	}
	// Skill code can write network-fetched content into /work files;
	// the byte-level content is untrusted even when the path is
	// operator-validated.
	source := fmt.Sprintf("skill /work file: %s (work_id=%s)", p.Path, p.WorkID)
	return wrapUntrusted(source, out)
}

// workManifestEntry is one entry in the post-execute /work listing.
type workManifestEntry struct {
	Path string
	Size int64
}

// buildWorkManifest walks the /work host directory after a skill
// execution and returns a flat list of files (not directories). Used
// to surface whatever the skill produced so the agent knows what to
// fetch via skill_work_read.
func buildWorkManifest(workDir string) []workManifestEntry {
	var out []workManifestEntry
	_ = filepath.WalkDir(workDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// Skip the hidden uv venv we baked into UV_PROJECT_ENVIRONMENT;
		// the agent never wants to read those files and they would
		// drown out any real output.
		rel, relErr := filepath.Rel(workDir, path)
		if relErr != nil {
			return nil
		}
		if strings.HasPrefix(rel, ".venv/") || rel == ".venv" {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		out = append(out, workManifestEntry{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// resolveWorkPath joins workRoot + relPath and verifies the result
// stays under workRoot. Mirrors the path-escape guard the skills
// adapter applies for its own resources.
func resolveWorkPath(workRoot, relPath string) (string, error) {
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("absolute paths not allowed")
	}
	clean := filepath.Clean(filepath.Join(workRoot, relPath))
	rel, err := filepath.Rel(workRoot, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the work directory", relPath)
	}
	return clean, nil
}

// newWorkID returns a 12-hex-character random identifier safe for use
// as a directory name. Length is enough to make collisions
// astronomically unlikely within a session.
func newWorkID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// validWorkID checks the work_id matches the format newWorkID
// produces: 12 lowercase hex characters. Anything else is treated as
// invalid input rather than as an existence-check at the filesystem
// level (defense in depth against path-traversal-style abuse).
func validWorkID(id string) bool {
	if len(id) != 12 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
