# Agents Guide

This document describes the code structure, architecture, and design decisions in Faultline to help AI coding agents (and humans) navigate and modify the codebase effectively.

## Project Layout

Faultline is a single `package main` Go project. All source files live in the repository root with no sub-packages. The logical modules are separated by file:

```
faultline/
  main.go          Entry point, signal handling, two-phase shutdown
  agent.go         Core agent loop, context management, compaction, shutdown
  config.go        TOML configuration loading and struct definitions
  llm.go           Hand-rolled OpenAI-compatible chat completions client
  openai.go        Wire-format types for the chat completions API
  kobold.go        Optional KoboldCpp native-API client (tokencount, abort, perf)
  memory.go        File-based persistent memory store with trash/restore
  index.go         BM25 search index (pure Go, in-memory)
  tools.go         Tool definitions, dispatch, web fetching, HTML-to-markdown
  telegram.go      Telegram bot for bidirectional collaborator communication
  sandbox.go       Docker-based Python script execution environment
  prompt.go        Prompt loading, embedding, template rendering
  log.go           Daily-rotating log files and multi-handler for slog
  prompts/         Embedded prompt templates (compiled into binary)
    system.md      Main system prompt
    compaction.md  Context compaction instructions
    cycle_start.md First message at startup
    continue.md    Injected when agent responds without tool calls
    shutdown.md    Graceful shutdown instructions
```

## Architecture Overview

```
main.go
  |
  v
Agent.Run() -- infinite loop
  |
  +-> Build system prompt (system.md + recent memories + timestamp)
  |
  +-> LLM Chat Request (messages + tool definitions)
  |     |
  |     v
  |   LLMClient.Chat() --> OpenAI-compatible API
  |     |
  |     v
  |   Response: tool calls or text
  |
  +-> Tool execution (if tool calls present)
  |     +-> web_fetch        (HTTP + HTML-to-markdown + TTL cache)
  |     +-> memory_*         (MemoryStore -- filesystem CRUD)
  |     +-> memory_search    (BM25 SearchIndex)
  |     +-> send_message     (Telegram outbound)
  |     +-> sandbox_*        (Docker Python execution)
  |     +-> context_status   (token usage + backend perf via KoboldExtras if detected)
  |     +-> get_time         (current timestamp)
  |     +-> sleep            (suspend N seconds, interrupted by operator messages)
  |
  +-> If text-only response: inject continue prompt, loop
  |
  +-> Context compaction (when tokens > threshold)
  |     +-> Ask agent to save state to memory
  |     +-> Agent writes summary
  |     +-> Rebuild context from system prompt + fresh memories + summary
  |
  +-> Graceful shutdown (on first SIGINT)
        +-> Inject shutdown prompt
        +-> Agent saves state (up to 10 turns, 2min timeout)
```

## Module Details

### main.go (117 lines)

Entry point. Parses the `-config` flag, loads TOML configuration, sets up dual logging (console at configurable level, file always at DEBUG with daily rotation), implements two-phase shutdown via signal handling, optionally starts the Telegram bot, creates the `Agent`, and calls `agent.Run()`.

The two-phase shutdown works by maintaining two channels: `shutdownCh` (closed on first signal) and a context cancellation (on second signal). The agent checks `shutdownCh` at the top of each turn.

### agent.go (512 lines)

The `Agent` struct and its operation loop. Key components:

- **`Agent` struct**: Holds references to all subsystems (`LLMClient`, `MemoryStore`, `SearchIndex`, `ToolExecutor`, `Telegram`, `Sandbox`, optional `KoboldExtras`).
- **`NewAgent()`**: Initializes all subsystems, builds the initial search index from existing memory files. If `kobold_extras` is enabled in config, attempts a 5s detection probe against `/api/extra/version`; on failure the kobold pointer stays nil and the agent uses heuristic token counts.
- **`countMessageTokens()`**: Single chokepoint for all token estimation. Uses the real KoboldCpp tokenizer when available, falls back to `EstimateMessagesTokens` otherwise. Bounded by a 5s timeout.
- **`abortInFlight()`**: Called in the LLM-error path when the parent context was canceled (forced shutdown). Best-effort POST to KoboldCpp's `/api/extra/abort` so the model actually stops generating server-side. No-op when KoboldExtras isn't available.
- **`Run()`**: The main infinite loop. Each iteration: checks for shutdown, injects pending collaborator messages, checks if compaction is needed, sends to LLM (without cancellation), then processes the response -- handling tool calls, deferring tool calls if a collaborator message arrived during generation, or injecting a continue prompt for plain text responses.
- **`compactContext()`**: Injects the compaction prompt, gives the agent up to 10 turns to save state and produce a summary, then calls `rebuildContext()`.
- **`rebuildContext()`**: Rebuilds the search index, reloads prompts (which may have been modified by the agent), gathers fresh context memories, and constructs a new message list.
- **`gracefulSave()`**: Similar to compaction but triggered by shutdown signal, with a 2-minute timeout.
- **`gatherContextMemories()`**: Returns the 5 most recently modified non-operational memory files to include in the system prompt.
- **Collaborator messages mid-generation**: The LLM request is never cancelled. After a response comes back, any collaborator messages queued during generation are drained. If the response was text-only, the collaborator messages are appended in place of the continue prompt. If the response contained tool calls, each `tool_call_id` is satisfied with a "deferred" stub (required to keep the OpenAI message log valid) and the collaborator messages are appended -- letting the agent reply first and decide whether to re-issue the deferred calls.

### config.go (126 lines)

Defines the `Config` struct with nested sections: `APIConfig`, `AgentConfig`, `TelegramConfig`, `LogConfig`, `SandboxConfig`. Includes a custom `duration` type that implements `encoding.TextUnmarshaler` for TOML duration strings (e.g., `"60s"`, `"5m"`). `DefaultConfig()` provides sensible defaults. `LoadConfig()` reads and parses the TOML file, with missing fields keeping defaults.

### llm.go (273 lines)

`LLMClient` is a hand-rolled HTTP client for the OpenAI-compatible `/v1/chat/completions` endpoint. We previously used `github.com/sashabaranov/go-openai`, but that library has no public API for injecting vendor-specific extras (`top_k`, `min_p`, `repetition_penalty`) into chat completion requests. Since the agent only needs one POST endpoint with no streaming, dropping the dependency was simpler than working around it. Key details:

- **Construction**: `NewLLMClient(apiURL, apiKey, model, logger)`. The HTTP client has no global timeout (a long generation may legitimately take many minutes); cancellation is driven by the caller's context. The Bearer token is set on each request when `apiKey` is non-empty.
- **`ChatRequest`** is the public input. The `Model` field is filled in by the client from its configured value, so callers don't set it. Sampler fields (Temperature, TopP, PresencePenalty, FrequencyPenalty, Seed, MaxTokens, plus the vendor extensions TopK, MinP, RepetitionPenalty) use zero-value-omitted semantics: a 0 means "don't send, let the server decide." Seed=0 specifically means "unset" because random seeds are the typical default.
- **`chatRequestWire`** is the internal JSON shape we POST. Kept separate so we control exactly which keys appear on the wire and with what `omitempty` behavior. Vendor extensions ride alongside the spec fields; servers that don't recognize them silently ignore them.
- **`Chat()`**: marshals the wire struct, POSTs to `apiURL + "/chat/completions"`, parses non-2xx responses through an OpenAI-style error envelope before falling back to the raw body, decodes successful responses into a `ChatResponse`. Logs only new messages since the last call (avoids re-logging the full context on every turn).
- **`EstimateTokens()` / `EstimateMessagesTokens()`**: rough heuristic of ~4 characters per token. Used as the fallback when KoboldCpp's real tokenizer isn't available.

### openai.go (85 lines)

Wire-format types matching the OpenAI chat completions API. Defines `Message`, `ToolCall`, `FunctionCall`, `Tool`, `FunctionDef`, `ChatResponse`, `Choice`, plus the role constants (`RoleSystem`, `RoleUser`, `RoleAssistant`, `RoleTool`) and `ToolTypeFunction`. JSON tags match what `go-openai` used to write so existing state files continue to load without migration. Fields go-openai included that we don't use (Refusal, MultiContent, Name, ReasoningContent, the deprecated FunctionCall) are intentionally omitted -- `encoding/json` silently ignores unknown fields on Unmarshal, so legacy state files with those keys still parse cleanly.

### kobold.go (260 lines)

`KoboldExtras` is an optional client for KoboldCpp-specific endpoints (`/api/extra/version`, `/api/extra/tokencount`, `/api/extra/abort`, `/api/extra/perf`) that sit alongside the OpenAI compatibility layer at the same base URL. Activated by `[api] kobold_extras = true` in config (default true). Key details:

- **Detection**: `Detect()` probes `/api/extra/version` at startup with a 5s timeout. Success marks the client as detected; failure leaves it unusable and the agent falls back to heuristics. The agent never depends on this client succeeding.
- **`CountString` / `CountMessages`**: Real tokenization via `/api/extra/tokencount`. `CountMessages` concatenates message contents and tool-call payloads into a single batched request, then adds a small per-message constant (`koboldChatTemplateOverhead = 10`) to approximate chat-template scaffolding the tokenizer endpoint can't see. Falls back to the heuristic on any error.
- **`Abort()`**: Best-effort POST to `/api/extra/abort`. Called from the agent's forced-shutdown path so the model actually stops generating instead of leaving the GPU pinned until the backend notices the client is gone.
- **`Perf()`**: Returns recent backend performance info surfaced in `context_status`.
- **Nil-safe receivers**: `Detected()` and `Version()` accept a nil `*KoboldExtras` so call sites in the agent and tools can use them without explicit nil checks.

### memory.go (855 lines)

`MemoryStore` -- the largest file after `tools.go`. A full-featured file-based persistence layer. Key design:

- **Base directory**: All files stored under a configurable root (default `./memory`).
- **Path normalization**: All paths are lowercased, cleaned, and verified to not escape the base directory. `.md` extension is auto-appended.
- **Trash system**: Deletions move files to `.trash/` under the memory root. Supports restore and empty trash.
- **Operations**: `Read` (with optional offset/lines for partial reads), `Write`, `Edit` (find-and-replace with optional replace-all), `Append`, `Insert` (before a line number), `Delete`, `Restore`, `Move`, `List` (with metadata), `Grep` (regex within a file), `AllFiles` (for indexing), `RecentFiles` (by modification time), `DirSize`.
- **Thread safety**: Operations use the filesystem as the synchronization mechanism (no explicit mutexes).
- **SearchResult**: Used both for search results and for passing recent memories to the system prompt builder.

### index.go (278 lines)

`SearchIndex` -- a pure-Go BM25 search engine. Key details:

- **Tokenization**: Splits on non-alphanumeric boundaries, lowercases, filters out stop words and tokens shorter than 2 characters.
- **BM25 parameters**: k1=1.5, b=0.75 (standard values).
- **Operations**: `Build` (from a map of path->content), `Search` (returns scored results), `Update` (single document), `Remove`/`RemovePrefix`.
- **In-memory only**: Rebuilt from disk on every startup and context rebuild.

### tools.go (2151 lines)

The largest file. Contains all tool definitions and execution handlers. Key sections:

- **`ToolDefs()`** (~345 lines): Returns OpenAI tool definitions. Sandbox tools are conditionally included based on configuration.
- **`ToolExecutor` struct**: Holds references to `MemoryStore`, `SearchIndex`, `Telegram`, `Sandbox`, plus a `webCache` for fetch results and a `contextTokens` field for reporting.
- **`Execute()`**: Central dispatch -- parses the tool call arguments as JSON, switches on the function name, calls the appropriate handler.
- **Web fetch** (~400 lines): Full HTTP client with custom HTML-to-markdown converter. Handles headings, lists (ordered/unordered), tables, blockquotes, code blocks, links, images, pre-formatted text, and more. Results are cached in a `webCache` with TTL and background eviction.
- **Memory tool handlers**: Thin wrappers around `MemoryStore` methods, formatting results as human-readable strings.
- **Sandbox tool handlers**: Delegate to the `Sandbox` struct methods.
- **`context_status`**: Reports estimated token usage vs. maximum.
- **`get_time`**: Returns the current timestamp.
- **`sleep`**: Suspends the agent for N seconds. Polls `Telegram.HasPending()` every 500ms so an operator message wakes the sleep without draining the queue (the agent's normal between-turn drain handles it). Bounded by `agent.max_sleep` (default 15m); requests above the cap are clamped with a note in the result. Returns immediately if a collaborator message is already queued at entry.

### telegram.go (217 lines)

`Telegram` struct for bidirectional collaborator communication:

- **Incoming**: Long-polls for updates via `GetUpdatesChan()`. Only accepts messages from the configured chat ID. Queues messages in a mutex-protected slice for the agent to drain on its next turn boundary.
- **Outgoing**: Converts markdown to Telegram MarkdownV2 using `goldmark-tgmd`. Auto-chunks messages at 4000 characters (below the 4096 limit), splitting at UTF-8 boundaries and preferring newline breaks. Falls back to plain text if MarkdownV2 conversion or sending fails.
- **`Pending()`**: Drains and returns all queued messages atomically.

### sandbox.go (736 lines)

`Sandbox` -- Docker-based Python execution:

- **Directory structure**: Flat layout with `scripts/`, `input/`, `output/` subdirectories. A `pyproject.toml` is seeded on init.
- **Container lifecycle**: Ephemeral containers created per operation (`docker run --rm`). Uses the host UID/GID for file ownership.
- **Security**: Filenames validated against a strict regex pattern (`^[a-z0-9][a-z0-9._-]*$`). Network access configurable. Memory limits enforced via Docker.
- **Package management**: Uses `uv` (fast Python package manager). Supports install, upgrade, and remove. Dependencies tracked in `pyproject.toml`.
- **Execution**: Scripts run with a configurable timeout. stdout/stderr captured and returned. Execution is logged to daily log files.
- **File operations**: Read, write, list, and delete files within the sandbox folders.

### prompt.go (125 lines)

Manages the mutable prompt system:

- **Embedding**: Default prompts are compiled into the binary via `//go:embed` directives from `prompts/*.md`.
- **Seeding**: On first run, defaults are written to the memory store. Subsequent runs load from disk, allowing the agent to have modified them.
- **Template rendering**: `RenderPrompt()` substitutes `{{TIME}}` with the current timestamp.
- **`BuildCycleContext()`**: Assembles the full system message by combining the system prompt, current timestamp, and up to 5 recent memories. Each memory is clipped to `Limits.RecentMemoryChars` (default 8000); a non-positive value disables the cap. When clipped, a retrieval hint is appended naming the file and suggesting a `memory_read` offset to resume from. The same pattern is used by `memory_search` (`Limits.MemorySearchResultChars`, default 6000) and sandbox script/shell output (`Limits.SandboxOutputChars`, default 64000).

### log.go (133 lines)

- **`DailyFileWriter`**: An `io.Writer` that auto-rotates to date-stamped files (`YYYY-MM-DD.log`). Supports an optional filename prefix (used by sandbox logs). Thread-safe via mutex.
- **`MultiHandler`**: A `slog.Handler` that fans log records to multiple handlers. Used to combine console output (at configurable level) with file output (always at DEBUG).

## Key Design Patterns

1. **Continuous autonomous operation**: The agent runs indefinitely without human prompting. There is no request-response cycle -- it just keeps going.

2. **Context compaction**: When the conversation grows too large, the agent saves state and summarizes, then context is rebuilt. This enables indefinite operation within a fixed context window.

3. **Self-modifying prompts**: The agent can read and rewrite its own system prompt and other operational prompts via the memory system. Changes take effect on the next context rebuild (compaction or restart).

4. **Two-phase shutdown**: First signal triggers graceful state-saving; second signal forces immediate exit. The agent has up to 10 turns and 2 minutes to save.

5. **Cooperative collaborator handoff**: Incoming Telegram messages never cancel an in-flight LLM request. The agent finishes its current thought, then on the next opportunity (after a text response, or as a deferral of tool calls) the collaborator message is injected, so the model can respond before deciding whether to continue its previous plan.

6. **Soft delete with trash**: Memory files are moved to `.trash/` on delete rather than being permanently removed, enabling restoration.

7. **Single package**: All code is in `package main` with no internal packages. Each file is a logical module. This keeps the project simple but means all types are in the same namespace.

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/BurntSushi/toml` | TOML configuration parsing |
| `github.com/go-telegram-bot-api/telegram-bot-api/v5` | Telegram bot API |
| `github.com/Mad-Pixels/goldmark-tgmd` | Markdown to Telegram MarkdownV2 |
| `golang.org/x/net` | HTML parsing for web_fetch |
| `github.com/yuin/goldmark` | Markdown parser (indirect, used by goldmark-tgmd) |

The OpenAI-compatible chat completions client is hand-rolled in `llm.go` + `openai.go` (no SDK dependency).

## Runtime Dependencies

- **Docker**: Required for the sandbox feature. Must be available in PATH.
- **Network access**: Required for the LLM API, web fetching, and Telegram.

## Data Flow Summary

1. **Startup**: Load config -> init memory store -> build BM25 index -> seed prompts -> start Telegram -> create agent -> enter loop.
2. **Each turn**: Check shutdown -> inject collaborator messages -> check compaction -> send to LLM -> process tool calls or inject continue prompt.
3. **Compaction**: Inject compaction prompt -> agent saves state via tools -> extract summary -> rebuild index -> reload prompts -> rebuild messages.
4. **Shutdown**: Inject shutdown prompt -> agent saves state via tools -> exit.

## Notes for Contributors

- There are currently no tests in the project.
- Token estimation is a rough heuristic (~4 chars/token), not a proper tokenizer.
- All memory file paths are lowercased for case-insensitive access.
- The HTML-to-markdown converter in `tools.go` is substantial (~400 lines) and handles most common HTML elements. It is not a third-party library.
- The default API URL in `DefaultConfig()` points to a local network address -- change this for your setup.
- The `webCache` runs a background goroutine for eviction; it is stopped via `Close()` when the `ToolExecutor` is closed.
