# Faultline

An autonomous AI agent daemon written in Go. Faultline runs as a persistent, long-lived process that continuously interacts with an LLM via an OpenAI-compatible API. It learns about the world by browsing the web, persists knowledge in a file-based memory system, communicates with a human collaborator via Telegram, and can execute Python scripts in a sandboxed Docker environment.

The agent can modify its own operating prompts, enabling self-directed behavioral evolution over time. With auto-update enabled, the daemon also keeps its own binary current — polling GitHub releases, verifying checksums, atomically swapping in new versions, and restarting cleanly. See [Auto-update](#auto-update) below.

## Requirements

- Go 1.26+ (only needed if building from source)
- Docker (optional; for the Python sandbox feature)
- A Telegram bot token (optional; for collaborator communication)
- An OpenAI-compatible API endpoint (local or remote)

## Installation

Pre-built binaries are published on every tagged release for `linux/amd64`, `linux/arm64`, and `darwin/arm64`.

```sh
# Pick the right tarball for your platform from the latest release at
# https://github.com/matjam/faultline/releases/latest
curl -L -O https://github.com/matjam/faultline/releases/latest/download/faultline_<version>_linux_x86_64.tar.gz
curl -L -O https://github.com/matjam/faultline/releases/latest/download/SHA256SUMS

# Verify
sha256sum -c SHA256SUMS --ignore-missing

# Extract and install
tar xzf faultline_<version>_linux_x86_64.tar.gz
sudo install faultline /usr/local/bin/        # or wherever you prefer
```

The release tarball also contains `LICENSE`, `README.md`, `AGENTS.md`, and `config.example.toml`.

Once installed, enable [Auto-update](#auto-update) and the daemon will pick up new releases automatically — no need to repeat this manual install for every version.

## Building from source

```sh
go build -o faultline ./cmd/faultline
```

For a build with version metadata baked in (matching what release builds embed):

```sh
go build \
  -ldflags="-X github.com/matjam/faultline/internal/version.Version=$(git describe --tags --always) \
            -X github.com/matjam/faultline/internal/version.Commit=$(git rev-parse --short HEAD) \
            -X github.com/matjam/faultline/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o faultline ./cmd/faultline
```

The binary embeds the default prompt templates from `internal/prompts/templates/` at compile time.

Check what version a binary reports:

```sh
./faultline -version
```

## Configuration

Faultline reads a TOML config file (default: `./config.toml`). Missing fields fall back to sensible defaults.

The repository includes [`config.example.toml`](config.example.toml) — a heavily-commented copy of every section with inline documentation for each field. Copy it to your deployment as `config.toml` and edit. The same defaults are returned by `config.Default()` in code.

Key sections:

- `[api]` — LLM endpoint URL, key, model, KoboldCpp auto-detection.
- `[agent]` — memory directory, context limits, sampler parameters, state persistence, sleep cap.
- `[telegram]` — optional collaborator messaging.
- `[log]` — console level + log directory.
- `[sandbox]` — optional Python execution via Docker.
- `[email]` — optional IMAP email reading.
- `[limits]` — content-size caps for memory excerpts, search results, sandbox output.

## Running

```sh
./faultline -config ./config.toml
```

The agent runs continuously until interrupted. Shutdown behavior:

- **First SIGINT/SIGTERM**: triggers graceful shutdown. The agent gets up to 10 turns (2-minute timeout) to save state to memory.
- **Second SIGINT/SIGTERM**: forces immediate exit.

Under a process supervisor (systemd, Docker, Kubernetes), the first signal is sufficient for clean rolling restarts.

## Auto-update

When `[update]` is enabled in `config.toml`, a background goroutine polls GitHub releases on a configured interval. If a newer version is available, the updater downloads the matching release tarball, verifies it against the published `SHA256SUMS`, atomically swaps the binary in place (keeping the old binary as `<binary>.previous` for one-deep rollback), and triggers graceful shutdown so the new binary takes over. Disabled by default; opt in with `enabled = true`.

The LLM does not decide whether to update. The agent has `update_check`, `update_apply`, and `get_version` tools that kick off the same code path, so the operator can say "update yourself" via Telegram, but the actual decision logic is in code.

Three restart modes — pick whichever matches how your deployment runs:

| `restart_mode` | What happens after the swap | Use when |
|----------------|------------------------------|----------|
| `exit` *(default)* | Save state and `os.Exit(0)`. Supervisor respawns the unit. | systemd, Docker, Kubernetes, runit, supervisord — anything with `Restart=always`. |
| `self-exec` | Save state and `syscall.Exec` the new binary, replacing the current process image. Same PID. | Bare-process runs without a supervisor (tmux, screen, manual `./faultline`). |
| `command` | Save state, run a configured `restart_command` detached, exit. | Custom orchestrators. |

On every successful update the agent appends an entry to `meta/version-history.md` in its memory store, so post-restart it can discover that it just updated by reading its own memory.

See the `[update]` section in [`config.example.toml`](config.example.toml) for every knob.

## Features

### Persistent Memory

The agent stores knowledge as markdown files in a configurable directory. All file paths are case-insensitive and auto-appended with `.md`. The memory system supports read, write, edit, append, insert, delete (soft, to `.trash/`), restore, move, list, grep, and full-text search.

### BM25 Search

An in-memory BM25 search index is built from all memory files on startup and rebuilt during context compaction. The agent uses this to find relevant memories by keyword.

### Semantic Search (optional)

When `[embeddings]` is configured with an OpenAI-compatible endpoint and model, Faultline embeds every memory file (excluding `prompts/` and `.trash/`) into an in-memory vector index, persisted to `<memory>/.vector/index.bin` in a custom binary format so embeddings aren't recomputed on restart.

**Paragraph-aligned chunking.** Files are split on blank lines and each paragraph is embedded as its own unit (keyed `path#0`, `path#1`, ...). Single-paragraph files keep the legacy bare-`path` shape. The only safety cap is a per-paragraph byte limit (3000 bytes) that triggers a byte-cut for the rare giant paragraph (one-line file, an unbroken code block, etc.) — paragraph boundaries are otherwise honoured exactly as the operator wrote them, so search snippets are semantically clean sections rather than arbitrary chunks.

**Adaptive batching.** The embedder calls the API in batches sized by `[embeddings].batch_size`. On batch failure (e.g. a server with a tighter physical batch limit, an oversized paragraph, transient errors) the batch size halves and retries; after 5 consecutive successful batches it doubles back toward the configured ceiling. A failure that persists down to batch size 1 means a single paragraph the server can't accept — that paragraph is logged and skipped, and the rest of the indexing pass continues. Skipped paragraphs don't appear in semantic search but BM25 still finds the parent file.

**Dual-section search.** `memory_search` returns BOTH lexical (BM25) and semantic results in clearly labeled sections per query. Semantic results are deduped to one entry per file (best-scoring paragraph wins) and the snippet shown is the matched paragraph itself, not the whole file — so the LLM gets the relevant section directly. When embeddings are disabled, `memory_search` falls back to BM25-only output.

**Defaults and cost.** Default model is `text-embedding-3-small` (1536 dim, ~$0.02/1M tokens — indexing 10k typical memory files is ~$0.10 one-time). Works with OpenAI, Ollama, LM Studio, vLLM, llama.cpp's embedding server, anything speaking the same wire shape. If you change the embedding model, the on-disk index records the prior model name and is automatically discarded and rebuilt on next startup.

### Web Browsing

The agent can fetch web pages, which are converted from HTML to readable markdown text. Results are cached with a TTL to avoid redundant fetches. Long pages can be paginated with offset/length parameters.

A separate `wiki_fetch` tool pulls plain-text article extracts directly from the MediaWiki API for cheap Wikipedia reads — no HTML parsing, much smaller token footprint than `web_fetch` for the same article.

### Context Compaction

When the conversation grows beyond a configurable token threshold, the agent is asked to save its current state to memory and produce a summary. The context is then rebuilt from the system prompt, recent memories, and the summary, allowing indefinite operation.

### KoboldCpp Extras (optional)

When the configured API endpoint is detected to be [KoboldCpp](https://github.com/LostRuins/koboldcpp), Faultline uses three native endpoints that sit alongside the OpenAI compatibility layer:

- **Real tokenization** via `/api/extra/tokencount` for compaction decisions, instead of the 4-chars-per-token heuristic. The heuristic under-counts code/JSON heavily, so without this the agent can be running 30-40% over its self-reported token usage by the time compaction triggers.
- **Generation aborts** via `/api/extra/abort` on forced shutdown, so the model actually stops generating instead of leaving the GPU/CPU pinned until the backend notices the client is gone.
- **Backend perf metrics** via `/api/extra/perf` surfaced in the `context_status` tool: last call's input/output tokens, eval speed, total generations, queue depth, uptime.

Detection is best-effort and bounded by a 5s timeout at startup. If the backend isn't KoboldCpp (real OpenAI, vLLM, llama.cpp's openai endpoint, etc.) detection fails silently and Faultline falls back to the heuristic with no other behavioural changes. Set `kobold_extras = false` in `[api]` to skip detection entirely.

### Self-Modifying Prompts

The agent's operating prompts (`system.md`, `compaction.md`, `cycle-start.md`, `continue.md`, `shutdown.md`) are mutable files in the memory store. The agent can read and rewrite them, changing its own behavior across context compactions.

The default contents of these prompts live in `internal/prompts/templates/*.md` in the source tree and are embedded into the binary at build time via `//go:embed`. At runtime they are seeded into `<memory_dir>/prompts/*.md` on first startup. After that, the running agent reads from the memory store, not the embedded copies. This means:

- Editing files under `internal/prompts/templates/` in the source tree only affects fresh installs (or installs whose memory store has had those files deleted). To rebuild from defaults, delete `<memory_dir>/prompts/` and restart.
- Edits the agent makes to its own prompts persist in the memory store and survive restarts.
- Edits to the embedded defaults require rebuilding the binary.

### Telegram Integration

Bidirectional communication with a human collaborator via Telegram. Incoming messages are surfaced at the next turn boundary -- the in-flight LLM request is never cancelled, so the agent finishes its current thought before responding. If the agent was about to use tools when the message arrived, those calls are deferred and the agent can choose whether to re-issue them after replying. Outgoing messages are converted to Telegram MarkdownV2 with auto-chunking for the 4096-character limit, falling back to plain text on conversion failure.

### Multi-runtime Sandbox

An optional Docker-based execution environment. The default image (`ghcr.io/matjam/faultline-sandbox`, built from `docker/sandbox/Dockerfile`) is Arch-based and ships Python+pip, [`uv`](https://github.com/astral-sh/uv) + `uvx`, Node.js + npm + npx, [Bun](https://bun.sh), [Deno](https://deno.com), Go, plus common CLI tools (curl, jq, ripgrep, fd, git, ...). `sandbox_execute` runs Python scripts via `uv`; `sandbox_shell` gives the agent arbitrary shell access to any runtime on PATH. Containers are ephemeral (created per execution, removed after); the sandbox has a flat file structure (`scripts/`, `input/`, `output/`) and supports configurable network access, memory limits, and execution timeouts. Configure a different image in `config.toml` if you need something else.

### IMAP Email (optional)

When `[email]` is configured, the agent gets an `email_fetch` tool that opens a short-lived IMAP connection per call. Useful for letting the agent pick up things its operator emails to a dedicated inbox.

### Agent Skills (optional)

Faultline supports the [Agent Skills](https://agentskills.io) open standard. When `[skills]` is enabled, Faultline scans `<dir>/<skill-name>/SKILL.md` files at startup and on every context rebuild, injects each skill's name + description into the system prompt's "Available Skills" section, and advertises four `skill_*` tools (activate, read, execute, work_read). Skills are operator-supplied folders that bundle specialized instructions plus optional `scripts/`, `references/`, and `assets/` subdirectories. Each `skill_execute` call runs in an isolated Docker container with **only** the named skill's directory mounted at `/skill` (read-only) plus a fresh per-call `/work` scratch directory — skills cannot see the agent's memory, the regular sandbox, or any other skill's data. The sandbox feature must be enabled separately for `skill_execute` to function.

## Tools

| Category | Tools |
|----------|-------|
| **Internet** | `web_fetch`, `wiki_fetch` |
| **Memory** | `memory_read`, `memory_write`, `memory_edit`, `memory_append`, `memory_insert`, `memory_delete`, `memory_move`, `memory_restore`, `memory_list`, `memory_list_trash`, `memory_empty_trash`, `memory_search` (BM25 + semantic when `[embeddings]` enabled), `memory_grep` |
| **System** | `context_status`, `get_time`, `sleep`, `send_message`, `get_version`, `rebuild_indexes` |
| **Self-update** (when enabled) | `update_check`, `update_apply` |
| **Sandbox** (when enabled) | `sandbox_write`, `sandbox_read`, `sandbox_edit`, `sandbox_append`, `sandbox_insert`, `sandbox_delete`, `sandbox_rename`, `sandbox_list`, `sandbox_execute`, `sandbox_shell`, `sandbox_install_package`, `sandbox_upgrade_package`, `sandbox_remove_package`, `sandbox_list_packages` |
| **Skills** (when enabled, ≥1 skill) | `skill_activate`, `skill_read`, `skill_execute`, `skill_work_read` |
| **Email** (when configured) | `email_fetch` |

## Architecture

Faultline follows hexagonal (ports & adapters) architecture: the agent loop is the domain hexagon, and external systems (LLM, memory, telegram, sandbox, IMAP, state persistence) are adapters behind interfaces the domain owns. See [AGENTS.md](AGENTS.md) for the full layout, port table, and per-package detail.

## Contributing

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `refactor:`, `chore:`, etc.). [release-please](https://github.com/googleapis/release-please) derives version bumps and changelog entries from the commit log on `main`. The recommended workflow: open PRs with whatever messy commits you like, set the PR title to a single conventional-commit subject, and squash-merge.

See AGENTS.md for the full conventional-commits table and other contributor notes.

## License

See [LICENSE](LICENSE) for details.
