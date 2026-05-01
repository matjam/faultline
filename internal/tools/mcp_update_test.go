package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matjam/faultline/internal/adapters/mcp"
	"github.com/matjam/faultline/internal/llm"
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
			Arguments: `{"config":` + configJSON + `}`,
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

func TestMCPConfigUpdateReloadsLiveToolSurface(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	approvals := mcp.NewApprovals()
	reloadCalled := false
	te := New(Deps{
		Logger:               silentTestLogger(),
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
			Arguments: `{"config":` + configJSON + `}`,
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
	names := toolDefNames(te.ToolDefs())
	if !names["mcp_github_search_repositories"] {
		t.Fatal("expected reloaded allowlisted MCP tool in ToolDefs")
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
			Arguments: `{"config":` + proposedConfig + `}`,
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
