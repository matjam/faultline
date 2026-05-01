package agent

import (
	"context"

	"github.com/matjam/faultline/internal/adapters/llm/kobold"
	"github.com/matjam/faultline/internal/llm"
	"github.com/matjam/faultline/internal/search/bm25"
)

// ChatModel is the LLM port. The agent does not care which backend
// provides the response (real OpenAI, KoboldCpp, vLLM, llama.cpp, an
// in-process fake for tests) so long as the request/response shape
// matches the OpenAI chat-completions de facto standard.
type ChatModel interface {
	Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error)
}

// Memory is the file-backed memory port from the agent's perspective.
// Tools deal with a much richer Memory surface; the agent itself only
// reads recent files (for the system prompt) and the full corpus (for
// search-index rebuilds), and seeds prompt files via the prompts loader
// which uses Read/Write.
type Memory interface {
	AllFiles() (map[string]string, error)
	RecentFiles(n int) ([]bm25.Result, error)
	Read(path string) (string, error)
	Write(path, content string) error
}

// Searcher is the in-memory search index. The agent rebuilds it whenever
// the corpus changes (startup, compaction); tools call into it for
// memory_search and update it on edits.
type Searcher interface {
	Build(docs map[string]string)
}

// Operator is the collaborator messaging port. May be nil when no
// collaborator channel is configured. Pending drains and returns all
// queued messages atomically.
type Operator interface {
	Pending() []string
}

// Tokenizer counts tokens, aborts in-flight generations, and exposes
// recent backend perf metrics. May be nil when no real tokenizer is
// available; the agent falls back to a heuristic estimator. PerfInfo
// leaks the kobold adapter's value type into the agent's import graph;
// this is intentional and bounded -- it's a value type with no
// behavior, and avoiding the leak would require a parallel struct
// that gets translated 1:1 at every boundary.
type Tokenizer interface {
	CountMessages(ctx context.Context, messages []llm.Message) int
	Abort(ctx context.Context)
	Detected() bool
	Version() string
	Perf(ctx context.Context) (*kobold.PerfInfo, error)
}

// Tools is the tool registry and dispatcher the agent drives the LLM
// against. Implemented by *tools.Executor. ToolDefs is fixed for the
// life of the process; Execute runs one tool call and returns the
// result body that will be wrapped in a tool-role chat message.
type Tools interface {
	ToolDefs() []llm.Tool
	Execute(ctx context.Context, call llm.ToolCall) string
	SetContextInfo(currentTokens int)
	Close()
}

// StateStore persists the conversation log across restarts. Path and
// logger are bound at construction; the agent calls Save after every
// turn and Load on startup.
type StateStore interface {
	Save(messages []llm.Message, idleStreak int) error
	Load() ([]llm.Message, int, error)
}
