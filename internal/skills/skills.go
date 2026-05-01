// Package skills holds the domain types for Agent Skills support
// (https://agentskills.io). A skill is a directory containing a SKILL.md
// file with YAML frontmatter (required: name, description; optional:
// license, compatibility, metadata, allowed-tools) and a markdown body
// of free-form instructions.
//
// This package owns the domain types and validation rules. Filesystem
// discovery, frontmatter parsing, and resource enumeration live in the
// adapter (internal/adapters/skills/fs).
package skills

import "fmt"

// Skill is the domain representation of one Agent Skill, parsed from a
// SKILL.md file. Description and Body are required for the skill to be
// usable; the rest of the fields are optional metadata.
type Skill struct {
	// Name is the skill identifier. Must match the parent directory.
	// Lenient validation: warnings are recorded in Diagnostics but the
	// skill is still loaded.
	Name string

	// Description is the catalog blurb the agent sees at startup.
	// Mandatory: skills with no description are skipped at discovery.
	Description string

	// Optional metadata fields from the spec.
	License       string
	Compatibility string
	Metadata      map[string]string
	AllowedTools  string // experimental per the spec; preserved but not enforced

	// Body is the markdown content following the closing frontmatter
	// delimiter, trimmed. This is what skill_activate returns.
	Body string

	// Dir is the absolute path to the skill's directory (the parent of
	// SKILL.md). Used to resolve relative resource paths.
	Dir string

	// SkillMDPath is the absolute path to the skill's SKILL.md.
	SkillMDPath string

	// Diagnostics is a list of human-readable warnings raised during
	// parsing. Populated when the skill is loaded leniently (e.g.
	// name doesn't match directory, name has invalid characters but
	// the skill is otherwise usable). Empty for clean loads.
	Diagnostics []string
}

// Catalog is a small projection of a Skill suitable for the system
// prompt's tier-1 disclosure. Just name + description.
type Catalog struct {
	Name        string
	Description string
}

// Catalog returns the tier-1 projection of this skill.
func (s Skill) ToCatalog() Catalog {
	return Catalog{Name: s.Name, Description: s.Description}
}

// MaxNameLen, MaxDescriptionLen, and MaxCompatLen are the spec's caps.
// Names exceeding MaxNameLen and descriptions exceeding MaxDescriptionLen
// are flagged as errors; the lenient loader may downgrade them to
// diagnostics. Compatibility's cap is informational.
const (
	MaxNameLen        = 64
	MaxDescriptionLen = 1024
	MaxCompatLen      = 500
)

// ValidateName checks the spec rules for the name field:
//   - non-empty, ≤ MaxNameLen
//   - only lowercase ASCII letters, digits, and hyphens
//   - hyphens never appear at the start or end
//   - no two consecutive hyphens
//
// Returns nil for spec-clean names. Callers using lenient validation
// should treat a non-nil error as a warning and load the skill anyway,
// recording the message in Skill.Diagnostics.
//
// Implemented procedurally rather than as a regex because Go's RE2
// engine doesn't support the lookahead a one-line pattern would want
// for the consecutive-hyphen rule.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("name %q exceeds %d characters", name, MaxNameLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			// allowed
		case c == '-':
			if i == 0 || i == len(name)-1 {
				return fmt.Errorf("name %q: hyphens not allowed at start or end", name)
			}
			if name[i-1] == '-' {
				return fmt.Errorf("name %q: consecutive hyphens not allowed", name)
			}
		default:
			return fmt.Errorf("name %q: only lowercase letters, digits, and hyphens allowed (got %q at index %d)", name, c, i)
		}
	}
	return nil
}

// ValidateDescription enforces the spec rule that description must be
// non-empty and not exceed MaxDescriptionLen. Empty is a hard error
// (the catalog needs the description); over-length is also returned as
// an error so the caller can decide whether to warn or skip.
func ValidateDescription(desc string) error {
	switch {
	case desc == "":
		return fmt.Errorf("description is empty")
	case len(desc) > MaxDescriptionLen:
		return fmt.Errorf("description exceeds %d characters", MaxDescriptionLen)
	}
	return nil
}
