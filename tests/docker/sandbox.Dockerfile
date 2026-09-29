# Integration-test sandbox image: deterministic Codex and OpenCode fixtures instead of real runners. Never shipped.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash ca-certificates git python3 \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10001 octomus \
 && useradd --uid 10001 --gid 10001 --create-home --home-dir /home/octomus --shell /bin/bash octomus
COPY tests/fixtures/codex.py /usr/local/bin/codex
COPY tests/fixtures/opencode.py /usr/local/bin/opencode
COPY tests/fixtures/worker.py /usr/local/bin/worker.py
RUN chmod 755 /usr/local/bin/codex /usr/local/bin/opencode \
 && printf '[safe]\n\tdirectory = *\n' > /etc/gitconfig
USER 10001:10001
WORKDIR /home/octomus
