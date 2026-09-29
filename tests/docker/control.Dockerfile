# Integration-test control plane: the production image built with the sandboxfixture tag, plus Python for the
# GitHub fixture. Never shipped.
ARG BASE=octomus-agent:e2e-base
FROM ${BASE}
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 \
 && rm -rf /var/lib/apt/lists/* \
 && printf '[safe]\n\tdirectory = *\n' >> /etc/octomus-gitconfig
USER 10001:10001
