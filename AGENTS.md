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
                              Tokenizer, Tools, StateStore, Skills,
                              Subagents
    config/                   TOML config loading
    log/                      DailyFileWriter, MultiHandler (slog)
    prompts/                  embedded prompt templates, Load, LoadAll,
                              Render, BuildCycleContext
      templates/              the embedded *.md defaults
    tools/                    tool registry & dispatcher
      executor.go             Executor + all tool handlers
      vector.go               Embedder interface + per-mutation reindex helpers
      wiki.go                 wiki_fetch tool
      html_test.go            HTML-to-markdown converter tests
    search/bm25/              pure-Go BM25 search index
    search/vector/            pure-Go in-memory vector index + binary
                              serialization (FVEC v1) for semantic search
    skills/                   domain types for Agent Skills (Skill, Catalog,
                              Validate*) — adapter is in adapters/skills/fs
    subagent/                 domain types + Manager for subagent delegation
                              (Profile, Report, ActiveStatus); spawnFn
                              bridge to composition lives in cmd/faultline
    llm/                      shared LLM types (Message, Tool, ChatRequest, ...)
                              + heuristic token estimator
    adapters/
      llm/openai/             OpenAI-compatible HTTP client + ChatLogger
      llm/kobold/             KoboldCpp extras (tokencount, abort, perf)
      embeddings/openai/      OpenAI-compatible /v1/embeddings client
      memory/fs/              filesystem-backed Memory adapter
      operator/telegram/      Telegram bot for collaborator messaging
      sandbox/docker/         Docker-based multi-runtime sandbox
                              (image: docker/sandbox/Dockerfile, Arch-based,
                              ships uv/python/node/bun/deno/go + CLI tools)
      skills/fs/              filesystem-backed Agent Skills catalog
                              (https://agentskills.io); reads
                              <root>/<name>/SKILL.md
      email/imap/             IMAP email client
      state/jsonfile/         JSON-file conversation persistence (Save/Load
                              + Persister wrapper that satisfies StateStore)
      auth/users/             argon2id password hashing, users.toml load/save
                              with first-run admin bootstrap, in-memory
                              session store, CSRF helpers — admin UI
                              authentication primitives
      admin/http/             embedded HTTP admin UI (HTMX + DaisyUI),
                              login/logout/dashboard skeleton; static assets
                              and html/template files embedded via go:embed
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
        |     +-> memory_*         (fs.Store; mutations also re-embed
        |                            into vector.Index when enabled)
        |     +-> memory_search    (bm25.Index, plus vector.Index when
        |                            embeddings configured: dual sections)
        |     +-> send_message     (operator port: telegram.Bot)
        |     +-> sandbox_*        (docker.Sandbox)
        |     +-> skill_activate   (skills/fs.Store; loads SKILL.md body)
        |     +-> skill_read       (skills/fs.Store; reads bundled resource)
        |     +-> skill_execute    (docker.Sandbox.ExecuteIsolated with
        |                            /skill ro + per-call /work rw + /cache)
        |     +-> skill_work_read  (reads files from a previous skill_execute's
        |                            /work directory by call_id)
        |     +-> skill_install    (optional; fetches tarball/git URL into
        |                            skills root, validates SKILL.md, runs
        |                            audit subagent (when [subagent] enabled,
        |                            fail-closed on DENY), reloads catalog.
        |                            Gated on [skills] install_enabled)
        |     +-> email_fetch      (short-lived imap.Client per call)
        |     +-> context_status   (token usage + kobold.Client.Perf if detected)
        |     +-> get_time         (current timestamp)
        |     +-> sleep            (suspend N seconds, interrupted by operator messages)
        |     +-> rebuild_indexes  (force full rebuild of bm25 and/or vector index;
        |                            shared helper in tools/vector.go also drives
        |                            startup reconcile from main.go)
        |     +-> subagent_run     (sync; subagent.Manager.Run; child agent loop
        |                            via spawnFn closure in cmd/faultline)
        |     +-> subagent_spawn   (async; returns work_id; report lands in
        |                            inbox alongside operator queue)
        |     +-> subagent_wait    (block until named subagent reports;
        |                            wakes on operator HasPending)
        |     +-> subagent_status  (active children with elapsed/prompt preview)
        |     +-> subagent_cancel  (cooperative cancel via child ctx)
        |     +-> subagent_report  (child-only; sink closure captures text +
        |                            calls childAgent.RequestStop)
        |
        +-> Graceful shutdown (on first SIGINT)
              +-> Subagents.CancelAll (children's goroutines unwind)
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
  | `Skills` | `*skillsfs.Store` | nil-allowed; List/Reload for the tier-1 catalog injection in `BuildCycleContext`. Tools layer drives the rest. |
  | `Subagents` | `*subagent.Manager` | nil-allowed; Pending/HasPending drive the inbox drain in `injectPendingMessages` (parallel to operator), Profiles feeds the system-prompt catalog, CancelAll fires from `gracefulSave`. Tools layer holds the same pointer for `subagent_run/spawn/wait/status/cancel`. |

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
- **`chunk.go`**: paragraph-aware splitter (`splitIntoUnits`), unit-key encoding helpers (`unitKey`, `pathFromUnitKey`, `chunkIdxFromUnitKey`). `maxParagraphBytes = 3000` is the only fallback cap — applied only when a single paragraph exceeds it, in which case it's byte-cut into sequential pieces.
- **`vector.go`**: `Embedder` interface (consumer-side; defined here rather than in `agent/ports.go` because the agent loop never embeds), per-mutation paragraph-aware reindex helpers (`reindexVector`, `removeVector`, `removeVectorPrefix`, `renameVector`, `reindexVectorDir`), path-exclusion logic (skip `prompts/` and `.trash/`), the adaptive batcher `embedWithAdaptiveBatching` (halves batch size on failure, grows back after 5 success streak, skips single-input failures), `fileLevelHits` for collapsing chunk-level search hits to one-per-file, dual-mode `memory_search` description selector, and the bulk reconcile/rebuild entry points (`ReconcileVectorIndex`, `RebuildVectorIndex`) used by both the startup reconcile in `cmd/faultline/embeddings.go` and the `rebuild_indexes` tool. The mutation hooks called from `executor.go` are no-ops when the feature is disabled, so the dispatcher code is identical regardless of operator config.
- **`wiki.go`**: `wiki_fetch` tool (MediaWiki API + plain-text extraction + cache).
- **`html_test.go`**: HTML-to-markdown conversion tests.

The Executor depends on adapter packages directly (memory/fs, sandbox/docker, operator/telegram, llm/kobold, email/imap). The agent depends on the Executor through the `Tools` port.

### internal/search/bm25/

Pure-Go BM25 search index. `bm25.Index` (with `Build`, `Update`, `Remove`, `RemovePrefix`, `Search`) and `bm25.Result` (path/content/score). Standard k1=1.5, b=0.75. In-memory only.

Lifecycle:
- **Built from disk on startup** in `agent.initializeContext` via `Build(memory.AllFiles())`.
- **Rebuilt after every context compaction** in `agent.rebuildContext` (same `Build` call) — guards against drift accumulating across long-running sessions.
- **Updated incrementally on every memory mutation** (`memory_write`, `memory_edit`, `memory_append`, `memory_insert`, `memory_delete`, `memory_move`, `memory_restore`) by the tool dispatcher. The index is always in sync with on-disk memory between rebuilds.
- **Force-rebuildable on demand** via the `rebuild_indexes` tool, which the LLM is told to use only when the operator asks or when it observes inconsistency between search results and known disk state.

`bm25.Result` is also reused by the memory adapter for non-scored returns (RecentFiles, AllFiles); Score is left at zero in those cases. This is a small shared type, not a violation of the dependency direction (memory imports bm25, never the reverse).

### internal/search/vector/

Pure-Go in-memory vector index keyed by string paths. `vector.Index` provides `Upsert`, `Remove`, `RemovePrefix`, `Rename`, `Search`, plus chunk-aware variants `RemoveChunks` / `RenameChunks` / `HasChunks` for paragraph-keyed entries (`path#N`) and `Save`/`Load` against a custom binary format ("FVEC v1") so embeddings persist across restarts.

- Brute-force cosine similarity over a `map[string][]float32`. At Faultline's scale (low-thousands of files, 1536-dim vectors) flat scan is sub-millisecond; HNSW/IVF was ruled out as overkill.
- Vectors are L2-normalised on Upsert so Search can use plain dot products.
- Keys are strings: bare `path` for single-paragraph files, `path#N` for chunked. The `#`-separator is unambiguous because the memory path validator restricts segments to `[a-z0-9.-]`. Dedup of paragraph-level results down to file level happens in the tools layer (`fileLevelHits`), not in the index.
- Binary format: `magic[4]="FVEC" | version[4] | dim[4] | count[4] | model_len[2] | model[N] | records[count]{path_len[2], path[N], vec[dim*4]}`. Deterministic ordering (keys sorted on save) for byte-stable diffs in tests.
- `ErrModelMismatch` returned by `Load` when the on-disk dim or model differs from what the in-memory index was constructed for; caller is expected to `Reset` and re-embed.
- `ErrCorrupt` returned for unreadable / wrong-magic / inconsistent files; caller is expected to rename the file aside (mirroring the `state/jsonfile` pattern) and continue with an empty index.
- `Dirty()` is an atomic the persistence loop polls; cleared by `Save`.

### internal/adapters/embeddings/openai/

`openai.Client` is a hand-rolled HTTP client for `/v1/embeddings`. Deliberately separate from `internal/adapters/llm/openai/` even though both target OpenAI-compatible servers — different endpoint, different request/response shape, different failure modes.

- `New(url, apiKey, model, timeout, logger)` constructs the client; `Probe(ctx)` sends a single-input embed call to discover the model's dim. `Dim()` returns 0 until probe succeeds.
- `Embed(ctx, []string)` returns one vector per input in input order. Defensive `sort.Slice` on `index` field of the response so order is preserved even with non-OpenAI servers that return out of order.
- Per-call timeout is applied via a child context derived from the caller's `ctx`, not from `http.Client.Timeout`, so callers can use a long ambient context for batch operations and still bound individual calls.
- Probe failure is **not** fatal at startup; main.go logs the error loudly and continues with semantic search disabled. The agent runs fine without it.

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

`docker.Sandbox` for Docker-backed script execution. The default image (`ghcr.io/matjam/faultline-sandbox`, built from `docker/sandbox/Dockerfile`) is a multi-runtime Arch-based image with Python+pip, uv+uvx, Node+npm+npx, Bun, Deno, Go, and common CLI tools (curl, jq, ripgrep, fd, git, ...). Any image with `sh` and `uv` on PATH satisfies the adapter's contract; the image is configurable per deployment.

- Flat directory layout: `scripts/`, `input/`, `output/` plus a seeded `pyproject.toml`.
- Ephemeral containers per operation (`docker run --rm`). Every run passes `--user <host_uid>:<host_gid>` for file ownership and `--security-opt no-new-privileges` to block setuid escalation. The image's Dockerfile additionally bakes in `USER 65532:65532` as a defense-in-depth fallback; if `--user` is ever omitted by a future code path, the container still refuses to run as root. The agent itself refuses to start as root (`os.Getuid()==0` is fatal in `cmd/faultline/main.go` and again in `sandbox.New`).
- Filenames validated against a strict regex (`^[a-z0-9][a-z0-9._-]*$`).
- Network access toggleable; memory limits enforced.
- `sandbox_execute` drives Python via `uv` (`uv sync && uv run python /scripts/X`); `sandbox_shell` runs arbitrary `sh -c` commands so the agent can drive any other runtime on PATH directly.
- Install/upgrade/remove tracked in `pyproject.toml` for the Python project; non-Python languages live entirely inside the container at runtime.
- Execution log written to `sandbox-YYYY-MM-DD.log` (separate from the main slog stream).
- `ExecuteIsolated(ctx, command, mounts, network, cwd, env)` is the building block for skill execution: a fresh `docker run --rm` with only the supplied bind-mounts (no `/scripts`, `/input`, `/output`, `/venv`, `/pyproject.toml`), respecting the same `--user UID:GID`, memory, and timeout settings as `Execute`/`ShellExec`. The skill_* tools use this with `/skill` (ro), `/cache` (rw), and a per-call `/work` (rw).
- `SkillWorkRoot()` and `ResetSkillWork()` manage the per-call scratch root at `<sandbox-dir>/skill-work/`. The composition root calls `ResetSkillWork` at startup so stale `work_id`s from a previous session can't resolve.

### internal/adapters/skills/fs/

`fs.Store` is the filesystem-backed Skills adapter; satisfies the agent's `Skills` port plus extra methods used by the tools layer (`Get`, `Read`, `Resources`).

- Discovery walks `<root>/<name>/SKILL.md` at depth 2; non-skill subdirectories (no SKILL.md) and dotfile dirs (`.git`, etc.) are ignored.
- Frontmatter parser uses `gopkg.in/yaml.v3` with a one-shot lenient retry that quotes unquoted-colon values (the spec's most common authoring mistake). Strict parse fails with no retry success surface as warnings; the skill is dropped.
- Lenient validation: name mismatch with directory → use directory name (with diagnostic); over-length name/description/compatibility → diagnostic, still loaded; missing description → hard skip.
- `Resources(name)` enumerates files under conventional `scripts/`, `references/`, `assets/` subdirs (excluding dotfiles), capped at `MaxResourceListing` (50) entries. Used by `skill_activate` to surface bundled resources without eagerly reading them.
- `Read(name, relPath)` reads a single resource file; the relative path is rejected if it's absolute or if `filepath.Rel` resolves outside the skill directory.
- `Reload()` is best-effort: per-skill parse errors log and drop the offending skill but never fail the whole catalog. The agent calls `Reload` on every context rebuild so operator-dropped skills become visible without a restart.

### internal/adapters/email/imap/

`imap.Client` wraps a go-imap dialer. Connections are short-lived: `New()` -> `FetchOverviews` or `FetchBody` -> `Close()`. The `email_fetch` tool handler in tools.go creates one per request.

### internal/adapters/state/jsonfile/

- `Save(path, messages, idleStreak)` and `Load(path, logger)`: package functions that do the actual filesystem work. Atomic writes via temp file + rename. Bad files are quarantined with a `.bad-<unix>` suffix on parse error or version mismatch.
- `Persister`: small wrapper that binds path + logger at construction so the agent can call `Save`/`Load` through the `StateStore` port without re-passing them.
- `sanitizeMessages`: strips trailing partial turns (assistant messages with unsatisfied `tool_call_id`s) so the resumed log is always a valid replay.

### internal/update/

Self-update orchestration. Polls the configured GitHub repo's `releases/latest`, compares versions, downloads the matching tarball, verifies it against `SHA256SUMS`, swaps the binary in place, and triggers graceful shutdown so the new binary takes over.

- **`Updater`**: main type. Constructed once in `cmd/faultline/main.go`, shared with the tools package via `Check` / `Apply` methods so the `update_check` / `update_apply` tools can drive the same pipeline.
- **`Run(ctx)`**: background polling loop. First check delayed 30s to avoid hammering GitHub on tight restart loops; subsequent checks on `cfg.CheckInterval`. No-op when `cfg.Enabled` is false.
- **`Apply(ctx)`**: downloads, verifies SHA256, extracts the binary from the tarball, atomically swaps (rotating old to `<binary>.previous` as a one-deep rollback slot), records to `meta/version-history.md`, and calls the `TriggerShutdown` callback. Serialized by an internal mutex; an `applied` atomic flag prevents repeat applies after one has succeeded.
- **`Result`**: the value `TriggerShutdown` receives — used by `cmd/faultline/main.go` to decide whether to `os.Exit(0)`, `syscall.Exec` the new binary, or run a configured restart command after `Agent.Run` returns.
- Asset selection follows goreleaser's name template: `faultline_<version>_<os>_<arch>.tar.gz`, with `amd64` rewritten to `x86_64` to match the Linux convention.
- `IsNewer` / `IsPrerelease` use `golang.org/x/mod/semver` for tag comparison; non-semver "current" (e.g. `dev` from a local build) is treated as the oldest possible version, so dev builds upgrade to real releases when self-update is enabled.

### internal/adapters/auth/users/

Local-auth primitives consumed by the admin HTTP adapter. No domain dependency; the agent loop knows nothing about users or sessions.

- **`argon2.go`**: `HashPassword` / `VerifyPassword`. argon2id, OWASP-ish defaults (`m=64 MiB, t=3, p=4, keylen=32, saltlen=16`). PHC string format on disk so params are self-describing — changing the constants does not invalidate existing hashes. Constant-time compare in Verify.
- **`users.go`**: `Store` (TOML-file-backed user list under a mutex). `New(path)` returns a `*BootstrapResult` exactly once on the first call against a missing file, with a freshly generated 24-char password drawn from a confusable-free alphabet. The plaintext is also written into the file as a comment so the operator can recover it after the log line scrolls. File perms are `0600`. `Verify` runs a dummy hash against an unknown-user lookup so timing does not betray account existence.
- **`session.go`**: `SessionStore` is in-memory only; restarts evict everything. Janitor goroutine evicts idle sessions on a 1-minute tick; `Get` also evicts eagerly when called past TTL. `NewToken` is exported as a small primitive the admin adapter reuses for the pre-session login-form CSRF token. `VerifyCSRF` does the constant-time compare.

### internal/adapters/admin/http/

Embedded HTTP admin UI driving adapter (stage-2 skeleton). Stdlib `net/http` only — no chi/gin/echo — to match the project's minimal-deps posture. Bound to a loopback address by default; reverse-proxy TLS termination is the documented path for remote access. There is no built-in TLS.

- **`server.go`**: `Server`, `New`, `Run`, `Shutdown`, route registration. Each (layout, content) pair is parsed once at startup into its own `*template.Template` so the `{{define "content"}}` blocks across content files do not collide as they would in a single ParseFS-parsed set.
- **`middleware.go`**: `requireAuth` (session lookup + CSRF check on non-safe methods), `requestLogger` (per-request structured log line; static-asset paths demoted to debug). CSRF is enforced on every `POST` / `PUT` / `DELETE` etc. via the `_csrf` form field compared against `Session.CSRFToken`.
- **`handlers_auth.go`**: `GET/POST /admin/login`, `POST /admin/logout`. Login uses a separate short-lived `faultline_login_csrf` cookie because there is no session yet at that point; the cookie is rotated on every form render and cleared on successful login. Failed logins return a generic message and an HTTP 401, never disclosing whether the username existed.
- **`assets/`**: vendored `htmx.min.js` (2.0.9), `tailwind.js` (`@tailwindcss/browser@4`, in-browser JIT), `daisyui.css` and `daisyui-themes.css` (5.5.19). All embedded via `go:embed`. Tailwind in the browser was chosen over a build-step CLI because the admin UI is low-traffic and avoiding a Node-flavored toolchain was an explicit requirement.
- **`templates/`**: `layout.html`, `login.html`, `dashboard.html`. Embedded via `go:embed`.

Composition wiring lives in `cmd/faultline/admin.go`. The admin server runs under the parent process context: first SIGINT closes `shutdownCh` (agent saves state) but the admin server stays up; second SIGINT cancels the parent context and the server shuts down. After the agent loop returns, `main.go` cancels the context explicitly so the admin server unblocks for clean exit.

In stage 2 the admin server has no view of agent internals — that comes in stage 3+ via the `AgentInspector` / `SubagentInspector` / `ToolObserver` ports.

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
| `golang.org/x/crypto/argon2` | argon2id password hashing (admin UI auth) |
| `github.com/yuin/goldmark` | Markdown (indirect, used by goldmark-tgmd) |

The OpenAI-compatible chat completions client is hand-rolled in `internal/adapters/llm/openai/` (no SDK). The admin UI's HTMX, DaisyUI, and Tailwind-browser assets are vendored under `internal/adapters/admin/http/assets/` and embedded via `go:embed`; no Node toolchain is involved at build or run time.

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
| `feat!:` or footer `BREAKING CHANGE:` | Major bump (1.2.0 -> 2.0.0) | Backwards-incompatible change. **Ask the maintainer first.** |
| `docs:` | No bump | Documentation only |
| `refactor:` | No bump | Code restructuring with no behavior change |
| `test:` | No bump | Test-only changes |
| `chore:` | No bump | Build, deps, repo housekeeping |
| `ci:` | No bump | CI/release workflow changes |

**Major bumps are a maintainer decision, not an automatic one.** Even if a
change is technically backwards-incompatible, the agent / contributor
opening a PR should not unilaterally tag it `feat!:` or attach a
`BREAKING CHANGE:` footer. Ask first. The maintainer accumulates
breaking changes and decides when there's enough to warrant a major
release. To force an explicit version when one is wanted, the
maintainer can add a `Release-As: 2.0.0` footer to a commit; release-please
honours it on the next run.

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
