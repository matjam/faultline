package prompts

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Prompt migrations: shipped instructions for updating mutable prompt
// files in place on existing deployments.
//
// Why this exists. The default prompt seeds (system.md, cycle-start.md,
// etc.) are written to the agent's memory store on first run. Once
// written they are owned by the agent — it can edit them at will, and
// upgrades that change the embedded defaults do not propagate to
// existing deployments. When a new release adds important behavioral
// guidance to system.md (for example, the untrusted-content
// convention), we cannot ship it as a template change alone; existing
// operators would need to manually port the change or wipe their
// prompts file.
//
// Migrations are the delivery mechanism. Each migration is a markdown
// file shipped in this package with a numeric prefix
// (`000_short_slug.md`, `001_*.md`, ...). At startup the agent runs a
// short bounded loop per pending migration: the body is injected as a
// user message, the LLM applies the change via its existing
// `memory_*` tools, and the runtime records the application in
// `prompts/migrations.md`. Already-applied migrations are skipped.
//
// Idempotency is the migration body's responsibility: every migration
// must include a "check first" step so re-running it (manually, or
// after a half-failed application) is safe. The body sees the same
// tool surface as the agent's normal loop.
//
// Records, not watermarks. We track the set of applied IDs rather
// than a single high-water mark. That keeps the tracking file
// human-readable and lets an operator manually re-trigger a single
// migration by deleting its line.

// migrationsLogPath is the memory-store path where applied migrations
// are recorded. Lives under prompts/ so it sits alongside the files
// migrations actually modify; isOperationalFile in the agent loop
// already filters everything under prompts/ out of the recent-memory
// catalog so this does not pollute system-prompt context.
const migrationsLogPath = "prompts/migrations.md"

// migrationsLogHeader is written when the log file does not yet exist.
// Kept short and self-explanatory so an operator opening the file
// understands what they are looking at.
const migrationsLogHeader = `# Prompt migrations applied

This file records which prompt migrations have been applied to this
deployment. Migrations are one-time instructions shipped with a
faultline release that update the agent's mutable prompt files in
place. The agent applies each migration once and the runtime records
the application here.

Re-running a migration that did not fully apply is safe (every
migration is required to be idempotent). Deleting a record below will
cause that migration to re-run on next startup.

## Applied

`

// appliedLineRe matches an applied-migration record line under
// "## Applied". Format: `- NNN slug rfc3339-timestamp [optional note...]`.
// Slug is the kebab/underscore identifier from the migration filename;
// timestamp is when the runtime recorded the application; trailing
// content is an optional free-form note (e.g. "error: turn cap").
var appliedLineRe = regexp.MustCompile(`^-\s+(\d+)\s+(\S+)\s+(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\S*)(?:\s.*)?$`)

// migrationFileRe matches the embedded migration filename shape:
// digits, underscore, slug, .md.
var migrationFileRe = regexp.MustCompile(`^(\d+)_(.+)\.md$`)

// migrationsFS embeds the shipped migration files. The directory
// glob is `templates/migrations/*` so empty deployments embed an
// empty FS without a build error.
//
//go:embed all:templates/migrations
var migrationsFS embed.FS

// Migration is one shipped prompt-update instruction.
type Migration struct {
	// ID is the numeric prefix of the filename. Must be unique
	// across the set; ordering of application is by ID ascending.
	ID int

	// Slug is the human-readable suffix of the filename, with
	// underscores preserved. Used for the applied-log record and
	// for human inspection; not load-bearing for application
	// ordering or idempotency.
	Slug string

	// Body is the markdown content of the migration file. Injected
	// verbatim as a user-role message during application.
	Body string
}

// LoadMigrations returns all embedded prompt migrations sorted by ID
// ascending. Filenames that do not match the migration shape are
// ignored (so the directory can carry a README without confusing the
// loader).
//
// Returns an error if two migrations share an ID — that is a build-
// time mistake we want to surface loudly rather than silently apply
// one and skip the other.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "templates/migrations")
	if err != nil {
		// An empty embed (no migrations shipped) returns
		// fs.ErrNotExist on some Go versions and a successful
		// empty list on others. Treat both as "no migrations".
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	seen := make(map[int]string, len(entries))
	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		m := migrationFileRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		id, err := strconv.Atoi(m[1])
		if err != nil {
			// Cannot happen because the regex matched \d+, but
			// belt-and-braces.
			return nil, fmt.Errorf("migration %q: bad id: %w", name, err)
		}
		if existing, dup := seen[id]; dup {
			return nil, fmt.Errorf("migration id %d appears in both %q and %q", id, existing, name)
		}
		body, err := fs.ReadFile(migrationsFS, "templates/migrations/"+name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		seen[id] = name
		out = append(out, Migration{
			ID:   id,
			Slug: m[2],
			Body: string(body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// LoadAppliedMigrations returns the set of migration IDs already
// recorded in prompts/migrations.md. Missing file -> empty set with
// no error: a fresh deployment has nothing applied yet, which is the
// expected state.
//
// Parses the file leniently: any line under "## Applied" matching the
// `- ID slug timestamp` shape contributes; anything else is ignored.
// Operators editing the file by hand to remove a record will not be
// punished for whitespace.
func LoadAppliedMigrations(store Store) (map[int]struct{}, error) {
	content, err := store.Read(migrationsLogPath)
	if err != nil || content == "" {
		// Read errors include "file does not exist" for the fs
		// adapter; treat both shapes as "nothing applied yet".
		// A real read error (permission, disk) will resurface on
		// the subsequent Write so we do not lose the diagnostic.
		return map[int]struct{}{}, nil
	}

	applied := make(map[int]struct{})
	inApplied := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			inApplied = strings.EqualFold(trimmed, "## Applied")
			continue
		}
		if !inApplied {
			continue
		}
		m := appliedLineRe.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		id, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		applied[id] = struct{}{}
	}
	return applied, nil
}

// PendingMigrations returns the migrations from `all` whose IDs are
// not in `applied`, preserving the input ordering (which LoadMigrations
// has already sorted ascending).
func PendingMigrations(all []Migration, applied map[int]struct{}) []Migration {
	out := make([]Migration, 0, len(all))
	for _, m := range all {
		if _, ok := applied[m.ID]; ok {
			continue
		}
		out = append(out, m)
	}
	return out
}

// RecordMigrationApplied appends a record line to the applied-log
// file. Creates the file with the canonical header on first call.
//
// `note` is an optional short suffix appended after the timestamp;
// used to flag partial applications ("error: turn cap reached") so an
// operator can spot and re-run them. Empty note produces a clean
// record line.
//
// The file is owned by the agent at runtime — it can edit, list,
// search, or delete it through normal memory tools. The runtime's
// guarantee is that it will not re-apply any migration whose ID is
// recorded under "## Applied" at startup.
func RecordMigrationApplied(store Store, m Migration, when time.Time, note string) error {
	current, _ := store.Read(migrationsLogPath)

	if current == "" {
		current = migrationsLogHeader
	}

	// If the file exists but lacks the "## Applied" section header,
	// add one before our record. Operators who hand-create the file
	// without the header should still get correctly-recorded
	// migrations.
	if !strings.Contains(current, "## Applied") {
		if !strings.HasSuffix(current, "\n") {
			current += "\n"
		}
		current += "\n## Applied\n\n"
	}

	// Build the record line.
	stamp := when.UTC().Format(time.RFC3339)
	line := fmt.Sprintf("- %03d %s %s", m.ID, m.Slug, stamp)
	if note != "" {
		line += " " + note
	}
	line += "\n"

	// Append after the file's existing content. We do not insert
	// directly under "## Applied" to avoid clobbering operator
	// commentary; record lines accumulate at the end of the file in
	// chronological order, which matches the file's purpose.
	if !strings.HasSuffix(current, "\n") {
		current += "\n"
	}
	current += line

	return store.Write(migrationsLogPath, current)
}
