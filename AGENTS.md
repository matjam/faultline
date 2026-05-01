# Agents Guide

This document describes the code structure, architecture, and design decisions in Faultline to help AI coding agents (and humans) navigate and modify the codebase effectively.

## Project Layout

Faultline follows hexagonal (ports & adapters) architecture. The agent loop is the domain hexagon; everything else (LLM, memory, telegram, sandbox, IMAP, state persistence) is an adapter behind a port the domain owns.

```
faultline/
  cmd/faultline/
    main.go                   composition root: parse config, build adapters,
                              wire them into the agent via ports, run the loop
  internal/
    agent/                    the hexagon (domain)
      agent.go                Agent struct, Run() loop, compaction, idle
                              detection, graceful shutdown
      ports.go                interfaces the agent depends on:
                              ChatModel, Memory, Searcher, Operator,
                              Tokenizer, Tools, StateStore
    config/                   TOML config loading
    log/                      DailyFileWriter, MultiHandler (slog)
    prompts/                  embedded prompt templates, Load, LoadAll,
                              Render, BuildCycleContext
      templates/              the embedded *.md defaults
    tools/                    tool registry & dispatcher
      executor.go             Executor + all tool handlers
      wiki.go                 wiki_fetch tool
      html_test.go            HTML-to-markdown converter tests
    search/bm25/              pure-Go BM25 search index
    llm/                      shared LLM types (Message, Tool, ChatRequest, ...)
                              + heuristic token estimator
    adapters/
      llm/openai/             OpenAI-compatible HTTP client + ChatLogger
      llm/kobold/             KoboldCpp extras (tokencount, abort, perf)
      memory/fs/              filesystem-backed Memory adapter
      operator/telegram/      Telegram bot for collaborator messaging
      sandbox/docker/         Docker-based Python sandbox
      email/imap/             IMAP email client
      state/jsonfile/         JSON-file conversation persistence (Save/Load
                              + Persister wrapper that satisfies StateStore)
```

The `internal/` prefix is enforced by Go: nothing outside this module can import these packages. That's a feature.

## Architecture Overview

```
cmd/faultline/main.go
  |
  +-> Parse config, build logger
  |
  +-> Construct adapters:
  |     fs.New, bm25.New, docker.New, openai.New, kobold.New (best-effort
  |     detection), telegram.New, jsonfile.NewPersister, tools.New
  |
  +-> agent.New(cfg, agent.Deps{Chat, Memory, Search, Operator, Tokenizer,
  |                              Tools, State}, logger)
  |
  +-> agent.Run(ctx, shutdownCh)
        |
        v
      Agent.Run -- infinite loop
        |
        +-> Build system prompt (system.md + recent memories + timestamp)
        |   via prompts.BuildCycleContext
        |
        +-> chat.Chat(ctx, llm.ChatRequest{...})
        |
        +-> If tool calls: tools.Execute() per call
        |
        +-> If text-only: inject continue prompt, loop
        |
        +-> Context compaction (when tokens > threshold)
        |     +-> Inject compaction prompt
        |     +-> Agent saves state via tools (memory_write etc.)
        |     +-> Rebuild context from system prompt + summary
        |
        +-> Tool dispatch (delegated to tools.Executor):
        |     +-> web_fetch        (HTTP + HTML-to-markdown + TTL cache)
        |     +-> wiki_fetch       (MediaWiki API + cache)
        |     +-> memory_*         (fs.Store)
        |     +-> memory_search    (bm25.Index)
        |     +-> send_message     (operator port: telegram.Bot)
        |     +-> sandbox_*        (docker.Sandbox)
        |     +-> email_fetch      (short-lived imap.Client per call)
        |     +-> context_status   (token usage + kobold.Client.Perf if detected)
        |     +-> get_time         (current timestamp)
        |     +-> sleep            (suspend N seconds, interrupted by operator messages)
        |
        +-> Graceful shutdown (on first SIGINT)
              +-> Inject shutdown prompt
              +-> Agent saves state (up to 10 turns, 2min timeout)
```

## Module Details

### cmd/faultline/main.go

The composition root. It is the only place in the codebase that knows which adapter implements which port.

Responsibilities:
- Parse the `-config` flag, load TOML config.
- Build the `slog.Logger` (console at configured level, file at DEBUG with daily rotation).
- Set up two-phase shutdown via signal handling (first signal closes a channel; second cancels the parent context).
- Construct each concrete adapter (`fs.Store`, `bm25.Index`, `openai.Client`, `kobold.Client` with best-effort detection, `telegram.Bot` if configured, `docker.Sandbox` if enabled, `jsonfile.Persister`, `tools.Executor`).
- Wire them into `agent.New` via the `agent.Deps` struct.
- Defer Close on resources whose lifecycle outlives the agent loop (sandbox, chat log).
- Run the agent.

### internal/agent/

The hexagon. Pure domain logic with no I/O outside what the ports allow.

- **`agent.go`**: the `Agent` struct, `New()` constructor, `Run()` loop, context compaction (`compactContext`, `rebuildContext`), idle-loop detection (`idleNudgeThreshold`, `idleForceCompactionThreshold`, `idleNudgePrompt`), graceful save (`gracefulSave`), token estimation (`countMessageTokens` -- routes to Tokenizer when available, heuristic otherwise), backend-perf logging.

- **`ports.go`**: the interfaces the agent depends on. Adapters satisfy them structurally; nothing in `internal/adapters/...` imports `internal/agent`.

  | Port | Implemented by | Notes |
  |------|---------------|-------|
  | `ChatModel` | `*openai.Client` | One method: `Chat`. |
  | `Memory` | `*fs.Store` | Subset used by the agent loop. Tools deal with the richer fs.Store surface directly. |
  | `Searcher` | `*bm25.Index` | Build only; tools call into the same index for richer ops. |
  | `Operator` | `*telegram.Bot` | nil-allowed when no collaborator channel is configured. |
  | `Tokenizer` | `*kobold.Client` | nil-allowed; agent falls back to heuristic. Includes `Perf()` for context_status diagnostics; returns `*kobold.PerfInfo` (intentional small leak documented in port comment). |
  | `Tools` | `*tools.Executor` | ToolDefs, Execute, SetContextInfo, Close. |
  | `StateStore` | `*jsonfile.Persister` | Save/Load conversation log; binds path + logger at construction. |

### internal/config/

`config.Config` and its sub-structs (`APIConfig`, `AgentConfig`, `TelegramConfig`, etc.). `config.Load` reads a TOML file; `config.Default` returns sensible defaults. Includes a `duration` type that implements `encoding.TextUnmarshaler` so TOML strings like `"5m"` parse into `time.Duration`.

### internal/log/

- `log.Daily`: an `io.Writer` that auto-rotates to date-stamped files (`YYYY-MM-DD.log`). `log.NewDaily` and `log.NewDailyPrefixed` are the constructors. Thread-safe via mutex.
- `log.MultiHandler`: a `slog.Handler` that fans records to multiple handlers (console + file).

### internal/prompts/

- Embedded default prompts in `templates/*.md`, compiled into the binary via `//go:embed`.
- `prompts.Load(store, name)`: loads a single prompt, seeding the embedded default to the memory store on first run.
- `prompts.LoadAll(store)`: loads all five prompts.
- `prompts.Render(template, now)`: substitutes `{{TIME}}` placeholders.
- `prompts.BuildCycleContext(systemPrompt, memories, now, charLimit)`: assembles the full system message with recent memory excerpts and truncation hints.
- `prompts.Store` is a tiny interface (Read/Write) that `*fs.Store` satisfies structurally, so the prompts package doesn't import the memory adapter.

### internal/tools/

`tools.Executor` and all tool handlers. The largest package by line count.

- **`executor.go`**: `Executor` struct, `New()` constructor, `ToolDefs()` (tool registry advertised to the LLM), `Execute()` central dispatch, web fetching with custom HTML-to-markdown converter (~400 lines), `webCache` with TTL eviction, all `memory_*` / `sandbox_*` / utility (`get_time`, `sleep`, `send_message`, `context_status`) handlers.
- **`wiki.go`**: `wiki_fetch` tool (MediaWiki API + plain-text extraction + cache).
- **`html_test.go`**: HTML-to-markdown conversion tests.

The Executor depends on adapter packages directly (memory/fs, sandbox/docker, operator/telegram, llm/kobold, email/imap). The agent depends on the Executor through the `Tools` port.

### internal/search/bm25/

Pure-Go BM25 search index. `bm25.Index` (with `Build`, `Update`, `Remove`, `RemovePrefix`, `Search`) and `bm25.Result` (path/content/score). Standard k1=1.5, b=0.75. In-memory only -- rebuilt from disk on startup and after each context compaction.

`bm25.Result` is also reused by the memory adapter for non-scored returns (RecentFiles, AllFiles); Score is left at zero in those cases. This is a small shared type, not a violation of the dependency direction (memory imports bm25, never the reverse).

### internal/llm/

Shared LLM-shaped value types. The OpenAI chat-completions wire shape is treated as the de facto domain language for LLM messaging: every plausible backend speaks it. See `Standards/Hexagonal-Architecture.md` for the "pragmatic exception" justification.

- `Message`, `ToolCall`, `FunctionCall`, `Tool`, `FunctionDef`, `ChatResponse`, `Choice`.
- Role constants (`RoleSystem`, `RoleUser`, `RoleAssistant`, `RoleTool`) and `ToolTypeFunction`.
- `ChatRequest`: the agent-facing request type with sampler params (Temperature, TopP, TopK, MinP, RepetitionPenalty, etc.).
- `EstimateTokens`, `EstimateMessagesTokens`: heuristic ~4-chars-per-token fallback when no real tokenizer is available.

### internal/adapters/llm/openai/

`openai.Client` is a hand-rolled HTTP client for `/v1/chat/completions`. We previously used `github.com/sashabaranov/go-openai` but dropped it because it has no public API for vendor-specific extras (`top_k`, `min_p`, `repetition_penalty`).

- `chatRequestWire` is the JSON shape POSTed; the agent never sees it.
- Vendor extension fields ride alongside the spec fields with `omitempty`; servers that don't recognize them silently ignore them.
- Bearer token set when configured; no global HTTP timeout (long generations are legitimate; cancellation is via the caller's context).
- `ChatLogger` lives here too: human-readable transcript of every request/response, separate from the slog debug log. One file per day (`chat-YYYY-MM-DD.log`). Nil-safe so callers can omit the chat logger.

### internal/adapters/llm/kobold/

`kobold.Client` for KoboldCpp-specific endpoints (`/api/extra/version`, `/api/extra/tokencount`, `/api/extra/abort`, `/api/extra/perf`).

- `Detect()` probes `/api/extra/version` at startup with a 5s timeout. Failure leaves the client unusable; main.go discards it and the agent falls back to heuristics.
- `CountString` / `CountMessages` use the real tokenizer plus a small per-message overhead constant (`chatTemplateOverhead = 10`) to approximate chat-template scaffolding.
- `Abort()` lets forced-shutdown actually stop generation server-side.
- `Perf()` returns `*PerfInfo` consumed by `context_status` and the agent's `logBackendPerf`.
- Nil-safe on `Detected()` and `Version()` so callers can skip explicit nil checks.
- `StopReasonString(int) string` maps integer stop codes to human labels.

### internal/adapters/memory/fs/

`fs.Store` is the file-based memory store.

- All paths lowercased, cleaned, and verified to not escape the base directory. `.md` extension is auto-appended.
- Soft delete via `.trash/` under the memory root; supports restore and empty trash.
- Operations: `Read`, `ReadLines`, `Write`, `Edit` (find-and-replace, optional replace-all), `Append`, `Insert`, `Delete`, `Restore`, `Move`, `List`, `Grep`, `AllFiles`, `RecentFiles`, `Stat`, `DirSize`, `ListTrash`, `EmptyTrash`.
- Thread safety via the filesystem (no explicit mutexes).
- `IsTrashPath(path) bool` is exported for the agent's `isOperationalFile` check.

### internal/adapters/operator/telegram/

`telegram.Bot` for bidirectional collaborator communication.

- Long-polls for updates via `GetUpdatesChan()`. Only accepts messages from the configured chat ID.
- Outgoing messages use markdown-to-Telegram-MarkdownV2 conversion (`goldmark-tgmd`); auto-chunks at 4000 chars.
- `Pending()` drains the queue atomically; `HasPending()` peeks without draining (used by the `sleep` tool to wake on operator input without stealing the message from the agent's normal between-turn drain).

### internal/adapters/sandbox/docker/

`docker.Sandbox` for Docker-backed Python execution.

- Flat directory layout: `scripts/`, `input/`, `output/` plus a seeded `pyproject.toml`.
- Ephemeral containers per operation (`docker run --rm`). Host UID/GID mapping for file ownership.
- Filenames validated against a strict regex (`^[a-z0-9][a-z0-9._-]*$`).
- Network access toggleable; memory limits enforced.
- `uv` for fast Python package management. Install/upgrade/remove tracked in `pyproject.toml`.
- Execution log written to `sandbox-YYYY-MM-DD.log` (separate from the main slog stream).

### internal/adapters/email/imap/

`imap.Client` wraps a go-imap dialer. Connections are short-lived: `New()` -> `FetchOverviews` or `FetchBody` -> `Close()`. The `email_fetch` tool handler in tools.go creates one per request.

### internal/adapters/state/jsonfile/

- `Save(path, messages, idleStreak)` and `Load(path, logger)`: package functions that do the actual filesystem work. Atomic writes via temp file + rename. Bad files are quarantined with a `.bad-<unix>` suffix on parse error or version mismatch.
- `Persister`: small wrapper that binds path + logger at construction so the agent can call `Save`/`Load` through the `StateStore` port without re-passing them.
- `sanitizeMessages`: strips trailing partial turns (assistant messages with unsatisfied `tool_call_id`s) so the resumed log is always a valid replay.

## Key Design Patterns

1. **Hexagonal architecture (ports & adapters)**: see `Standards/Hexagonal-Architecture.md` in the user's vault. The agent depends only on interfaces it owns; adapters implement those interfaces structurally.

2. **Continuous autonomous operation**: the agent runs indefinitely. There is no request-response cycle.

3. **Context compaction**: when the conversation grows too large, the agent saves state and summarizes; context is rebuilt from system prompt + summary + fresh memories. This enables indefinite operation within a fixed context window.

4. **Self-modifying prompts**: the agent can read and rewrite its own system/operational prompts via the memory tools. Changes take effect on the next context rebuild.

5. **Two-phase shutdown**: first signal triggers graceful state-saving via the Tools port (model gets up to 10 turns, 2 min); second signal forces immediate exit.

6. **Cooperative collaborator handoff**: incoming Telegram messages never cancel an in-flight LLM request. The agent finishes its current thought, then on the next opportunity (after a text response, or as a deferral of tool calls) the collaborator message is injected.

7. **Soft delete with trash**: memory files move to `.trash/` on delete, restorable until `EmptyTrash`.

8. **De facto wire shape as domain type**: the OpenAI chat-completions Message/Tool/ToolCall shapes live in `internal/llm/` and are used end-to-end. The pragmatic exception is documented in the architecture standard. This is honest, not a leaky abstraction: every plausible backend speaks this shape.

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/BurntSushi/toml` | TOML config |
| `github.com/go-telegram-bot-api/telegram-bot-api/v5` | Telegram bot API |
| `github.com/Mad-Pixels/goldmark-tgmd` | Markdown -> Telegram MarkdownV2 |
| `github.com/BrianLeishman/go-imap` | IMAP client |
| `golang.org/x/net/html` | HTML parsing for web_fetch |
| `github.com/yuin/goldmark` | Markdown (indirect, used by goldmark-tgmd) |

The OpenAI-compatible chat completions client is hand-rolled in `internal/adapters/llm/openai/` (no SDK).

## Runtime Dependencies

- **Docker**: required for the sandbox feature. Must be in PATH.
- **Network access**: required for the LLM API, web fetching, Telegram, IMAP.

## Configuration

A heavily commented example config lives at `config.example.toml` in the
repository root. Copy it to your deployment as `config.toml` and edit. The
embedded `config.Default()` returns the same values; `config.Load()`
overlays anything set in the TOML file on top.

## Contributing

### Commit messages: Conventional Commits

Faultline uses [Conventional Commits](https://www.conventionalcommits.org/)
because release-please derives version bumps and changelog entries from
the commit log on `main`.

The convention is `type(scope)?: short subject`. Common types:

| Type | Effect on version | Used for |
|------|-------------------|----------|
| `feat:` | Minor bump (1.2.0 -> 1.3.0) | New user-facing feature |
| `fix:` | Patch bump (1.2.0 -> 1.2.1) | Bug fix |
| `feat!:` or footer `BREAKING CHANGE:` | Major bump (1.2.0 -> 2.0.0) | Backwards-incompatible change |
| `docs:` | No bump | Documentation only |
| `refactor:` | No bump | Code restructuring with no behavior change |
| `test:` | No bump | Test-only changes |
| `chore:` | No bump | Build, deps, repo housekeeping |
| `ci:` | No bump | CI/release workflow changes |

The simplest discipline for keeping `main`'s commit log clean:

1. Open a PR with whatever messy commits you like during development.
2. Set the PR title to a single conventional-commit subject (e.g.
   `feat: add memory_grep -B/-A context flags`).
3. Squash-merge. GitHub uses the PR title as the squashed commit subject,
   so `main` ends up with one tidy conventional commit per PR.

### Tests

Tests exist for `config`, `log`, `prompts`, `llm`, `bm25`, `openai`
(chatlog), `kobold`, `memory/fs`, `operator/telegram`, `state/jsonfile`,
and `tools` (HTML conversion + path validation). The `agent` package has
no tests yet -- adding test seams is straightforward thanks to the ports.

### Other notes

- The HTML-to-markdown converter in `internal/tools/executor.go` is
  substantial (~400 lines) and handles most common HTML elements. Not a
  third-party library.
- The `webCache` runs a background goroutine for eviction; it is stopped
  via `Close()` when the `tools.Executor` is closed.
- The default API URL in `config.Default()` points to a local network
  address -- change it for your setup, or pass a populated `config.toml`.
