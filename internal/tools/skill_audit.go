package tools

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/subagent"
)

// skillAuditPromptTemplate is the embedded audit instructions. The
// `.md` file lives next to this source file so it ships with the
// binary; the agent cannot edit it via the memory store. Substitution
// placeholders are {{NAME}}, {{SOURCE}}, {{DESCRIPTION}}, {{MANIFEST}}.
//
//go:embed skill_audit.md
var skillAuditPromptTemplate string

const (
	// auditPerFileMaxBytes caps the inlined content of a single file.
	// Files larger than this are inlined up to the cap with a
	// truncation marker and their full sha256 so the auditor can
	// still recognize them.
	auditPerFileMaxBytes = 50 * 1024

	// auditTotalMaxBytes caps the total inlined content across all
	// files. Once the cap is reached, remaining files are listed by
	// path/size/sha256 only. Keeps the audit prompt small enough to
	// fit comfortably in any reasonable context window even when the
	// skill is enormous.
	auditTotalMaxBytes = 200 * 1024

	// auditBinaryProbeBytes is how many bytes from the start of the
	// file we scan for a NUL byte to classify as binary. Files with a
	// NUL in the first 8 KiB are not inlined.
	auditBinaryProbeBytes = 8 * 1024

	// skillAuditTimeout is the wall-clock budget for one audit run.
	// Generous because audits include web searches over slow links.
	// Independent from skillInstallTimeout so download/extract isn't
	// extended unnecessarily.
	skillAuditTimeout = 15 * time.Minute
)

// auditVerdict is the parsed result of an audit subagent run.
type auditVerdict struct {
	Approved  bool
	Skipped   bool   // true when audit could not run (e.g. no manager)
	Summary   string // first line, post-prefix
	Rationale string // everything after the first line
	Raw       string // unparsed report text, for logging
}

// auditSkill runs the audit subagent against the extracted skill
// directory and returns a verdict. Callers MUST treat anything other
// than Approved=true (or Skipped=true with a clear notice) as a
// refusal.
//
// When the subagent.Manager is nil, audit is skipped with a notice;
// the install proceeds. Operators who turned on skill_install but
// not [subagent] explicitly opted out of audit. This is a deliberate
// fail-open exception; every other failure mode is fail-closed.
func (te *Executor) auditSkill(ctx context.Context, name, source, description, dir string) auditVerdict {
	if te.subagentMgr == nil {
		te.logger.Warn("skill audit skipped: subagent support is not enabled; install proceeds without security review",
			"skill", name, "source", source)
		return auditVerdict{
			Approved: true,
			Skipped:  true,
			Summary:  "audit skipped: subagent support is not enabled",
		}
	}

	manifest, manifestErr := buildAuditManifest(dir)
	if manifestErr != nil {
		return auditVerdict{
			Approved:  false,
			Summary:   "audit failed: could not build skill manifest",
			Rationale: manifestErr.Error(),
		}
	}

	prompt := strings.NewReplacer(
		"{{NAME}}", name,
		"{{SOURCE}}", source,
		"{{DESCRIPTION}}", description,
		"{{MANIFEST}}", manifest,
	).Replace(skillAuditPromptTemplate)

	auditCtx, cancel := context.WithTimeout(ctx, skillAuditTimeout)
	defer cancel()

	te.logger.Info("skill audit starting",
		"skill", name,
		"source", source,
		"manifest_bytes", len(manifest),
		"prompt_bytes", len(prompt),
	)

	report, err := te.subagentMgr.Run(auditCtx, subagent.DefaultProfileName, prompt)
	if err != nil {
		return auditVerdict{
			Approved:  false,
			Summary:   "audit failed: subagent could not run",
			Rationale: err.Error(),
		}
	}
	if report.Truncated {
		return auditVerdict{
			Approved:  false,
			Summary:   "audit failed: subagent hit turn or time cap before reaching a verdict",
			Rationale: report.Text,
			Raw:       report.Text,
		}
	}
	if report.Canceled {
		return auditVerdict{
			Approved:  false,
			Summary:   "audit failed: subagent was canceled before reporting",
			Rationale: report.Text,
			Raw:       report.Text,
		}
	}
	if report.Err != nil {
		return auditVerdict{
			Approved:  false,
			Summary:   "audit failed: subagent reported an error",
			Rationale: report.Err.Error(),
			Raw:       report.Text,
		}
	}

	v := parseAuditVerdict(report.Text)
	te.logger.Info("skill audit verdict",
		"skill", name,
		"approved", v.Approved,
		"summary", v.Summary,
	)
	return v
}

// parseAuditVerdict interprets the audit subagent's report. The
// first non-empty line MUST start with "APPROVE:" or "DENY:" (case-
// sensitive). Anything else is treated as a deny ("verdict not
// parseable") -- fail-closed.
func parseAuditVerdict(text string) auditVerdict {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return auditVerdict{
			Approved: false,
			Summary:  "audit failed: empty report",
			Raw:      text,
		}
	}

	firstLine, rest, _ := strings.Cut(trimmed, "\n")
	firstLine = strings.TrimSpace(firstLine)
	rest = strings.TrimSpace(rest)

	switch {
	case strings.HasPrefix(firstLine, "APPROVE:"):
		return auditVerdict{
			Approved:  true,
			Summary:   strings.TrimSpace(strings.TrimPrefix(firstLine, "APPROVE:")),
			Rationale: rest,
			Raw:       text,
		}
	case strings.HasPrefix(firstLine, "DENY:"):
		return auditVerdict{
			Approved:  false,
			Summary:   strings.TrimSpace(strings.TrimPrefix(firstLine, "DENY:")),
			Rationale: rest,
			Raw:       text,
		}
	default:
		return auditVerdict{
			Approved:  false,
			Summary:   "audit failed: verdict not parseable (must start with 'APPROVE:' or 'DENY:')",
			Rationale: text,
			Raw:       text,
		}
	}
}

// buildAuditManifest walks the extracted skill directory and renders
// a markdown listing of every file with its size, sha256, and either
// inlined content (subject to per-file and total caps) or an "omitted"
// marker. Hidden directories (.git, .github, etc.) are skipped.
func buildAuditManifest(root string) (string, error) {
	type entry struct {
		rel     string
		size    int64
		content []byte
		sha     [32]byte
		binary  bool
	}
	var entries []entry

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Skip hidden directories at any depth (.git, .github, etc.).
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// Skip hidden files at any depth.
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		probeLen := len(content)
		if probeLen > auditBinaryProbeBytes {
			probeLen = auditBinaryProbeBytes
		}
		binary := false
		for i := 0; i < probeLen; i++ {
			if content[i] == 0 {
				binary = true
				break
			}
		}
		entries = append(entries, entry{
			rel:     filepath.ToSlash(rel),
			size:    info.Size(),
			content: content,
			sha:     sha256.Sum256(content),
			binary:  binary,
		})
		return nil
	})
	if err != nil {
		return "", err
	}

	// Sort for deterministic output (helps the auditor reason about
	// the structure and is friendlier to test diffs).
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].rel < entries[j-1].rel; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Total files: %d\n\n", len(entries))

	totalInlined := 0
	for _, e := range entries {
		switch {
		case e.binary:
			fmt.Fprintf(&b, "### %s\n%d bytes; binary; sha256=%x\n[binary file omitted from inlined content]\n\n",
				e.rel, e.size, e.sha)
		case totalInlined >= auditTotalMaxBytes:
			fmt.Fprintf(&b, "### %s\n%d bytes; sha256=%x\n[content omitted: total inlined cap (%d KiB) reached]\n\n",
				e.rel, e.size, e.sha, auditTotalMaxBytes/1024)
		case len(e.content) > auditPerFileMaxBytes:
			fmt.Fprintf(&b, "### %s\n%d bytes total; first %d shown; sha256=%x\n```\n%s\n```\n[truncated -- file exceeds per-file cap]\n\n",
				e.rel, e.size, auditPerFileMaxBytes, e.sha, e.content[:auditPerFileMaxBytes])
			totalInlined += auditPerFileMaxBytes
		default:
			fmt.Fprintf(&b, "### %s\n%d bytes; sha256=%x\n```\n%s\n```\n\n",
				e.rel, e.size, e.sha, e.content)
			totalInlined += int(e.size)
		}
	}

	return b.String(), nil
}
