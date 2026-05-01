package fs

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/matjam/faultline/internal/skills"
)

// ErrNotFound is returned by Get/Read when the requested skill does
// not exist in the catalog.
var ErrNotFound = errors.New("skill not found")

// ErrPathEscape is returned by Read when the requested resource path
// resolves outside the skill's directory (path traversal attempt).
var ErrPathEscape = errors.New("resource path escapes skill directory")

// resourceDirs are the optional subdirectories the spec defines.
// Anything outside these is hidden from the bundled-resources listing
// at activation time, but still readable via Read for skills with
// non-conventional layouts.
var resourceDirs = []string{"scripts", "references", "assets"}

// MaxResourceListing caps how many resources are surfaced in a single
// activation result. A pathologically large skill won't blow up the
// activation message; the caller appends a "[truncated]" note when
// this kicks in.
const MaxResourceListing = 50

// Store is the filesystem-backed implementation of the agent's Skills
// port. It owns an in-memory catalog rebuilt by Reload (called at
// startup and on context rebuild). All methods are safe for concurrent
// use; the agent's Reload-from-rebuild path serializes naturally with
// the loop, but tools may call List/Get/Read mid-turn.
type Store struct {
	root   string
	logger *slog.Logger

	mu      sync.RWMutex
	catalog map[string]*skills.Skill
	order   []string // sorted skill names for deterministic List output

	// Operator-controlled enable/disable state, loaded from a
	// TOML file by LoadDisabledFromFile. Disabled skills disappear
	// from List/Get (the agent never sees them) but remain
	// visible via ListAll for the admin UI's toggle page.
	stateFile string
	disabled  map[string]bool
}

// New constructs a Store rooted at dir. Reload is called once
// synchronously so the catalog is populated before the constructor
// returns. A missing root directory is not a fatal error -- the
// catalog stays empty and the operator can drop skills in later, with
// a Reload at the next context rebuild picking them up.
func New(dir string, logger *slog.Logger) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve skills root: %w", err)
	}
	s := &Store{
		root:   abs,
		logger: logger,
	}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Root returns the absolute path of the skills root directory.
func (s *Store) Root() string {
	return s.root
}

// Reload rescans the skills root directory and rebuilds the in-memory
// catalog. Called at startup and again on every context rebuild so
// operator-dropped skills appear without a process restart.
//
// Reload is best-effort: per-skill parse errors are logged and the
// problem skill is dropped from the catalog, but the operation as a
// whole only fails on filesystem-level issues (e.g. unreadable root).
// A missing root is treated as "no skills" rather than an error.
func (s *Store) Reload() error {
	info, err := os.Stat(s.root)
	if errors.Is(err, os.ErrNotExist) {
		// No skills root yet -- equivalent to an empty catalog. Operator
		// can mkdir it later; next Reload will pick up its contents.
		s.mu.Lock()
		s.catalog = nil
		s.order = nil
		s.mu.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat skills root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("skills root %q is not a directory", s.root)
	}

	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("read skills root: %w", err)
	}

	catalog := make(map[string]*skills.Skill)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip dotfiles and the conventional "exclude" patterns. A
		// .git directory at the skills root happens when the operator
		// keeps their skills under git.
		if strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(s.root, name)
		mdPath := filepath.Join(dir, "SKILL.md")
		raw, err := os.ReadFile(mdPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Subdir with no SKILL.md isn't a skill; ignore quietly.
				continue
			}
			s.logger.Warn("skills: SKILL.md unreadable; skipping",
				slog.String("name", name),
				slog.String("err", err.Error()))
			continue
		}

		sk, err := loadSkill(raw, dir, mdPath, name)
		if err != nil {
			s.logger.Warn("skills: skipping malformed skill",
				slog.String("name", name),
				slog.String("err", err.Error()))
			continue
		}

		// Per the spec: warn on collisions but be deterministic. Within
		// a single root we shouldn't hit collisions because directory
		// names must be unique on the filesystem; this is here in case
		// loadSkill ever rewrites the name (currently it doesn't).
		if existing, exists := catalog[sk.Name]; exists {
			s.logger.Warn("skills: name collision; keeping first found",
				slog.String("name", sk.Name),
				slog.String("kept", existing.Dir),
				slog.String("dropped", sk.Dir))
			continue
		}
		catalog[sk.Name] = sk

		for _, d := range sk.Diagnostics {
			s.logger.Warn("skills: diagnostic during load",
				slog.String("name", sk.Name),
				slog.String("msg", d))
		}
	}

	order := make([]string, 0, len(catalog))
	for name := range catalog {
		order = append(order, name)
	}
	sort.Strings(order)

	s.mu.Lock()
	s.catalog = catalog
	s.order = order
	s.mu.Unlock()

	s.logger.Info("skills: catalog loaded",
		slog.Int("count", len(catalog)),
		slog.String("root", s.root))
	return nil
}

// loadSkill parses a single SKILL.md and returns a populated Skill.
// Returns an error only when the skill must be skipped entirely
// (no frontmatter, unparseable YAML, or missing description). Lenient
// issues (name mismatch, over-length description, etc.) are recorded
// in skill.Diagnostics and the skill is still returned.
func loadSkill(raw []byte, dir, mdPath, dirName string) (*skills.Skill, error) {
	fm, body, ok := splitFrontmatter(raw)
	if !ok {
		return nil, errors.New("missing YAML frontmatter")
	}
	parsed, err := parseFrontmatter(fm)
	if err != nil {
		return nil, err
	}

	sk := &skills.Skill{
		Body:        body,
		Dir:         dir,
		SkillMDPath: mdPath,
	}
	if err := applyFrontmatter(sk, parsed, dirName); err != nil {
		return nil, err
	}
	return sk, nil
}

// LoadSkillForValidation parses a SKILL.md as if it were being added
// to the catalog and returns the populated Skill. Exposed for the
// install-time pre-flight check in the tools layer so a malformed
// skill is rejected before being moved into the live skills root.
//
// The same rules apply as during normal Reload(): missing frontmatter
// or description is a hard error; lenient issues land in
// skill.Diagnostics rather than failing the load.
func LoadSkillForValidation(raw []byte, dir, mdPath, dirName string) (*skills.Skill, error) {
	return loadSkill(raw, dir, mdPath, dirName)
}

// List returns all skills in the catalog that are not operator-
// disabled, ordered by name. The agent uses this for system-prompt
// catalog injection and the tools layer uses it for skill_activate;
// disabled skills are invisible to both. The returned slice is a
// copy of the underlying records; mutating it does not affect the
// store.
//
// To see every skill regardless of state, use ListAll (admin UI).
func (s *Store) List() []skills.Skill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]skills.Skill, 0, len(s.order))
	for _, name := range s.order {
		if s.disabled[name] {
			continue
		}
		out = append(out, *s.catalog[name])
	}
	return out
}

// Get returns the skill with the given name. Returns ErrNotFound if
// the skill is not in the catalog or is operator-disabled. The same
// error is used for both cases on purpose: the agent shouldn't
// distinguish "skill doesn't exist" from "skill turned off by the
// operator" — either way, it can't run the skill.
func (s *Store) Get(name string) (skills.Skill, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sk, ok := s.catalog[name]
	if !ok || s.disabled[name] {
		return skills.Skill{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return *sk, nil
}

// Read returns the contents of a resource file inside the named
// skill's directory. relPath is interpreted relative to the skill
// directory and validated to ensure it does not escape via "..". Any
// path that resolves outside the skill directory returns
// ErrPathEscape.
func (s *Store) Read(name, relPath string) (string, error) {
	sk, err := s.Get(name)
	if err != nil {
		return "", err
	}
	target, err := resolveResource(sk.Dir, relPath)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", fmt.Errorf("read %s/%s: %w", name, relPath, err)
	}
	return string(data), nil
}

// resolveResource joins skillDir + relPath, then verifies the result
// is still under skillDir. Symlinks are not specially handled -- if a
// skill directory contains a symlink that points outside its dir,
// reads through that symlink will succeed at the OS level. This is
// considered the operator's responsibility (skills are trusted; they
// were dropped into the skills root by the operator).
func resolveResource(skillDir, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("relative path is required")
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("%w: absolute paths not allowed", ErrPathEscape)
	}
	clean := filepath.Clean(filepath.Join(skillDir, relPath))
	rel, err := filepath.Rel(skillDir, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, relPath)
	}
	return clean, nil
}

// Resource is one entry in the bundled-resources listing returned by
// Resources. Path is the skill-relative path; Size is the file size
// in bytes for files (0 for directories). IsDir distinguishes the two
// so the agent knows whether a path is something it can read directly.
type Resource struct {
	Path  string
	Size  int64
	IsDir bool
}

// Resources walks the conventional resource subdirectories
// (scripts/, references/, assets/) of the named skill and returns a
// flat listing of every file underneath them. Hidden files (dotfiles)
// are skipped. The listing is capped at MaxResourceListing entries;
// when the cap is hit, Truncated is true so the caller can append a
// note to the activation message.
//
// This is the tier-2 disclosure: when a skill activates, the agent
// learns what supporting files exist without their content being
// loaded into context.
func (s *Store) Resources(name string) (entries []Resource, truncated bool, err error) {
	sk, err := s.Get(name)
	if err != nil {
		return nil, false, err
	}
	for _, sub := range resourceDirs {
		subPath := filepath.Join(sk.Dir, sub)
		info, statErr := os.Stat(subPath)
		if statErr != nil || !info.IsDir() {
			continue
		}
		walkErr := filepath.WalkDir(subPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			base := d.Name()
			if strings.HasPrefix(base, ".") && path != subPath {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if path == subPath {
				return nil // skip the resource-dir root itself
			}
			rel, relErr := filepath.Rel(sk.Dir, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				entries = append(entries, Resource{Path: rel, IsDir: true})
				return nil
			}
			fi, fiErr := d.Info()
			if fiErr != nil {
				return fiErr
			}
			entries = append(entries, Resource{Path: rel, Size: fi.Size()})
			if len(entries) >= MaxResourceListing {
				truncated = true
				return filepath.SkipAll
			}
			return nil
		})
		if walkErr != nil {
			return entries, truncated, fmt.Errorf("walk %s: %w", sub, walkErr)
		}
		if truncated {
			break
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, truncated, nil
}
