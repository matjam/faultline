package main

import (
	"fmt"
	"strings"
	"time"
)

// BuildCycleContext assembles the full system message with recent memories.
// memoryCharLimit caps the per-entry content size; when exceeded, a
// retrieval hint is appended pointing the agent at memory_read so it can
// load the rest of the file. A non-positive limit disables the cap.
//
// This will move into internal/prompts/ once SearchResult moves out of the
// main package alongside MemoryStore.
func BuildCycleContext(systemPrompt string, memories []SearchResult, now time.Time, memoryCharLimit int) string {
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

// lineCountFor returns the number of newline-delimited lines in s.
// A trailing newline does not add an extra line. Used to build retrieval
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
