# syntax=docker/dockerfile:1.6

# Faultline agent sandbox image.
#
# Multi-runtime execution environment for the Faultline agent's sandbox
# tools. The Go side talks to this image purely through:
#
#   - sh / bash on PATH
#   - uv on PATH (drives Python script execution and package management)
#   - mounts at /scripts /input /output /venv /cache /pyproject.toml /uv.lock
#   - --user UID:GID (host user; image must run unprivileged)
#
# Beyond that, anything on PATH inside the container is reachable by the
# agent via the sandbox_shell tool. This image bakes in the runtimes the
# agent is most likely to want: python+pip, uv+uvx, node+npm+npx, bun,
# deno, go, plus common LLM-friendly CLI tools (curl, wget, git, jq,
# ripgrep, fd, ...).
#
# Base is Arch Linux's base-devel for an up-to-date toolchain on every
# rebuild. Reproducibility comes from pinning the *published* image
# (ghcr.io/matjam/faultline-sandbox:vX.Y.Z), not from pinning every
# Arch package -- pacman versions move with each rebuild and that's
# the point of using a rolling base.

FROM archlinux:base-devel

LABEL org.opencontainers.image.source="https://github.com/matjam/faultline"
LABEL org.opencontainers.image.description="Faultline agent sandbox: multi-runtime execution environment (Python/uv, Node, Bun, Deno, Go) on Arch Linux"
LABEL org.opencontainers.image.licenses="MIT"

# pipefail so a failure in any stage of `curl ... | sh` aborts the build.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# ---------------------------------------------------------------------------
# pacman: system tools and language runtimes from official repos.
#
# Single RUN to keep one tight layer. Sync DBs, full upgrade, install
# everything we want, then purge caches so the image stays compact.
# Versions are not pinned -- Arch is rolling and the published image
# itself is the artifact-of-record.
# ---------------------------------------------------------------------------
# hadolint ignore=DL3018
RUN pacman -Syu --noconfirm --needed \
        bash coreutils findutils diffutils patch \
        ca-certificates openssl \
        curl wget \
        git \
        jq ripgrep fd \
        less tree which \
        tar gzip unzip zip \
        gnu-netcat \
        make \
        python python-pip \
        nodejs npm \
        go \
        deno \
    && pacman -Scc --noconfirm \
    && rm -rf /var/cache/pacman/pkg/* /var/lib/pacman/sync/*

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
    PATH=/usr/local/bin:/usr/bin:/bin

WORKDIR /

# The sandbox always passes its own command to `docker run` (e.g.
# `sh -c "uv sync && uv run python /scripts/foo.py"`); this default is
# only used by ad-hoc operator invocations.
CMD ["/bin/bash"]
