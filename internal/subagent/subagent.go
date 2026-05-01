// Package subagent holds the domain types for subagent delegation.
//
// A subagent is a child agent loop spawned by the primary agent to
// perform isolated work. The primary chooses which "profile" the child
// runs under (LLM endpoint, key, model, sampler overrides) and supplies
// all relevant context as a free-form prompt; the child has no access
// to the primary's conversation log. The child runs the same agent
// hexagon as the primary -- with the same tool surface (modulo a small
// set of restrictions, see internal/tools) -- and terminates by
// calling the subagent_report tool, whose payload is returned to the
// primary.
//
// This package owns the domain types: Profile, Report, the SpawnFunc
// signature that bridges domain to composition, and the Manager that
// owns the active subagent registry and the report inbox the primary
// agent drains alongside operator messages.
//
// Construction of the actual child agent.Agent + tools.Executor +
// llm.Chat client happens in cmd/faultline/main.go via a SpawnFunc
// closure; this package never imports the agent or tools packages.
package subagent

import (
	"fmt"
	"time"
)

// Profile describes one subagent execution profile. Profiles are
// named, advertised to the primary agent in its system prompt, and
// selected by name when the primary calls subagent_run / subagent_spawn.
//
// The "default" profile name is reserved: it is synthesized at runtime
// from the primary agent's own [api] / [agent] config and means "run
// the subagent against the same backend the primary uses." Operator
// profiles must use a different name.
type Profile struct {
	// Name is the profile identifier. Lowercase ASCII letters,
	// digits, and hyphens; 1-32 chars; no leading/trailing hyphens
	// and no consecutive hyphens. Cannot be "default".
	Name string

	// APIURL is the OpenAI-compatible chat-completions base URL
	// (ending in /v1, no trailing slash). Required for operator
	// profiles; the synthesized "default" profile inherits from
	// the primary.
	APIURL string

	// APIKey is sent as a Bearer token. May be empty for local
	// servers that don't authenticate.
	APIKey string

	// Model is the model identifier for the chat-completions call.
	// Required.
	Model string

	// Purpose is operator-supplied free-form text that explains
	// WHEN the primary agent should pick this profile. Rendered in
	// the primary's system prompt next to the profile name. Empty
	// is allowed but discouraged -- without it, the primary has to
	// guess from the model name.
	Purpose string

	// Sampler overrides. Zero means "inherit from primary's [agent]".
	// Matches the existing zero-value-omitted convention used by
	// AgentConfig. (If a profile genuinely needs Temperature == 0,
	// use 0.0001 -- functionally equivalent for our backends.)
	Temperature       float32
	TopP              float32
	TopK              int
	MinP              float32
	RepetitionPenalty float32
	MaxRespTokens     int
}

// Catalog is the projection of a Profile suitable for inclusion in
// the primary agent's system prompt. The agent only needs to see the
// name and purpose to make a routing decision; URL/key/model are
// implementation detail.
type Catalog struct {
	Name    string
	Purpose string
}

// ToCatalog returns the system-prompt projection of this profile.
func (p Profile) ToCatalog() Catalog {
	return Catalog{Name: p.Name, Purpose: p.Purpose}
}

// DefaultProfileName is the reserved profile name that the primary
// constructs at runtime from its own [api] / [agent] configuration.
// Operator-supplied profiles must use a different name.
const DefaultProfileName = "default"

// MaxNameLen is the upper bound on profile name length. Mirrors the
// skill name cap so log lines stay readable.
const MaxNameLen = 32

// MaxPurposeLen caps the operator-supplied purpose string. Too long
// and it dwarfs the system prompt; this is a soft cap that produces
// a load error.
const MaxPurposeLen = 512

// ValidateName enforces the profile-name spelling rules. See
// Profile.Name for the rules. Implemented procedurally because Go's
// RE2 engine doesn't support the lookahead a one-line pattern would
// want for the consecutive-hyphen rule.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	if name == DefaultProfileName {
		return fmt.Errorf("name %q is reserved", DefaultProfileName)
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

// ValidateProfile checks all spec rules for a profile loaded from
// operator config. Returns the first error found; callers should
// log it and skip the profile (loud failure on bad config rather
// than silently advertising broken profiles to the agent).
func ValidateProfile(p Profile) error {
	if err := ValidateName(p.Name); err != nil {
		return fmt.Errorf("profile name: %w", err)
	}
	if p.APIURL == "" {
		return fmt.Errorf("profile %q: api_url is required", p.Name)
	}
	if p.Model == "" {
		return fmt.Errorf("profile %q: model is required", p.Name)
	}
	if len(p.Purpose) > MaxPurposeLen {
		return fmt.Errorf("profile %q: purpose exceeds %d characters", p.Name, MaxPurposeLen)
	}
	return nil
}

// Report is what a subagent returns to the primary when its loop
// terminates. The Text field is the free-form summary the child
// produced via the subagent_report tool. Truncated is set when the
// child hit MaxTurnsPerRun or RunTimeout before reporting; in that
// case Text is whatever last assistant message the child emitted (or
// empty if it never produced one). Canceled is set when the run was
// aborted by the primary via subagent_cancel or by parent shutdown.
type Report struct {
	WorkID     string
	Profile    string
	Text       string
	Err        error
	Canceled   bool
	Truncated  bool
	StartedAt  time.Time
	FinishedAt time.Time
}

// ActiveStatus is the projection of a running subagent surfaced by
// the subagent_status tool. Just the bookkeeping the primary cares
// about; the conversation log is private to the child.
type ActiveStatus struct {
	WorkID  string
	Profile string
	Prompt  string // truncated for display
	Started time.Time
	Async   bool
}
