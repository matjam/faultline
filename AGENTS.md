# Agents Guide

This document describes the code structure, architecture, and design decisions in Faultline to help AI coding agents (and humans) navigate and modify the codebase effectively.

## Project Layout

Faultline is a single `package main` Go project. All source files live in the repository root with no sub-packages. The logical modules are separated by file:

```
faultline/
  main.go          Entry point, signal handling, two-phase shutdown
  agent.go         Core agent loop, context management, compaction, shutdown
  config.go        TOML configuration loading and struct definitions
  llm.go           OpenAI-compatible LLM client wrapper
  memory.go        File-based persistent memory store with trash/restore
  index.go         BM25 search index (pure Go, in-memory)
  tools.go         Tool definitions, dispatch, web fetching, HTML-to-markdown
  telegram.go      Telegram bot for bidirectional operator communication
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
  |     +-> context_status   (token usage reporting)
  |     +-> get_time         (current timestamp)
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

- **`Agent` struct**: Holds references to all subsystems (`LLMClient`, `MemoryStore`, `SearchIndex`, `ToolExecutor`, `Telegram`, `Sandbox`).
- **`NewAgent()`**: Initializes all subsystems, builds the initial search index from existing memory files.
- **`Run()`**: The main infinite loop. Each iteration: checks for shutdown, injects pending operator messages, checks if compaction is needed, creates a per-turn cancellable context (for operator message interrupts), sends to LLM, processes tool calls or injects a continue prompt.
- **`compactContext()`**: Injects the compaction prompt, gives the agent up to 10 turns to save state and produce a summary, then calls `rebuildContext()`.
- **`rebuildContext()`**: Rebuilds the search index, reloads prompts (which may have been modified by the agent), gathers fresh context memories, and constructs a new message list.
- **`gracefulSave()`**: Similar to compaction but triggered by shutdown signal, with a 2-minute timeout.
- **`gatherContextMemories()`**: Returns the 5 most recently modified non-operational memory files to include in the system prompt.
- **Operator interrupts**: A goroutine watches the Telegram wakeup channel and cancels the per-turn context if a message arrives, aborting the in-flight LLM request so the agent processes the message on the next iteration.

### config.go (126 lines)

Defines the `Config` struct with nested sections: `APIConfig`, `AgentConfig`, `TelegramConfig`, `LogConfig`, `SandboxConfig`. Includes a custom `duration` type that implements `encoding.TextUnmarshaler` for TOML duration strings (e.g., `"60s"`, `"5m"`). `DefaultConfig()` provides sensible defaults. `LoadConfig()` reads and parses the TOML file, with missing fields keeping defaults.

### llm.go (139 lines)

`LLMClient` wraps `go-openai`. Key details:

- Uses `openai.DefaultConfig()` with a custom `BaseURL` for OpenAI-compatible endpoints.
- `Chat()` sends completion requests, logging only new messages since the last call (avoids re-logging the full context on every turn).
- `EstimateTokens()` uses a rough heuristic of ~4 characters per token.
- `EstimateMessagesTokens()` sums token estimates across all messages, including tool call names and arguments, plus a small overhead per message.

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

### telegram.go (217 lines)

`Telegram` struct for bidirectional operator communication:

- **Incoming**: Long-polls for updates via `GetUpdatesChan()`. Only accepts messages from the configured chat ID. Queues messages in a mutex-protected slice. Sends a wakeup signal on a buffered channel.
- **Outgoing**: Converts markdown to Telegram MarkdownV2 using `goldmark-tgmd`. Auto-chunks messages at 4000 characters (below the 4096 limit), splitting at UTF-8 boundaries and preferring newline breaks. Falls back to plain text if MarkdownV2 conversion or sending fails.
- **`Pending()`**: Drains and returns all queued messages atomically.
- **`WakeupChan()`**: Returns the channel used by the agent loop to detect incoming messages during LLM calls.

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
- **`BuildCycleContext()`**: Assembles the full system message by combining the system prompt, current timestamp, and up to 5 recent memories (truncated to 2000 chars each).

### log.go (133 lines)

- **`DailyFileWriter`**: An `io.Writer` that auto-rotates to date-stamped files (`YYYY-MM-DD.log`). Supports an optional filename prefix (used by sandbox logs). Thread-safe via mutex.
- **`MultiHandler`**: A `slog.Handler` that fans log records to multiple handlers. Used to combine console output (at configurable level) with file output (always at DEBUG).

## Key Design Patterns

1. **Continuous autonomous operation**: The agent runs indefinitely without human prompting. There is no request-response cycle -- it just keeps going.

2. **Context compaction**: When the conversation grows too large, the agent saves state and summarizes, then context is rebuilt. This enables indefinite operation within a fixed context window.

3. **Self-modifying prompts**: The agent can read and rewrite its own system prompt and other operational prompts via the memory system. Changes take effect on the next context rebuild (compaction or restart).

4. **Two-phase shutdown**: First signal triggers graceful state-saving; second signal forces immediate exit. The agent has up to 10 turns and 2 minutes to save.

5. **Operator interrupts**: Incoming Telegram messages cancel the current LLM request via context cancellation, so the agent processes them on the next loop iteration without waiting for a potentially long generation to complete.

6. **Soft delete with trash**: Memory files are moved to `.trash/` on delete rather than being permanently removed, enabling restoration.

7. **Single package**: All code is in `package main` with no internal packages. Each file is a logical module. This keeps the project simple but means all types are in the same namespace.

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/BurntSushi/toml` | TOML configuration parsing |
| `github.com/sashabaranov/go-openai` | OpenAI-compatible API client |
| `github.com/go-telegram-bot-api/telegram-bot-api/v5` | Telegram bot API |
| `golang.org/x/net` | HTML parsing for web_fetch |
| `github.com/Mad-Pixels/goldmark-tgmd` | Markdown to Telegram MarkdownV2 (indirect) |
| `github.com/yuin/goldmark` | Markdown parser (indirect, used by goldmark-tgmd) |

## Runtime Dependencies

- **Docker**: Required for the sandbox feature. Must be available in PATH.
- **Network access**: Required for the LLM API, web fetching, and Telegram.

## Data Flow Summary

1. **Startup**: Load config -> init memory store -> build BM25 index -> seed prompts -> start Telegram -> create agent -> enter loop.
2. **Each turn**: Check shutdown -> inject operator messages -> check compaction -> send to LLM -> process tool calls or inject continue prompt.
3. **Compaction**: Inject compaction prompt -> agent saves state via tools -> extract summary -> rebuild index -> reload prompts -> rebuild messages.
4. **Shutdown**: Inject shutdown prompt -> agent saves state via tools -> exit.

## Notes for Contributors

- There are currently no tests in the project.
- Token estimation is a rough heuristic (~4 chars/token), not a proper tokenizer.
- All memory file paths are lowercased for case-insensitive access.
- The HTML-to-markdown converter in `tools.go` is substantial (~400 lines) and handles most common HTML elements. It is not a third-party library.
- The default API URL in `DefaultConfig()` points to a local network address -- change this for your setup.
- The `webCache` runs a background goroutine for eviction; it is stopped via `Close()` when the `ToolExecutor` is closed.
