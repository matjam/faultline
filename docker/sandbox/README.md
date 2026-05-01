# Faultline Sandbox Image

Multi-runtime execution image for the Faultline agent's sandbox tools.

Published as `ghcr.io/matjam/faultline-sandbox` by the
`.github/workflows/sandbox-image.yml` workflow.

## What's inside

Languages and package managers:

- Python 3 + `pip`
- `uv` + `uvx` (Astral)
- Node.js + `npm` + `npx`
- Bun
- Deno
- Go

CLI tools the agent tends to reach for:

- `curl`, `wget`, `git`
- `jq`, `ripgrep`, `fd`, `less`, `tree`, `which`
- `tar`, `gzip`, `unzip`, `zip`, `gnu-netcat`, `make`, `diffutils`, `patch`

Base: `archlinux:base-devel` (rolling). Each image rebuild picks up
current versions of the runtimes; the *published* image is the
reproducible artifact, not the Dockerfile inputs.

## Contracts the Go side relies on

`internal/adapters/sandbox/docker/` talks to the image via:

- `sh` and `uv` on `PATH`
- Mounts: `/scripts` (ro), `/input` (ro), `/output` (rw), `/venv` (rw),
  `/cache` (rw), `/pyproject.toml`, `/uv.lock`
- Env: `UV_CACHE_DIR=/cache`, `UV_LINK_MODE=copy`,
  `UV_PROJECT_ENVIRONMENT=/venv`
- `--user UID:GID` (host user; image must run as any UID)
- `--network=none` by default

Anything else on `PATH` is reachable through the agent's `sandbox_shell`
tool.

## Building locally

```sh
docker build -t faultline-sandbox:dev docker/sandbox
```

Then point `config.toml` at it:

```toml
[sandbox]
enabled = true
image = "faultline-sandbox:dev"
```

## Smoke test

```sh
docker run --rm faultline-sandbox:dev bash -c '
  uv --version
  uvx --version
  python --version
  pip --version
  node --version
  npm --version
  bun --version
  deno --version
  go version
  curl --version | head -1
  jq --version
  rg --version | head -1
'
```

Should print one version line per runtime. If anything is missing, the
build is broken.

## Tags published by CI

- `:latest` — most recent commit on `main`
- `:vX.Y.Z`, `:vX.Y` — release tags (driven by `release-please`)
- `:sha-<short>` — every commit on `main`
- `:pr-<n>` — pull-request preview builds (built but not pushed for
  forks; the workflow only pushes when the head ref is on this repo)

The `config.Default()` shipped with the binary points at `:latest`. Pin
to a versioned tag in your `config.toml` if you want a specific image
version locked down.

## Why Arch

1. **Rolling toolchains.** Each rebuild picks up current node, go,
   python, deno without a separate version-pinning treadmill.
2. **`pacman` is a clean superset of what we need.** All system tools
   and most runtimes (deno included) are in the official repos; only
   `uv` and `bun` come from upstream installers.
3. **No accidental Debian-isms.** The previous default
   (`ghcr.io/astral-sh/uv:python3.12-bookworm-slim`) was Debian-flavoured
   and Python-only. Arch makes the multi-runtime intent obvious.
