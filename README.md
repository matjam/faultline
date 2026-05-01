# Faultline

An autonomous AI agent daemon written in Go. Faultline runs as a persistent, long-lived process that continuously interacts with an LLM via an OpenAI-compatible API. It learns about the world by browsing the web, persists knowledge in a file-based memory system, communicates with a human collaborator via Telegram, and can execute Python scripts in a sandboxed Docker environment.

The agent can modify its own operating prompts, enabling self-directed behavioral evolution over time. With auto-update enabled, the daemon also keeps its own binary current — polling GitHub releases, verifying checksums, atomically swapping in new versions, and restarting cleanly. See [Auto-update](#auto-update) below.

## Requirements

- Go 1.26+ (only needed if building from source)
- Docker on the host machine (optional, for the sandbox / `skill_execute` features). The expected deployment is faultline running as a host process talking to the host's Docker daemon — see [Deployment](#deployment).
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

Faultline refuses to start as root (`uid=0`). The agent has broad filesystem and network access, and the sandbox security model depends on `--user <unprivileged>:<unprivileged>` propagating an unprivileged host UID into containers — running as root collapses both protections. Run under a dedicated unprivileged user (e.g. `User=faultline` in a systemd unit, or `sudo -u faultline` for ad-hoc).

## Deployment

The expected deployment is **faultline as a host process** on a machine with Docker installed. Faultline talks to the host's Docker daemon to spawn ephemeral sandbox containers per `sandbox_*` and `skill_execute` call. This is the path that's been tested and is what the maintainer runs in production.

### Native, under systemd (recommended)

A minimal user-level systemd unit:

```ini
# ~/.config/systemd/user/faultline.service
[Unit]
Description=Faultline
After=network.target

[Service]
Type=simple
WorkingDirectory=/data/faultline
# Optional: wait for the LLM backend to come up before starting.
ExecStartPre=/bin/bash -c 'until curl -sf http://localhost:5001/v1/models >/dev/null 2>&1; do sleep 5; done'
ExecStart=/data/faultline/bin/faultline
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

Layout under `/data/faultline`:

```
/data/faultline/
├── bin/faultline           # the binary; auto-update writes here
├── config.toml
├── memory/                 # the agent's mutable memory store
├── sandbox/                # per-sandbox scratch + cache
├── skills/                 # operator-supplied (or skill_install-ed) skills
├── logs/                   # daily-rotated agent logs and chat transcripts
└── state.json              # conversation log, restored on restart
```

Enable and start:

```sh
systemctl --user daemon-reload
systemctl --user enable --now faultline.service
journalctl --user -u faultline -f
```

The user that owns this unit (`User=` for system units, the invoking user for user units) needs:

- Membership in the `docker` group (or equivalent) so it can talk to the daemon.
- Read access to the LLM endpoint (network, API key in `config.toml`).
- Write access to `/data/faultline` and everything under it.
- **Not** to be `root` — faultline refuses to start at `uid=0`.

A system-level unit (`/etc/systemd/system/faultline.service`) with `User=faultline` works the same way; pick whichever matches how you run other daemons.

### Faultline in a container (untested; significant security trade-off)

It is technically possible to run faultline itself inside a Docker container, mounting the host's Docker socket so the in-container agent can spawn sandbox containers on the host. **This has not been tested by the maintainer.** It is documented here for completeness, with the trade-off called out:

- Mounting `/var/run/docker.sock` into a container is functionally equivalent to giving that container root on the host. Anyone with access to the socket can `docker run -v /:/host …` and escape immediately.
- Faultline's `os.Getuid()==0` refusal does NOT protect against this. The check looks at the agent process's UID; it doesn't see that the process can talk to a privileged daemon.
- A successful prompt injection or a malicious skill that bypasses the audit subagent can therefore compromise the host even though the in-container agent runs unprivileged.

If you accept this trade-off — for the operational benefits of containerization (supervisor restart, log capture, resource limits) — the rough shape is:

```yaml
# docker-compose.yml — UNTESTED. Validate before relying on.
services:
  faultline:
    image: ghcr.io/<your-fork-or-mirror>/faultline:<version>
    user: "1000:1000"
    restart: unless-stopped
    volumes:
      - /data/faultline:/data/faultline
      - /var/run/docker.sock:/var/run/docker.sock
    working_dir: /data/faultline
    command: ["./bin/faultline", "-config", "./config.toml"]
    # Whatever network and DNS your LLM endpoint needs:
    network_mode: host
```

Alternative without the socket-mount risk: **don't run faultline in a container.** The native systemd path above gets you supervisor restart, log capture, and resource limits via `MemoryMax=` / `CPUQuota=` etc. without the `docker.sock` blast radius.

Note that there is no published `ghcr.io/matjam/faultline` image; the published image is the **sandbox** image (`ghcr.io/matjam/faultline-sandbox`) used by the agent's sandbox feature. If you containerize faultline itself, you build that image yourself.

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

When `[skills] install_enabled = true` (off by default), the agent can also install new skills autonomously via `skill_install`, which fetches a tarball URL or git repository into the skills directory after validating that it contains a parseable `SKILL.md`. The catalog reloads on every context rebuild plus immediately after a successful install, so a freshly-installed skill is visible without a restart.

When `[subagent]` is also enabled, every `skill_install` triggers a security audit before the skill reaches the catalog: an audit subagent (running under the `default` profile) is given the extracted skill's metadata and full file contents, instructed to look for behavior-vs-intent mismatches, exfiltration patterns, credential theft, code execution, obfuscation, and other malicious indicators, and asked to search the web for reports about the specific skill or its author. The audit verdict (`APPROVE: ...` or `DENY: ...`) is parsed from the subagent's report; anything that doesn't explicitly approve is fail-closed (the install is aborted and the temporary download is discarded). When `[subagent]` is disabled, the audit is skipped with a loud warning and a notice in the install output — operators who want autonomous installs without the security review can run that way deliberately, but the default deployment will have both enabled.

### Subagents (optional)

When `[subagent]` is enabled, the primary agent gains five tools (`subagent_run`, `subagent_spawn`, `subagent_wait`, `subagent_status`, `subagent_cancel`) and can delegate work to a child agent loop running under a configured profile. Profiles select an LLM endpoint, model, and sampler overrides; a synthesized `default` profile (matching `[api]`) is always available. The primary supplies all relevant context as the child's prompt; the child runs a fresh agent loop with the same tool surface (minus `sleep`, `update_*`, and nested `subagent_*`) and terminates by calling `subagent_report`, whose payload is returned to the primary. Synchronous runs (`subagent_run`) block the primary until the child reports; async runs (`subagent_spawn`) let the primary keep working while the report arrives in its inbox like an operator message. `subagent_wait(work_id)` is the bridge — block until a previously-spawned child reports, drain the report inline. Children share the primary's memory, search indexes, sandbox, and skills; they cannot see its conversation log and cannot themselves spawn further subagents.

## Tools

| Category | Tools |
|----------|-------|
| **Internet** | `web_fetch`, `wiki_fetch` |
| **Memory** | `memory_read`, `memory_write`, `memory_edit`, `memory_append`, `memory_insert`, `memory_delete`, `memory_move`, `memory_restore`, `memory_list`, `memory_list_trash`, `memory_empty_trash`, `memory_search` (BM25 + semantic when `[embeddings]` enabled), `memory_grep` |
| **System** | `context_status`, `get_time`, `sleep`, `send_message`, `get_version`, `rebuild_indexes` |
| **Self-update** (when enabled) | `update_check`, `update_apply` |
| **Sandbox** (when enabled) | `sandbox_write`, `sandbox_read`, `sandbox_edit`, `sandbox_append`, `sandbox_insert`, `sandbox_delete`, `sandbox_rename`, `sandbox_list`, `sandbox_execute`, `sandbox_shell`, `sandbox_install_package`, `sandbox_upgrade_package`, `sandbox_remove_package`, `sandbox_list_packages` |
| **Skills** (when enabled, ≥1 skill) | `skill_activate`, `skill_read`, `skill_execute`, `skill_work_read` |
| **Skills install** (when `install_enabled`) | `skill_install` |
| **Subagents** (when enabled) | `subagent_run`, `subagent_spawn`, `subagent_wait`, `subagent_status`, `subagent_cancel` |
| **Email** (when configured) | `email_fetch` |

## Architecture

Faultline follows hexagonal (ports & adapters) architecture: the agent loop is the domain hexagon, and external systems (LLM, memory, telegram, sandbox, IMAP, state persistence) are adapters behind interfaces the domain owns. See [AGENTS.md](AGENTS.md) for the full layout, port table, and per-package detail.

## Contributing

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `refactor:`, `chore:`, etc.). [release-please](https://github.com/googleapis/release-please) derives version bumps and changelog entries from the commit log on `main`. The recommended workflow: open PRs with whatever messy commits you like, set the PR title to a single conventional-commit subject, and squash-merge.

See AGENTS.md for the full conventional-commits table and other contributor notes.

## License

See [LICENSE](LICENSE) for details.
