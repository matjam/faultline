package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/matjam/faultline/internal/config"
	"github.com/matjam/faultline/internal/llm"
	"github.com/matjam/faultline/internal/search/bm25"
)

func TestRunSuppliesToolDefsToPromptMigrations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	chat := &migrationToolDefsChat{cancel: cancel}
	cfg := config.Default()
	cfg.Limits.RecentMemoryChars = 1024

	agent := New(cfg, Deps{
		Chat:     chat,
		Memory:   newAgentTestMemory(),
		Search:   noopSearcher{},
		Tools:    migrationToolDefsTools{},
		State:    emptyStateStore{},
		MaxTurns: 1,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := agent.Run(ctx, make(chan struct{}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if len(chat.firstToolNames) == 0 {
		t.Fatal("expected prompt migration Chat request to include tool definitions")
	}
	if chat.firstToolNames[0] != "memory_write" {
		t.Fatalf("first tool name = %q, want memory_write", chat.firstToolNames[0])
	}
}

type migrationToolDefsChat struct {
	cancel         context.CancelFunc
	firstToolNames []string
	once           sync.Once
}

func (c *migrationToolDefsChat) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	c.once.Do(func() {
		for _, tool := range req.Tools {
			if tool.Function != nil {
				c.firstToolNames = append(c.firstToolNames, tool.Function.Name)
			}
		}
		c.cancel()
	})
	return &llm.ChatResponse{
		Choices: []llm.Choice{{
			Message: llm.Message{
				Role:    llm.RoleAssistant,
				Content: "migration done",
			},
			FinishReason: "stop",
		}},
	}, nil
}

type migrationToolDefsTools struct{}

func (migrationToolDefsTools) ToolDefs() []llm.Tool {
	return []llm.Tool{{
		Type: llm.ToolTypeFunction,
		Function: &llm.FunctionDef{
			Name:       "memory_write",
			Parameters: map[string]any{"type": "object"},
		},
	}}
}

func (migrationToolDefsTools) Execute(context.Context, llm.ToolCall) string { return "ok" }
func (migrationToolDefsTools) SetContextInfo(int)                           {}
func (migrationToolDefsTools) Close()                                       {}

type agentTestMemory struct {
	files map[string]string
}

func newAgentTestMemory() *agentTestMemory {
	return &agentTestMemory{files: map[string]string{}}
}

func (m *agentTestMemory) AllFiles() (map[string]string, error) {
	out := make(map[string]string, len(m.files))
	for path, content := range m.files {
		out[path] = content
	}
	return out, nil
}

func (m *agentTestMemory) RecentFiles(int) ([]bm25.Result, error) { return nil, nil }

func (m *agentTestMemory) Read(path string) (string, error) {
	content, ok := m.files[path]
	if !ok {
		return "", errors.New("not found")
	}
	return content, nil
}

func (m *agentTestMemory) Write(path, content string) error {
	m.files[path] = content
	return nil
}

type noopSearcher struct{}

func (noopSearcher) Build(map[string]string) {}

type emptyStateStore struct{}

func (emptyStateStore) Save([]llm.Message, int) error { return nil }
func (emptyStateStore) Load() ([]llm.Message, int, error) {
	return nil, 0, nil
}
