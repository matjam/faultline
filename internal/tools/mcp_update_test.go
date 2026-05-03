package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matjam/faultline/internal/adapters/mcp"
	"github.com/matjam/faultline/internal/llm"
	"github.com/matjam/faultline/internal/subagent"
)

func TestMCPConfigUpdateRequiresRawApproval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	approvals := mcp.NewApprovals()
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPConfigFile:        path,
		MCPConfigEditEnabled: true,
		MCPApprovals:         approvals,
	})

	configJSON := `{"servers":[{"name":"github","transport":"http","url":"https://example.invalid/mcp","allow_tools":["search_repositories"]}]}`
	proposal := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: proposalArgs(t, emptyMCPConfigHash(t), configJSON),
		},
	})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("proposal should not write config file, stat err=%v", err)
	}

	approvalLine := approvalLineFromProposal(t, proposal)
	rejected := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_update_config",
			Arguments: `{"approval_id":"` + approvalIDFromLine(t, approvalLine) + `","config":` + configJSON + `}`,
		},
	})
	if !strings.Contains(rejected, "approval") {
		t.Fatalf("expected approval rejection, got %q", rejected)
	}

	te.RecordCollaboratorMessage(approvalLine)
	updated := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_update_config",
			Arguments: `{"approval_id":"` + approvalIDFromLine(t, approvalLine) + `","config":` + configJSON + `}`,
		},
	})
	if !strings.Contains(updated, "updated") {
		t.Fatalf("expected update success, got %q", updated)
	}
	if _, err := mcp.LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}

func TestMCPConfigProposalSendsApprovalRequestWhenNotifierConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	approvals := mcp.NewApprovals()
	var sent []string
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPConfigFile:        path,
		MCPConfigEditEnabled: true,
		MCPApprovals:         approvals,
		MCPApprovalNotifier: func(text string) error {
			sent = append(sent, text)
			return nil
		},
	})

	configJSON := `{"servers":[{"name":"github","transport":"http","url":"https://example.invalid/mcp","allow_tools":["search_repositories"]}]}`
	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: proposalArgs(t, emptyMCPConfigHash(t), configJSON),
		},
	})

	if len(sent) != 1 {
		t.Fatalf("sent messages = %d, want 1", len(sent))
	}
	for _, want := range []string{"A MCP config change has been requested:", "```diff", "Review the proposed diff in the agent/tool output, then reply exactly with:", "`APPROVE MCP"} {
		if !strings.Contains(sent[0], want) {
			t.Fatalf("sent approval request missing %q:\n%s", want, sent[0])
		}
	}
	if !strings.Contains(sent[0], "diff --git") {
		t.Fatalf("sent approval request should include compact diff:\n%s", sent[0])
	}
	if strings.Contains(sent[0], "ghp_") {
		t.Fatalf("sent approval request leaked secret:\n%s", sent[0])
	}
	if !strings.Contains(got, "approval request sent") {
		t.Fatalf("tool result = %q, want sent confirmation", got)
	}
	if strings.Contains(got, "APPROVE MCP") {
		t.Fatalf("tool result should not rely on LLM relaying approval phrase:\n%s", got)
	}
}

func TestMCPConfigProposalFallsBackWhenNotifierFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPConfigFile:        path,
		MCPConfigEditEnabled: true,
		MCPApprovals:         mcp.NewApprovals(),
		MCPApprovalNotifier: func(string) error {
			return errors.New("telegram unavailable")
		},
	})

	configJSON := `{"servers":[{"name":"github","transport":"http","url":"https://example.invalid/mcp","allow_tools":["search_repositories"]}]}`
	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: proposalArgs(t, emptyMCPConfigHash(t), configJSON),
		},
	})

	for _, want := range []string{"sending approval request failed", "telegram unavailable", "Exact proposed diff:", "APPROVE MCP"} {
		if !strings.Contains(got, want) {
			t.Fatalf("fallback result missing %q:\n%s", want, got)
		}
	}
}

func TestMCPConfigUpdateReloadsLiveToolSurface(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	approvals := mcp.NewApprovals()
	reloadCalled := false
	oldCaller := &closeTrackingMCPCaller{}
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPCaller:            oldCaller,
		MCPConfigFile:        path,
		MCPConfigEditEnabled: true,
		MCPApprovals:         approvals,
		MCPReload: func(context.Context) (mcp.Caller, []mcp.DiscoveredServer, error) {
			reloadCalled = true
			return &fakeMCPCaller{}, []mcp.DiscoveredServer{
				{
					Server: mcp.ServerConfig{
						Name:       "github",
						Transport:  "http",
						URL:        "https://example.invalid/mcp",
						AllowTools: []string{"search_repositories"},
					},
					Tools: []mcp.DiscoveredTool{{Name: "search_repositories"}},
				},
			}, nil
		},
	})

	configJSON := `{"servers":[{"name":"github","transport":"http","url":"https://example.invalid/mcp","allow_tools":["search_repositories"]}]}`
	proposal := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: proposalArgs(t, emptyMCPConfigHash(t), configJSON),
		},
	})
	approvalLine := approvalLineFromProposal(t, proposal)
	te.RecordCollaboratorMessage(approvalLine)
	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_update_config",
			Arguments: `{"approval_id":"` + approvalIDFromLine(t, approvalLine) + `","config":` + configJSON + `}`,
		},
	})
	if !strings.Contains(got, "updated") {
		t.Fatalf("expected update success, got %q", got)
	}
	if !reloadCalled {
		t.Fatal("expected successful config update to reload live MCP state")
	}
	if !oldCaller.closed {
		t.Fatal("old MCP caller should close after reload when no subagents are active")
	}
	names := toolDefNames(te.ToolDefs())
	if !names["mcp_github_search_repositories"] {
		t.Fatal("expected reloaded allowlisted MCP tool in ToolDefs")
	}
}

func TestMCPConfigUpdateDefersOldCallerCloseWhileSubagentActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	approvals := mcp.NewApprovals()
	mgr := subagent.New(
		subagent.Config{RunTimeout: time.Hour},
		[]subagent.Profile{{Name: subagent.DefaultProfileName}},
		func(ctx context.Context, workID string, _ subagent.Profile, _ string, _ int) subagent.Report {
			<-ctx.Done()
			return subagent.Report{WorkID: workID, Canceled: true}
		},
		silentTestLogger(),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workID, err := mgr.Spawn(ctx, subagent.DefaultProfileName, "hold old caller")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = mgr.Cancel(workID) }()

	oldCaller := &closeTrackingMCPCaller{}
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPCaller:            oldCaller,
		MCPConfigFile:        path,
		MCPConfigEditEnabled: true,
		MCPApprovals:         approvals,
		MCPReload: func(context.Context) (mcp.Caller, []mcp.DiscoveredServer, error) {
			return &fakeMCPCaller{}, nil, nil
		},
		SubagentManager: mgr,
	})

	configJSON := `{"servers":[{"name":"github","transport":"http","url":"https://example.invalid/mcp","allow_tools":["search_repositories"]}]}`
	proposal := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: proposalArgs(t, emptyMCPConfigHash(t), configJSON),
		},
	})
	approvalLine := approvalLineFromProposal(t, proposal)
	te.RecordCollaboratorMessage(approvalLine)
	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_update_config",
			Arguments: `{"approval_id":"` + approvalIDFromLine(t, approvalLine) + `","config":` + configJSON + `}`,
		},
	})
	if !strings.Contains(got, "updated") {
		t.Fatalf("expected update success, got %q", got)
	}
	if oldCaller.closed {
		t.Fatal("old MCP caller should stay open while a subagent is active")
	}

	te.Close()
	if !oldCaller.closed {
		t.Fatal("deferred old MCP caller should close with the primary executor")
	}
}

func TestMCPConfigUpdateRejectsHashMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPConfigFile:        path,
		MCPConfigEditEnabled: true,
		MCPApprovals:         mcp.NewApprovals(),
	})

	proposedConfig := `{"servers":[{"name":"github","transport":"http","url":"https://example.invalid/mcp","allow_tools":["search_repositories"]}]}`
	changedConfig := `{"servers":[{"name":"github","transport":"http","url":"https://example.invalid/mcp","allow_tools":["delete_repository"]}]}`
	proposal := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: proposalArgs(t, emptyMCPConfigHash(t), proposedConfig),
		},
	})
	approvalLine := approvalLineFromProposal(t, proposal)
	te.RecordCollaboratorMessage(approvalLine)

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_update_config",
			Arguments: `{"approval_id":"` + approvalIDFromLine(t, approvalLine) + `","config":` + changedConfig + `}`,
		},
	})
	if !strings.Contains(got, "approval") {
		t.Fatalf("expected approval rejection, got %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("hash mismatch should not write config file, stat err=%v", err)
	}
}

func TestMCPConfigProposalRequiresConfigUpdatesEnabled(t *testing.T) {
	te := New(Deps{
		Logger:       silentTestLogger(),
		MCPApprovals: mcp.NewApprovals(),
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: `{"config":{"servers":[]}}`,
		},
	})
	if !strings.Contains(got, "not configured") {
		t.Fatalf("expected not configured error, got %q", got)
	}
}

func TestMCPReadConfigReturnsCurrentRawConfigAndHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	cfg := mcp.Config{Servers: []mcp.ServerConfig{
		{
			Name:       "github",
			Transport:  "stdio",
			Command:    "github-mcp",
			Env:        map[string]string{"GITHUB_TOKEN": "raw-secret"},
			AllowTools: []string{"search_repositories"},
		},
	}}
	if err := mcp.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPConfigFile:        path,
		MCPConfigEditEnabled: true,
		MCPApprovals:         mcp.NewApprovals(),
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "mcp_read_config"},
	})

	var result struct {
		Config     mcp.Config `json:"config"`
		ConfigHash string     `json:"config_hash"`
	}
	if err := json.Unmarshal([]byte(got), &result); err != nil {
		t.Fatalf("mcp_read_config returned invalid JSON: %v\n%s", err, got)
	}
	wantHash, err := mcp.ConfigHash(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConfigHash != wantHash {
		t.Fatalf("config_hash = %q, want %q", result.ConfigHash, wantHash)
	}
	if got := result.Config.Servers[0].Env["GITHUB_TOKEN"]; got != "raw-secret" {
		t.Fatalf("env token = %q, want raw-secret", got)
	}
}

func TestMCPConfigProposalRequiresBaseConfigHash(t *testing.T) {
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPConfigFile:        filepath.Join(t.TempDir(), "mcp.json"),
		MCPConfigEditEnabled: true,
		MCPApprovals:         mcp.NewApprovals(),
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: `{"config":{"servers":[]}}`,
		},
	})
	if !strings.Contains(got, "base_config_hash") {
		t.Fatalf("expected base_config_hash rejection, got %q", got)
	}
}

func TestMCPConfigProposalRejectsStaleBaseConfigHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := mcp.SaveConfig(path, mcp.Config{Servers: []mcp.ServerConfig{
		{Name: "existing", Transport: "http", URL: "https://example.invalid/mcp"},
	}}); err != nil {
		t.Fatal(err)
	}
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPConfigFile:        path,
		MCPConfigEditEnabled: true,
		MCPApprovals:         mcp.NewApprovals(),
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: proposalArgs(t, emptyMCPConfigHash(t), `{"servers":[]}`),
		},
	})
	if !strings.Contains(got, "stale") {
		t.Fatalf("expected stale base hash rejection, got %q", got)
	}
}

func TestMCPConfigProposalIncludesExactDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	current := mcp.Config{Servers: []mcp.ServerConfig{}}
	if err := mcp.SaveConfig(path, current); err != nil {
		t.Fatal(err)
	}
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPConfigFile:        path,
		MCPConfigEditEnabled: true,
		MCPApprovals:         mcp.NewApprovals(),
	})

	configJSON := `{"servers":[{"name":"github","transport":"http","url":"https://example.invalid/mcp","allow_tools":["search_repositories"]}]}`
	proposal := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: proposalArgs(t, configHash(t, current), configJSON),
		},
	})

	for _, want := range []string{
		"config_hash: ",
		"```diff\n",
		"diff --git a/mcp.json b/mcp.json",
		"--- a/mcp.json",
		"+++ b/mcp.json",
		`-  "servers": []`,
		`+      "name": "github",`,
		`+      "url": "https://example.invalid/mcp"`,
		"```",
	} {
		if !strings.Contains(proposal, want) {
			t.Fatalf("proposal diff missing %q:\n%s", want, proposal)
		}
	}
	if strings.Contains(proposal, "diff_hash:") {
		t.Fatalf("proposal should label approval hash as config_hash, got:\n%s", proposal)
	}
}

func TestMCPConfigProposalDiffShowsOnlyChangedHunks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	current := mcp.Config{Servers: []mcp.ServerConfig{
		{Name: "github", Transport: "http", URL: "https://example.invalid/mcp", AllowTools: []string{"search_repositories"}},
		{Name: "slack", Transport: "http", URL: "https://mcp.slack.com/mcp", AllowTools: []string{"search"}},
	}}
	if err := mcp.SaveConfig(path, current); err != nil {
		t.Fatal(err)
	}
	proposed := mcp.Config{Servers: []mcp.ServerConfig{
		current.Servers[0],
		{Name: "slack", Transport: "http", URL: "https://mcp.slack.com/mcp", AllowTools: []string{"search", "channels_history"}},
	}}
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPConfigFile:        path,
		MCPConfigEditEnabled: true,
		MCPApprovals:         mcp.NewApprovals(),
	})
	proposedJSON, err := canonicalMCPConfigJSON(proposed)
	if err != nil {
		t.Fatal(err)
	}

	proposal := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: proposalArgs(t, configHash(t, current), proposedJSON),
		},
	})

	if !strings.Contains(proposal, `+        "channels_history"`) {
		t.Fatalf("proposal diff missing changed allowlist entry:\n%s", proposal)
	}
	if strings.Contains(proposal, `"name": "github"`) {
		t.Fatalf("proposal diff included unrelated unchanged server:\n%s", proposal)
	}
}

func TestMCPConfigProposalRejectsNoopUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	cfg := mcp.Config{Servers: []mcp.ServerConfig{
		{Name: "github", Transport: "http", URL: "https://example.invalid/mcp", AllowTools: []string{"search_repositories"}},
	}}
	if err := mcp.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	te := New(Deps{
		Logger:               silentTestLogger(),
		MCPConfigFile:        path,
		MCPConfigEditEnabled: true,
		MCPApprovals:         mcp.NewApprovals(),
	})

	got := te.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{
			Name:      "mcp_propose_config_update",
			Arguments: proposalArgs(t, configHash(t, cfg), `{"servers":[{"name":"github","transport":"http","url":"https://example.invalid/mcp","allow_tools":["search_repositories"]}]}`),
		},
	})
	if !strings.Contains(got, "No MCP config changes to propose.") {
		t.Fatalf("expected no-op rejection, got:\n%s", got)
	}
	if strings.Contains(got, "APPROVE MCP ") {
		t.Fatalf("no-op proposal should not create approval, got:\n%s", got)
	}
}

func TestSubagentDoesNotAdvertiseConfigUpdateTools(t *testing.T) {
	te := New(Deps{
		Mode:                 ModeSubagent,
		Logger:               silentTestLogger(),
		MCPConfigFile:        filepath.Join(t.TempDir(), "mcp.json"),
		MCPConfigEditEnabled: true,
		MCPApprovals:         mcp.NewApprovals(),
	})
	names := toolDefNames(te.ToolDefs())
	if names["mcp_propose_config_update"] || names["mcp_update_config"] {
		t.Fatal("subagent should not advertise MCP config update tools")
	}
}

func approvalLineFromProposal(t *testing.T, proposal string) string {
	t.Helper()
	for _, line := range strings.Split(proposal, "\n") {
		if strings.HasPrefix(line, "APPROVE MCP ") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("approval line not found in proposal: %s", proposal)
	return ""
}

func approvalIDFromLine(t *testing.T, line string) string {
	t.Helper()
	parts := strings.Fields(line)
	if len(parts) != 4 {
		t.Fatalf("approval line %q has %d fields, want 4", line, len(parts))
	}
	return parts[2]
}

func proposalArgs(t *testing.T, baseHash, configJSON string) string {
	t.Helper()
	baseHashJSON, err := json.Marshal(baseHash)
	if err != nil {
		t.Fatal(err)
	}
	return `{"base_config_hash":` + string(baseHashJSON) + `,"config":` + configJSON + `}`
}

func emptyMCPConfigHash(t *testing.T) string {
	t.Helper()
	return configHash(t, mcp.Config{Servers: []mcp.ServerConfig{}})
}

func configHash(t *testing.T, cfg mcp.Config) string {
	t.Helper()
	hash, err := mcp.ConfigHash(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

type closeTrackingMCPCaller struct {
	closed bool
}

func (c *closeTrackingMCPCaller) CallTool(context.Context, string, string, json.RawMessage) (string, error) {
	return "", nil
}

func (c *closeTrackingMCPCaller) Close() error {
	c.closed = true
	return nil
}
