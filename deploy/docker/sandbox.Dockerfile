# The sandbox image every agent turn and verification command runs in. It carries the pinned runners and common
# build tools; extend it with your project's toolchain (see docs/sandbox.md) and point OCTOMUS_SANDBOX_IMAGE at the
# result. It holds no credential: runner logins arrive in a volume, and GitHub credentials never do.
ARG NODE_VERSION=22
FROM node:${NODE_VERSION}-trixie-slim
ARG CODEX_VERSION=0.153.4
ARG OPENCODE_VERSION=1.18.30
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash build-essential ca-certificates curl git jq less procps python3 python3-venv ripgrep unzip xz-utils \
 && rm -rf /var/lib/apt/lists/* \
 && npm install --global --no-audit --no-fund "@openai/codex@${CODEX_VERSION}" "opencode-ai@${OPENCODE_VERSION}" \
 && npm cache clean --force \
 && groupadd --gid 10001 octomus \
 && useradd --uid 10001 --gid 10001 --create-home --home-dir /home/octomus --shell /bin/bash octomus
ENV NPM_CONFIG_UPDATE_NOTIFIER=false \
    NPM_CONFIG_FUND=false \
    NO_UPDATE_NOTIFIER=1
USER 10001:10001
WORKDIR /home/octomus
