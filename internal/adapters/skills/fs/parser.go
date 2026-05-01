// Package fs is the filesystem-backed adapter for the Skills port.
// Skills are stored as <root>/<skill-name>/SKILL.md plus optional
// scripts/, references/, and assets/ subdirectories.
package fs

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/matjam/faultline/internal/skills"
)

// frontmatterDelim is the YAML frontmatter fence used by SKILL.md.
var frontmatterDelim = []byte("---")

// rawFrontmatter is the structural shape we extract from SKILL.md
// before normalising into a domain skills.Skill. Everything is
// deliberately permissive (string or map) so we don't reject otherwise
// valid skills on minor type quirks.
type rawFrontmatter struct {
	Name          string                 `yaml:"name"`
	Description   string                 `yaml:"description"`
	License       string                 `yaml:"license"`
	Compatibility string                 `yaml:"compatibility"`
	Metadata      map[string]interface{} `yaml:"metadata"`
	AllowedTools  string                 `yaml:"allowed-tools"`
}

// splitFrontmatter pulls the YAML frontmatter and body apart. Returns:
//
//   - frontmatter bytes: everything between the opening "---" line and
//     the matching closing "---" line, exclusive.
//   - body string: everything after the closing "---", trimmed.
//
// Returns ok=false when no frontmatter is present (the file doesn't
// start with "---" on its own line). The caller treats that as a hard
// error: a SKILL.md without frontmatter has no name or description and
// can't be loaded.
func splitFrontmatter(content []byte) (frontmatter []byte, body string, ok bool) {
	// Normalise CRLF to LF so the line scanner works regardless of how
	// the file was authored. SKILL.md authored on Windows is common.
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))

	// Strip a leading UTF-8 BOM if present -- some editors prepend one,
	// and a BOM in front of the opening "---" would prevent a match.
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))

	lines := bytes.SplitN(content, []byte("\n"), 2)
	if len(lines) < 2 || !bytes.Equal(bytes.TrimSpace(lines[0]), frontmatterDelim) {
		return nil, "", false
	}

	rest := lines[1]
	closeIdx := indexLine(rest, frontmatterDelim)
	if closeIdx < 0 {
		return nil, "", false
	}

	frontmatter = rest[:closeIdx]
	body = strings.TrimSpace(string(rest[closeIdx+len(frontmatterDelim):]))
	// Strip a leading newline left over from the closing fence.
	body = strings.TrimLeft(body, "\n")
	return frontmatter, body, true
}

// indexLine returns the byte offset of the first line in `data` that
// equals (after trim) `target`, or -1 if not found. Includes the
// trailing newline before the matched line so the caller can slice the
// frontmatter cleanly.
func indexLine(data, target []byte) int {
	pos := 0
	for pos < len(data) {
		nl := bytes.IndexByte(data[pos:], '\n')
		var line []byte
		var lineEnd int
		if nl < 0 {
			line = data[pos:]
			lineEnd = len(data)
		} else {
			line = data[pos : pos+nl]
			lineEnd = pos + nl
		}
		if bytes.Equal(bytes.TrimSpace(line), target) {
			return pos
		}
		if nl < 0 {
			return -1
		}
		pos = lineEnd + 1
	}
	return -1
}

// parseFrontmatter parses a YAML frontmatter block into rawFrontmatter.
// Applies a one-shot lenient retry: if the first parse fails on the
// common "unquoted colon in value" mistake the spec calls out, retry
// with values quoted.
func parseFrontmatter(frontmatter []byte) (rawFrontmatter, error) {
	var raw rawFrontmatter
	if err := yaml.Unmarshal(frontmatter, &raw); err == nil {
		return raw, nil
	} else {
		// Try the lenient retry. If it still fails, surface the original
		// error -- the operator gets the more diagnostic message.
		retried, retryErr := lenientRetry(frontmatter)
		if retryErr == nil {
			if err2 := yaml.Unmarshal(retried, &raw); err2 == nil {
				return raw, nil
			}
		}
		return raw, fmt.Errorf("yaml parse: %w", err)
	}
}

// lenientRetry rewrites the frontmatter to quote any value containing
// an unescaped colon-then-space sequence in a `key: value` line. This
// covers the common authoring mistake the agentskills spec calls out:
//
//	description: Use this skill when: the user asks about PDFs
//
// where the second `:` confuses the YAML parser. We only quote when
// the value is otherwise unquoted (no leading "/' and not starting
// with a YAML control char) and we leave already-valid lines alone.
//
// This is a heuristic, not a full YAML rewriter. It's deliberately
// limited to the single most common compatibility issue.
func lenientRetry(frontmatter []byte) ([]byte, error) {
	lines := bytes.Split(frontmatter, []byte("\n"))
	for i, line := range lines {
		// Only consider top-level scalar `key: value` lines (not
		// indented keys, not list items, not blank, not comments).
		trim := bytes.TrimSpace(line)
		if len(trim) == 0 || trim[0] == '#' || trim[0] == '-' {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			// indented -- part of a map; skip
			continue
		}
		colon := bytes.Index(line, []byte(": "))
		if colon < 0 {
			continue
		}
		key := bytes.TrimSpace(line[:colon])
		value := bytes.TrimSpace(line[colon+2:])
		if len(value) == 0 {
			continue
		}
		// Already quoted or block-scalar -> leave alone.
		first := value[0]
		if first == '"' || first == '\'' || first == '|' || first == '>' || first == '[' || first == '{' {
			continue
		}
		// If the value contains another `:` followed by space, quoting it
		// is the fix. (A trailing `:` is fine in plain YAML; only `: ` mid-
		// value confuses the parser.)
		if bytes.Contains(value, []byte(": ")) {
			// Escape any embedded double quote and wrap.
			escaped := bytes.ReplaceAll(value, []byte(`"`), []byte(`\"`))
			rebuilt := append([]byte{}, key...)
			rebuilt = append(rebuilt, []byte(": \"")...)
			rebuilt = append(rebuilt, escaped...)
			rebuilt = append(rebuilt, '"')
			lines[i] = rebuilt
		}
	}
	return bytes.Join(lines, []byte("\n")), nil
}

// stringMap normalises a yaml.v3 untyped-map value into the
// map[string]string Skill.Metadata wants. yaml.v3 returns interface{}
// for arbitrary values; we stringify each one with fmt.Sprintf. Nested
// maps/slices are flattened to their Go default formatting -- the
// skill author's responsibility to keep metadata simple.
func stringMap(in map[string]interface{}) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}

// applyFrontmatter populates a Skill from a parsed rawFrontmatter
// block. dirName is the parent directory's basename, used to detect
// name mismatches under lenient validation.
//
// Returns nil if the skill is loadable (potentially with diagnostics);
// returns an error only when the skill must be skipped entirely
// (description missing).
func applyFrontmatter(s *skills.Skill, raw rawFrontmatter, dirName string) error {
	// Name: the spec says it must match the directory. Lenient: if the
	// frontmatter name is missing, fall back to the directory name; if
	// it disagrees, prefer the directory name so the catalog key stays
	// stable, and record a diagnostic.
	name := raw.Name
	if name == "" {
		name = dirName
		s.Diagnostics = append(s.Diagnostics, "frontmatter has no `name` field; using directory name")
	} else if name != dirName {
		s.Diagnostics = append(s.Diagnostics, fmt.Sprintf("frontmatter name %q does not match directory %q; using directory name", name, dirName))
		name = dirName
	}
	if err := skills.ValidateName(name); err != nil {
		s.Diagnostics = append(s.Diagnostics, "name is not spec-clean: "+err.Error())
	}
	s.Name = name

	// Description: hard error if missing or empty -- nothing to put in
	// the catalog. Over-length is a warning.
	desc := strings.TrimSpace(raw.Description)
	if desc == "" {
		return fmt.Errorf("description is missing or empty")
	}
	if err := skills.ValidateDescription(desc); err != nil {
		s.Diagnostics = append(s.Diagnostics, err.Error())
	}
	s.Description = desc

	s.License = strings.TrimSpace(raw.License)
	s.Compatibility = strings.TrimSpace(raw.Compatibility)
	if s.Compatibility != "" && len(s.Compatibility) > skills.MaxCompatLen {
		s.Diagnostics = append(s.Diagnostics, fmt.Sprintf("compatibility exceeds %d characters", skills.MaxCompatLen))
	}
	s.AllowedTools = strings.TrimSpace(raw.AllowedTools)
	s.Metadata = stringMap(raw.Metadata)

	return nil
}
