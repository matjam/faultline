# syntax=docker/dockerfile:1.6

# Faultline agent sandbox image.
#
# Multi-runtime execution environment for the Faultline agent's sandbox
# tools. The Go side talks to this image purely through:
#
#   - sh / bash on PATH
#   - uv on PATH (drives Python script execution and package management)
#   - mounts at /scripts /input /output /venv /node /mcp /cache /pyproject.toml /uv.lock
#   - --user UID:GID (host user; image must run unprivileged)
#
# Beyond that, anything on PATH inside the container is reachable by the
# agent via the sandbox_shell tool. This image bakes in the runtimes the
# agent is most likely to want: python+pip, uv+uvx, node+npm+npx, bun,
# deno, go, plus common LLM-friendly CLI tools (curl, wget, git, jq,
# ripgrep, fd, ...).
#
# Base is Debian bookworm because it publishes both amd64 and arm64 images.
# Reproducibility comes from pinning the *published* image
# (ghcr.io/matjam/faultline-sandbox:vX.Y.Z), not from pinning every package.

FROM debian:bookworm-slim

LABEL org.opencontainers.image.source="https://github.com/matjam/faultline"
LABEL org.opencontainers.image.description="Faultline agent sandbox: multi-runtime execution environment (Python/uv, Node, Bun, Deno, Go) on Debian"
LABEL org.opencontainers.image.licenses="MIT"

# pipefail so a failure in any stage of `curl ... | sh` aborts the build.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# ---------------------------------------------------------------------------
# apt: system tools and language runtimes from official repos.
#
# Single RUN to keep one tight layer. Sync DBs, full upgrade, install
# everything we want, then purge caches so the image stays compact.
# Versions are not pinned -- the published image itself is the
# artifact-of-record.
# ---------------------------------------------------------------------------
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash coreutils findutils diffutils patch \
        ca-certificates openssl \
        curl wget \
        git \
        jq ripgrep fd-find \
        less tree which \
        tar gzip unzip zip \
        netcat-openbsd \
        make \
        python3 python3-pip python-is-python3 \
        nodejs npm \
        golang-go \
    && ln -sf /usr/bin/fdfind /usr/local/bin/fd \
    && ln -sf /usr/bin/pip3 /usr/local/bin/pip \
    && rm -rf /var/lib/apt/lists/*

# ---------------------------------------------------------------------------
# uv + uvx via Astral's official installer.
#
# UV_INSTALL_DIR is the documented env-var for the install script. Putting
# the binaries in /usr/local/bin keeps them on PATH for any UID -- the
# sandbox always runs the container with --user <host_uid>:<host_gid>, so
# tools must be readable/executable by an arbitrary unprivileged user.
# --no-modify-path stops the installer touching shell rc files (irrelevant
# in a container, noisy in the build log).
# ---------------------------------------------------------------------------
ENV UV_INSTALL_DIR=/usr/local/bin
RUN curl -LsSf https://astral.sh/uv/install.sh | sh -s -- --no-modify-path \
    && /usr/local/bin/uv --version \
    && /usr/local/bin/uvx --version

# ---------------------------------------------------------------------------
# deno via the official installer.
# ---------------------------------------------------------------------------
ENV DENO_INSTALL=/usr/local
RUN curl -fsSL https://deno.land/install.sh | sh \
    && /usr/local/bin/deno --version

# ---------------------------------------------------------------------------
# bun via the official installer.
#
# BUN_INSTALL controls where bun lands. The installer drops the binary at
# $BUN_INSTALL/bin/bun, so /usr/local gives us /usr/local/bin/bun -- on
# the default PATH for any user.
# ---------------------------------------------------------------------------
ENV BUN_INSTALL=/usr/local
RUN curl -fsSL https://bun.sh/install | bash \
    && /usr/local/bin/bun --version

# ---------------------------------------------------------------------------
# Runtime defaults expected by the Go sandbox.
#
# These match the -e flags the Go side passes on every `docker run`. Baking
# them in lets ad-hoc invocations (e.g. operator debugging via
# `docker run --rm -it ghcr.io/matjam/faultline-sandbox`) behave the same
# as the agent's invocations. uv honours UV_PROJECT_ENVIRONMENT and
# UV_CACHE_DIR whenever a project root is on the working directory.
# ---------------------------------------------------------------------------
ENV UV_CACHE_DIR=/cache \
    UV_LINK_MODE=copy \
    UV_PROJECT_ENVIRONMENT=/venv \
    PATH=/node/node_modules/.bin:/usr/local/bin:/usr/bin:/bin

# ---------------------------------------------------------------------------
# Defense-in-depth: bake in a non-root default USER.
#
# The Go side ALWAYS passes --user <host_uid>:<host_gid> on every
# `docker run`, so this directive is overridden in normal operation and
# the container actually runs as the unprivileged host user. The USER
# directive matters only when --user is somehow not passed (a future
# bug, an ad-hoc operator `docker run` without the flag, etc.); in that
# case we want the image to refuse to be root rather than silently fall
# back to it.
#
# UID 65532 is the conventional "nonroot" UID (used by Distroless and
# others). Far from typical host UIDs (0-1999) so collisions with any
# operator running faultline as their normal account are unlikely. The
# user has no shell and a nonexistent home dir on purpose -- there is
# no scenario where the sandbox should be able to log in or persist
# state.
# ---------------------------------------------------------------------------
RUN echo 'sandbox:x:65532:65532::/nonexistent:/usr/sbin/nologin' >> /etc/passwd \
    && echo 'sandbox:x:65532:' >> /etc/group
USER 65532:65532

WORKDIR /

# The sandbox always passes its own command to `docker run` (e.g.
# `sh -c "uv sync && uv run python /scripts/foo.py"`); this default is
# only used by ad-hoc operator invocations.
CMD ["/bin/bash"]
