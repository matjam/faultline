// Package prompts loads and renders the agent's mutable prompt templates.
//
// The default prompt contents are embedded into the binary at build time
// from internal/prompts/templates/. On first run, defaults are written to
// the agent's memory store; on subsequent runs they are loaded from disk,
// which lets the agent edit its own prompts and have those edits persist
// across restarts.
package prompts

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/search/bm25"
	"github.com/matjam/faultline/internal/skills"
	"github.com/matjam/faultline/internal/subagent"
)

// Embedded default prompt contents, compiled into the binary from templates/.
var (
	//go:embed templates/system.md
	defaultSystem string

	//go:embed templates/compaction.md
	defaultCompaction string

	//go:embed templates/cycle-start.md
	defaultCycleStart string

	//go:embed templates/continue.md
	defaultContinue string

	//go:embed templates/shutdown.md
	defaultShutdown string

	//go:embed templates/identity-core.md
	defaultIdentityCore string

	//go:embed templates/changelog.md
	defaultChangelog string
)

// Store is the persistence backend used to read and seed prompt files.
// The agent's MemoryStore satisfies this interface structurally.
type Store interface {
	Read(path string) (string, error)
	Write(path, content string) error
}

// Migrator extends Store with the file operations needed to apply
// one-time prompt-file renames at startup. *fs.Store satisfies it.
type Migrator interface {
	Store
	Move(src, dst string) error
}

// legacyRenames lists prompt files that have been renamed in the codebase
// and need a one-time migration on existing stores. Each pair maps the
// old path (deprecated) to the new path (current). Migrate processes
// these every startup; it is a no-op once migration has run.
var legacyRenames = []struct {
	old string
	new string
}{
	{old: "prompts/cycle_start.md", new: "prompts/cycle-start.md"},
}

// Migrate applies one-time prompt filename renames. Behavior per pair:
//
//   - both old and new exist: returns an error. The operator must
//     resolve the conflict manually (the agent might have meaningful
//     content in either file, and silently picking one risks data loss).
//   - only old exists: renames old -> new.
//   - only new exists, or neither: no-op.
//
// Idempotent and safe to call every startup.
func Migrate(store Migrator) error {
	for _, r := range legacyRenames {
		oldContent, oldErr := store.Read(r.old)
		_, newErr := store.Read(r.new)
		oldExists := oldErr == nil && oldContent != ""
		newExists := newErr == nil

		switch {
		case oldExists && newExists:
			return fmt.Errorf("prompt migration conflict: both %q and %q exist; remove one manually before starting", r.old, r.new)
		case oldExists && !newExists:
			if err := store.Move(r.old, r.new); err != nil {
				return fmt.Errorf("migrate %q -> %q: %w", r.old, r.new, err)
			}
		}
	}
	return nil
}

// promptFile defines a mutable prompt file with its default seed content.
type promptFile struct {
	path         string
	defaultValue string
}

// files maps prompt names to their memory paths and embedded defaults.
// Initialized in init() after the embed variables are populated.
var files map[string]promptFile

func init() {
	files = map[string]promptFile{
		"system": {
			path:         "prompts/system.md",
			defaultValue: defaultSystem,
		},
		"compaction": {
			path:         "prompts/compaction.md",
			defaultValue: defaultCompaction,
		},
		"cycle-start": {
			path:         "prompts/cycle-start.md",
			defaultValue: defaultCycleStart,
		},
		"continue": {
			path:         "prompts/continue.md",
			defaultValue: defaultContinue,
		},
		"shutdown": {
			path:         "prompts/shutdown.md",
			defaultValue: defaultShutdown,
		},
		"identity-core": {
			path:         "identity/core.md",
			defaultValue: defaultIdentityCore,
		},
		"changelog": {
			path:         "prompts/changelog.md",
			defaultValue: defaultChangelog,
		},
	}
}

// Load loads a prompt from the store, seeding the embedded default if it
// doesn't exist yet.
func Load(store Store, name string) (string, error) {
	pf, ok := files[name]
	if !ok {
		return "", fmt.Errorf("unknown prompt: %s", name)
	}

	content, err := store.Read(pf.path)
	if err == nil && content != "" {
		return content, nil
	}

	// First run - seed the default
	if err := store.Write(pf.path, pf.defaultValue); err != nil {
		return "", fmt.Errorf("write default prompt %s: %w", name, err)
	}
	return pf.defaultValue, nil
}

// LoadAll loads all prompt files, seeding defaults as needed. Returns a map
// of prompt name -> content.
func LoadAll(store Store) (map[string]string, error) {
	prompts := make(map[string]string)
	for name := range files {
		content, err := Load(store, name)
		if err != nil {
			return nil, err
		}
		prompts[name] = content
	}
	return prompts, nil
}

// Render takes a prompt template and substitutes known placeholders.
// Currently only {{TIME}} is supported.
func Render(template string, now time.Time) string {
	result := template
	result = strings.ReplaceAll(result, "{{TIME}}", now.Format(time.RFC1123))
	return result
}

// BuildCycleContext assembles the full system message with recent
// memories and (optionally) the skill catalog. Both sections are
// omitted entirely when their input slice is empty -- no empty
// headers in the rendered prompt.
//
// memoryCharLimit caps the per-entry memory excerpt size; when
// exceeded, a retrieval hint is appended pointing the agent at
// memory_read. A non-positive limit disables the cap.
//
// skillCatalog, when non-empty, is rendered as an "## Available
// Skills" section with a brief instruction telling the agent to call
// skill_activate when a task matches a skill's description. Each
// entry costs ~50-100 tokens, matching the spec's tier-1 disclosure.
func BuildCycleContext(systemPrompt string, memories []bm25.Result, skillCatalog []skills.Skill, subagentCatalog []subagent.Catalog, now time.Time, memoryCharLimit int) string {
	var sb strings.Builder

	sb.WriteString(systemPrompt)
	sb.WriteString("\n\n---\n\n")
	fmt.Fprintf(&sb, "**Current Time**: %s\n\n", now.Format(time.RFC1123))

	if len(skillCatalog) > 0 {
		writeSkillCatalog(&sb, skillCatalog)
	}

	if len(subagentCatalog) > 0 {
		writeSubagentCatalog(&sb, subagentCatalog)
	}

	if len(memories) > 0 {
		sb.WriteString("## Recent Memories\n\n")
		for _, m := range memories {
			fmt.Fprintf(&sb, "### %s\n", m.Path)
			content := m.Content
			total := len(content)
			if memoryCharLimit > 0 && total > memoryCharLimit {
				content = content[:memoryCharLimit]
				sb.WriteString(content)
				fmt.Fprintf(&sb,
					"\n\n*[truncated: showing first %d of %d chars; call `memory_read` with path=%q to read the full file, or with offset=%d (line-based) to continue from where this preview ends]*",
					memoryCharLimit, total, m.Path, lineCountFor(content)+1)
			} else {
				sb.WriteString(content)
			}
			sb.WriteString("\n\n")
		}
	}

	return sb.String()
}

// writeSubagentCatalog renders the subagent profile disclosure
// section: a brief explanation of the delegation tools followed by a
// bulleted name/purpose list. The model uses this to pick a profile
// when calling subagent_run / subagent_spawn.
func writeSubagentCatalog(sb *strings.Builder, catalog []subagent.Catalog) {
	sb.WriteString("## Available Subagent Profiles\n\n")
	sb.WriteString("You can delegate isolated work to a subagent via `subagent_run` ")
	sb.WriteString("(synchronous; returns the report inline) or `subagent_spawn` ")
	sb.WriteString("(asynchronous; the report arrives in your context like an operator message). ")
	sb.WriteString("Use `subagent_wait` to block on a previously-spawned subagent, ")
	sb.WriteString("`subagent_status` to list active spawns, and `subagent_cancel` to abort one. ")
	sb.WriteString("The subagent has the same tools you do (minus sleep, update_*, and nested subagent_*) ")
	sb.WriteString("but cannot see your conversation -- you must put everything it needs in the `prompt`.\n\n")
	for _, p := range catalog {
		purpose := strings.TrimSpace(p.Purpose)
		if purpose == "" {
			purpose = "(no purpose configured)"
		}
		fmt.Fprintf(sb, "- **%s**: %s\n", p.Name, purpose)
	}
	sb.WriteString("\n")
}

// writeSkillCatalog renders the tier-1 skill disclosure section:
// behavioral instructions plus a bulleted name/description list.
// Format follows the agentskills.io implementation guide: the catalog
// itself is small (~100 tok per skill), full instructions only load
// when the model calls skill_activate.
func writeSkillCatalog(sb *strings.Builder, catalog []skills.Skill) {
	sb.WriteString("## Available Skills\n\n")
	sb.WriteString("The following skills provide specialized instructions for specific tasks. ")
	sb.WriteString("When a task matches a skill's description, call the `skill_activate` tool ")
	sb.WriteString("with the skill's name to load its full instructions. ")
	sb.WriteString("Skills can also expose bundled scripts and references that are loaded ")
	sb.WriteString("on demand via `skill_read`, and execute scripts in an isolated sandbox via `skill_execute`.\n\n")
	for _, s := range catalog {
		fmt.Fprintf(sb, "- **%s**: %s\n", s.Name, s.Description)
	}
	sb.WriteString("\n")
}

// lineCountFor returns the number of newline-delimited lines in s. A
// trailing newline does not add an extra line. Used to build retrieval
// hints that tell the agent where to resume reading after a truncated
// preview.
func lineCountFor(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
