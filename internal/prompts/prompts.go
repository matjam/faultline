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
)

// Embedded default prompt contents, compiled into the binary from templates/.
var (
	//go:embed templates/system.md
	defaultSystem string

	//go:embed templates/compaction.md
	defaultCompaction string

	//go:embed templates/cycle_start.md
	defaultCycleStart string

	//go:embed templates/continue.md
	defaultContinue string

	//go:embed templates/shutdown.md
	defaultShutdown string
)

// Store is the persistence backend used to read and seed prompt files.
// The agent's MemoryStore satisfies this interface structurally.
type Store interface {
	Read(path string) (string, error)
	Write(path, content string) error
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
		"cycle_start": {
			path:         "prompts/cycle_start.md",
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

// BuildCycleContext assembles the full system message with recent memories.
// memoryCharLimit caps the per-entry content size; when exceeded, a
// retrieval hint is appended pointing the agent at memory_read so it can
// load the rest of the file. A non-positive limit disables the cap.
func BuildCycleContext(systemPrompt string, memories []bm25.Result, now time.Time, memoryCharLimit int) string {
	var sb strings.Builder

	sb.WriteString(systemPrompt)
	sb.WriteString("\n\n---\n\n")
	fmt.Fprintf(&sb, "**Current Time**: %s\n\n", now.Format(time.RFC1123))

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
