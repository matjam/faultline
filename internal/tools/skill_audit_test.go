package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matjam/faultline/internal/subagent"
)

func TestParseAuditVerdict_Approve(t *testing.T) {
	v := parseAuditVerdict("APPROVE: looks fine.\n\n## Findings\n\nAll good.")
	if !v.Approved {
		t.Error("expected Approved=true")
	}
	if v.Summary != "looks fine." {
		t.Errorf("Summary = %q", v.Summary)
	}
	if !strings.Contains(v.Rationale, "Findings") {
		t.Errorf("Rationale = %q", v.Rationale)
	}
}

func TestParseAuditVerdict_Deny(t *testing.T) {
	v := parseAuditVerdict("DENY: exfiltrates AWS creds.\n\n## Findings\n\nLine 47.")
	if v.Approved {
		t.Error("expected Approved=false")
	}
	if v.Summary != "exfiltrates AWS creds." {
		t.Errorf("Summary = %q", v.Summary)
	}
}

func TestParseAuditVerdict_FailClosedOnUnparseable(t *testing.T) {
	cases := []string{
		"",
		"  \n  ",
		"approve: lowercase",
		"This skill seems fine.",
		"# Findings\n\nApproved",
	}
	for _, c := range cases {
		v := parseAuditVerdict(c)
		if v.Approved {
			t.Errorf("input %q parsed as approved; want fail-closed deny", c)
		}
		if !strings.Contains(strings.ToLower(v.Summary), "audit failed") {
			t.Errorf("input %q: summary did not mention failure: %q", c, v.Summary)
		}
	}
}

func TestParseAuditVerdict_RequiresColonAndExactPrefix(t *testing.T) {
	// Common LLM mistakes that should still be rejected.
	rejected := []string{
		"APPROVE looks fine",                         // missing colon
		"APPROVED: legacy past tense",                // wrong word
		"DENIED: legacy past tense",                  // wrong word
		"REJECT: not the right verb",                 // not in the spec
		"  APPROVE: leading whitespace might be ok?", // we trim, so this should actually pass; remove from list
	}
	// Note: leading whitespace IS trimmed, so the last case actually passes.
	for _, c := range rejected[:4] {
		v := parseAuditVerdict(c)
		if v.Approved {
			t.Errorf("input %q approved unexpectedly", c)
		}
	}
	// Verify the leading-whitespace case approves (so we know the trim is intentional).
	if v := parseAuditVerdict("  APPROVE: trimmed.\nrationale"); !v.Approved {
		t.Error("leading-whitespace APPROVE should be approved (trim is intentional)")
	}
}

func TestBuildAuditManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: t\ndescription: x\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "main.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Hidden directory: should be skipped.
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("should be skipped"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Binary file (NUL byte): listed but not inlined.
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := buildAuditManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "SKILL.md") {
		t.Error("manifest missing SKILL.md")
	}
	if !strings.Contains(got, "scripts/main.py") {
		t.Errorf("manifest missing scripts/main.py; got: %s", got)
	}
	if !strings.Contains(got, "print('hi')") {
		t.Error("manifest missing inlined python content")
	}
	if !strings.Contains(got, "blob.bin") {
		t.Error("manifest missing blob.bin entry")
	}
	if !strings.Contains(got, "binary file omitted") {
		t.Error("expected binary marker for blob.bin")
	}
	if strings.Contains(got, "should be skipped") {
		t.Error("hidden directory contents leaked into manifest")
	}
	// Total files line: SKILL.md + scripts/main.py + blob.bin = 3.
	if !strings.Contains(got, "Total files: 3") {
		t.Errorf("expected file count 3; got: %s", got)
	}
}

func TestBuildAuditManifest_TruncatesLargeFile(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, auditPerFileMaxBytes+1024)
	for i := range big {
		big[i] = byte('a' + (i % 26))
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := buildAuditManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("expected truncation marker")
	}
}

func TestAuditSkill_ManagerNilSkipsApproved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: t\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	te := New(Deps{Logger: silentTestLogger()})

	v := te.auditSkill(context.Background(), "t", "https://example.com/t.tar.gz", "x", dir)
	if !v.Approved {
		t.Error("expected approved when manager is nil")
	}
	if !v.Skipped {
		t.Error("expected Skipped=true")
	}
}

func TestAuditSkill_RunsAndApproves(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: t\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spawn := func(ctx context.Context, workID string, p subagent.Profile, prompt string, maxTurns int) subagent.Report {
		// Sanity check: prompt should embed the manifest and metadata.
		if !strings.Contains(prompt, "SKILL.md") {
			t.Errorf("audit prompt missing manifest: %s", prompt[:200])
		}
		if !strings.Contains(prompt, "Stated description: x") {
			t.Error("audit prompt missing description")
		}
		return subagent.Report{Text: "APPROVE: looks fine.\n\nNo issues."}
	}
	mgr := subagent.New(subagent.Config{},
		[]subagent.Profile{{Name: subagent.DefaultProfileName, APIURL: "x", Model: "m"}},
		spawn, silentTestLogger())
	te := New(Deps{Logger: silentTestLogger(), SubagentManager: mgr})

	v := te.auditSkill(context.Background(), "t", "https://x/t.tar.gz", "x", dir)
	if !v.Approved {
		t.Errorf("expected approved; summary: %s, rationale: %s", v.Summary, v.Rationale)
	}
	if v.Skipped {
		t.Error("expected Skipped=false when manager runs the audit")
	}
}

func TestAuditSkill_RunsAndDenies(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: t\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spawn := func(ctx context.Context, workID string, p subagent.Profile, prompt string, maxTurns int) subagent.Report {
		return subagent.Report{Text: "DENY: exfiltrates secrets.\n\nFound a POST to attacker.example.com."}
	}
	mgr := subagent.New(subagent.Config{},
		[]subagent.Profile{{Name: subagent.DefaultProfileName, APIURL: "x", Model: "m"}},
		spawn, silentTestLogger())
	te := New(Deps{Logger: silentTestLogger(), SubagentManager: mgr})

	v := te.auditSkill(context.Background(), "t", "src", "x", dir)
	if v.Approved {
		t.Error("expected denied")
	}
	if !strings.Contains(v.Summary, "exfiltrates") {
		t.Errorf("Summary = %q", v.Summary)
	}
}

func TestAuditSkill_TruncatedReportDenies(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: t\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spawn := func(ctx context.Context, workID string, p subagent.Profile, prompt string, maxTurns int) subagent.Report {
		return subagent.Report{Truncated: true, Text: "midway through analysis"}
	}
	mgr := subagent.New(subagent.Config{},
		[]subagent.Profile{{Name: subagent.DefaultProfileName, APIURL: "x", Model: "m"}},
		spawn, silentTestLogger())
	te := New(Deps{Logger: silentTestLogger(), SubagentManager: mgr})

	v := te.auditSkill(context.Background(), "t", "src", "x", dir)
	if v.Approved {
		t.Error("truncated report must fail-closed (deny)")
	}
}
