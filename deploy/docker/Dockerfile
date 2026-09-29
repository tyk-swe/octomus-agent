# The Octomus control-plane image. The same image serves three roles, chosen by command:
#   octomus-agent --sandbox docker   the control plane: state, scheduler, dashboard and GitHub publication
#   octomus-agent --sandboxd         the sandbox broker, the only holder of the Docker socket
#   octomus-agent --egress           the egress gateway sandboxes reach the internet through
# Untrusted code never runs in this image; it runs in sandbox containers built from sandbox.Dockerfile.
ARG GO_VERSION=1.27.1
ARG NODE_VERSION=22

# The dashboard and binary build on the builder's own platform; Go cross-compiles for the target.
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-trixie-slim AS dashboard
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY VERSION /src/VERSION
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-trixie AS binary
ARG GO_TAGS=
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=dashboard /src/web/build ./web/build
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -tags "${GO_TAGS}" -ldflags="-s -w" -o /out/octomus-agent ./cmd/octomus-agent

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git gh \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10001 octomus \
 && useradd --uid 10001 --gid 10001 --home-dir /var/lib/octomus --no-create-home --shell /usr/sbin/nologin octomus \
 && install -d -o 10001 -g 10001 -m 0700 /var/lib/octomus /var/lib/octomus/data /var/lib/octomus/runner \
      /opt/octomus-tools /run/octomus /run/octomus-egress \
 && printf '[credential "https://github.com"]\n\thelper =\n\thelper = /usr/local/bin/octomus-agent --git-credential\n[core]\n\thooksPath = /dev/null\n\tfsmonitor = false\n' \
      > /etc/octomus-gitconfig
COPY --from=binary /out/octomus-agent /usr/local/bin/octomus-agent
# Named volumes mounted at the directories above start owned by the service user.
ENV HOME=/var/lib/octomus \
    OCTOMUS_CONTAINER=1 \
    OCTOMUS_DATA_DIR=/var/lib/octomus/data \
    GIT_CONFIG_GLOBAL=/etc/octomus-gitconfig \
    GIT_CONFIG_NOSYSTEM=1
USER 10001:10001
WORKDIR /var/lib/octomus
EXPOSE 4200
ENTRYPOINT ["/usr/local/bin/octomus-agent"]
CMD ["--listen", "0.0.0.0:4200", "--sandbox", "docker"]
