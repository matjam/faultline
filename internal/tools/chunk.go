package tools

import (
	"fmt"
	"strings"
)

// maxParagraphBytes is the hard cap on a single embedding unit (one
// paragraph). Paragraphs typically run well under this; the cap exists
// only as a fallback for documents containing one giant paragraph
// (e.g. a single-line file, an unbroken code block, a long bullet
// list with no blank lines between items). Sized at ~750 tokens, well
// under embeddinggemma's 2048-token context and any other reasonable
// embedding model.
//
// Files exceeding this cap on a single paragraph are byte-cut into
// pieces of this size; this loses paragraph-clean boundaries for that
// fragment but keeps the entire file indexable rather than dropping
// it.
const maxParagraphBytes = 3000

// splitIntoUnits splits a memory-file body into paragraph-aligned
// embedding units.
//
// Splitting strategy:
//   - Paragraphs are separated by one or more blank lines (matching
//     `\n\n+` after CRLF normalisation).
//   - Each non-empty paragraph is one unit.
//   - A paragraph longer than maxParagraphBytes is byte-cut into
//     ceil(len/maxParagraphBytes) successive pieces. This is rare in
//     practice; markdown memory files almost always have natural
//     paragraph boundaries well under 3000 bytes.
//
// Whitespace handling:
//   - Trailing whitespace on the input is stripped before splitting.
//   - Leading/trailing whitespace within each paragraph is preserved
//     (markdown indentation can be semantically meaningful).
//   - Paragraphs that are entirely whitespace after split are dropped.
//
// Returns nil for empty or whitespace-only input. The returned slice
// is in document order; callers (the indexer) use the slice index as
// the chunk number for keys like `path#0`, `path#1`, ...
func splitIntoUnits(body string) []string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if strings.TrimSpace(body) == "" {
		return nil
	}

	// Split on runs of one or more blank lines. A blank line is a
	// newline followed by optional whitespace and another newline.
	// Using a manual scanner is simpler than a regexp and avoids the
	// regexp import.
	//
	// We deliberately do NOT TrimSpace the body before splitting:
	// markdown indentation on the first paragraph (e.g. "  - bullet")
	// is semantically meaningful and must be preserved. Leading
	// blank-line runs at the top of the body produce empty paragraphs
	// from splitOnBlankLines, which the filter loop below drops.
	paragraphs := splitOnBlankLines(body)

	var units []string
	for _, p := range paragraphs {
		p = strings.TrimRight(p, " \t\n")
		if strings.TrimSpace(p) == "" {
			continue
		}
		if len(p) <= maxParagraphBytes {
			units = append(units, p)
			continue
		}
		// Oversized paragraph -> byte-cut. We could prefer to split
		// at a sentence boundary inside the paragraph, but the
		// extra logic isn't justified for the rare case this
		// triggers; an oversized paragraph is by definition unusual
		// content (single-line file, code block, etc).
		for i := 0; i < len(p); i += maxParagraphBytes {
			end := i + maxParagraphBytes
			if end > len(p) {
				end = len(p)
			}
			units = append(units, p[i:end])
		}
	}
	return units
}

// splitOnBlankLines partitions s into paragraph strings divided by
// blank-line separators. A blank-line separator is two consecutive
// newlines, optionally with whitespace between them. Adjacent blank
// lines collapse into one separator.
func splitOnBlankLines(s string) []string {
	var out []string
	var start int
	i := 0
	for i < len(s) {
		// Look for "\n" + optional whitespace + "\n" -> separator.
		if s[i] == '\n' {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			if j < len(s) && s[j] == '\n' {
				// Found a blank-line separator. Emit the
				// paragraph that ends at i (exclusive).
				if i > start {
					out = append(out, s[start:i])
				}
				// Skip past consecutive blank-line runs.
				k := j + 1
				for k < len(s) {
					if s[k] == '\n' || s[k] == ' ' || s[k] == '\t' {
						k++
						continue
					}
					break
				}
				start = k
				i = k
				continue
			}
		}
		i++
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// unitKey returns the index key for the n-th unit of a memory file.
// For single-unit files (the common case), the bare path is returned
// so the index keying matches the legacy one-vector-per-file shape.
// For multi-unit files, "path#N" is used; the # separator is
// unambiguous because the memory path validator restricts segments
// to [a-z0-9.-].
func unitKey(path string, n, total int) string {
	if total <= 1 {
		return path
	}
	return fmt.Sprintf("%s#%d", path, n)
}

// pathFromUnitKey returns the underlying memory path for a unit
// index key, stripping any "#N" suffix. Used by the memory_search
// post-processor to dedupe paragraph hits down to the file level.
func pathFromUnitKey(key string) string {
	if i := strings.LastIndexByte(key, '#'); i > 0 {
		// Confirm the suffix after # is all digits; if not, the #
		// is part of the path itself (shouldn't happen given the
		// memory path validator, but be defensive).
		suffix := key[i+1:]
		if suffix != "" && allDigits(suffix) {
			return key[:i]
		}
	}
	return key
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
