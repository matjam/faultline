# Faultline

An autonomous AI agent daemon written in Go. Faultline runs as a persistent, long-lived process that continuously interacts with an LLM via an OpenAI-compatible API. It learns about the world by browsing the web, persists knowledge in a file-based memory system, communicates with a human collaborator via Telegram, and can execute Python scripts in a sandboxed Docker environment.

The agent can modify its own operating prompts, enabling self-directed behavioral evolution over time.

## Requirements

- Go 1.25+
- Docker (for the Python sandbox feature)
- A Telegram bot token (optional, for collaborator communication)
- An OpenAI-compatible API endpoint (local or remote)

## Building

```sh
go build -o faultline .
```

The binary embeds all default prompt templates from `prompts/` at compile time.

## Configuration

Faultline reads a TOML configuration file (default: `./config.toml`). Missing fields use sensible defaults.

```toml
[api]
url = "http://localhost:5001/v1"   # OpenAI-compatible API endpoint
key = ""                            # API key (defaults to "not-needed" if empty)
model = "qwen"                      # Model name
kobold_extras = true                # Auto-detect KoboldCpp extras (real tokenizer,
                                    # generation aborts, perf metrics). Safe to leave on
                                    # for non-KoboldCpp backends; detection fails silently.

[agent]
memory_dir = "./memory"             # Directory for persistent memory files
max_tokens = 262144                 # Maximum context window size
temperature = 0.8                   # LLM temperature
max_response_tokens = 4096          # Max tokens per LLM response
compaction_threshold = 150000       # Token count triggering context compaction

[telegram]
token = ""                          # Telegram bot token
chat_id = 0                         # Telegram chat ID for collaborator

[log]
level = "info"                      # Console log level (debug/info/warn/error)
dir = "./logs"                      # Log file directory

[sandbox]
enabled = false                     # Enable Python sandbox
image = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim"
dir = "./sandbox"                   # Sandbox working directory
timeout = "5m"                      # Script execution timeout
network = false                     # Allow network access in sandbox
memory_limit = "512m"               # Docker memory limit
```

## Running

```sh
./faultline -config ./config.toml
```

The agent runs continuously until interrupted. Shutdown behavior:

- **First SIGINT/SIGTERM**: Triggers graceful shutdown. The agent is given up to 10 turns (2-minute timeout) to save its state to memory.
- **Second SIGINT/SIGTERM**: Forces immediate exit.

## Features

### Persistent Memory

The agent stores knowledge as markdown files in a configurable directory. All file paths are case-insensitive and auto-appended with `.md`. The memory system supports read, write, edit, append, insert, delete (soft, to `.trash/`), restore, move, list, grep, and full-text search.

### BM25 Search

An in-memory BM25 search index is built from all memory files on startup and rebuilt during context compaction. The agent uses this to find relevant memories by keyword.

### Web Browsing

The agent can fetch web pages, which are converted from HTML to readable markdown text. Results are cached with a TTL to avoid redundant fetches. Long pages can be paginated with offset/length parameters.

### Context Compaction

When the conversation grows beyond a configurable token threshold, the agent is asked to save its current state to memory and produce a summary. The context is then rebuilt from the system prompt, recent memories, and the summary, allowing indefinite operation.

### KoboldCpp Extras (optional)

When the configured API endpoint is detected to be [KoboldCpp](https://github.com/LostRuins/koboldcpp), Faultline uses three native endpoints that sit alongside the OpenAI compatibility layer:

- **Real tokenization** via `/api/extra/tokencount` for compaction decisions, instead of the 4-chars-per-token heuristic. The heuristic under-counts code/JSON heavily, so without this the agent can be running 30-40% over its self-reported token usage by the time compaction triggers.
- **Generation aborts** via `/api/extra/abort` on forced shutdown, so the model actually stops generating instead of leaving the GPU/CPU pinned until the backend notices the client is gone.
- **Backend perf metrics** via `/api/extra/perf` surfaced in the `context_status` tool: last call's input/output tokens, eval speed, total generations, queue depth, uptime.

Detection is best-effort and bounded by a 5s timeout at startup. If the backend isn't KoboldCpp (real OpenAI, vLLM, llama.cpp's openai endpoint, etc.) detection fails silently and Faultline falls back to the heuristic with no other behavioural changes. Set `kobold_extras = false` in `[api]` to skip detection entirely.

### Self-Modifying Prompts

The agent's operating prompts (`system.md`, `compaction.md`, `cycle_start.md`, `continue.md`, `shutdown.md`) are mutable files in the memory store. The agent can read and rewrite them, changing its own behavior across context compactions.

The default contents of these prompts live in `prompts/*.md` in the source tree and are embedded into the binary at build time via `//go:embed`. At runtime they are seeded into `<memory_dir>/prompts/*.md` on first startup. After that, the running agent reads from the memory store, not the embedded copies. This means:

- Editing files under `prompts/` in the source tree only affects fresh installs (or installs whose memory store has had those files deleted). To rebuild from defaults, delete `<memory_dir>/prompts/` and restart.
- Edits the agent makes to its own prompts persist in the memory store and survive restarts.
- Edits to the embedded defaults require rebuilding the binary.

### Telegram Integration

Bidirectional communication with a human collaborator via Telegram. Incoming messages are surfaced at the next turn boundary -- the in-flight LLM request is never cancelled, so the agent finishes its current thought before responding. If the agent was about to use tools when the message arrived, those calls are deferred and the agent can choose whether to re-issue them after replying. Outgoing messages are converted to Telegram MarkdownV2 with auto-chunking for the 4096-character limit, falling back to plain text on conversion failure.

### Python Sandbox

An optional Docker-based execution environment for Python scripts. Uses `uv` for package management. Containers are ephemeral (created per execution, removed after). The sandbox has a flat file structure (`scripts/`, `input/`, `output/`) and supports configurable network access, memory limits, and execution timeouts.

## Tools

The agent has access to the following tool categories:

| Category | Tools |
|----------|-------|
| **Internet** | `web_fetch` |
| **Memory** | `memory_read`, `memory_write`, `memory_edit`, `memory_append`, `memory_insert`, `memory_delete`, `memory_move`, `memory_restore`, `memory_list`, `memory_list_trash`, `memory_empty_trash`, `memory_search`, `memory_grep` |
| **System** | `context_status`, `get_time`, `send_message` |
| **Sandbox** | `sandbox_exec`, `sandbox_write`, `sandbox_read`, `sandbox_list`, `sandbox_delete`, `sandbox_install`, `sandbox_packages` (when enabled) |

## License

See LICENSE file for details.
