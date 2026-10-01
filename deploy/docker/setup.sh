#!/bin/sh
# Prepares and starts Octomus on this Docker host: checks Docker, writes .env, creates the operator token, stores the
# GitHub token for the control plane only, builds the images and starts the stack.
set -eu
cd "$(dirname "$0")"
fail() { printf 'octomus setup: %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null || fail 'Install Docker Engine 28 or later first: https://docs.docker.com/engine/install/'
docker compose version >/dev/null 2>&1 || fail 'Install the Docker Compose plugin (docker compose)'
server=$(docker version --format '{{.Server.Version}}' 2>/dev/null) || fail 'Cannot reach the Docker daemon; add yourself to the docker group or use sudo'
major=${server%%.*}
case "$major" in *[!0-9]*|'') fail "Unrecognised Docker Engine version $server";; esac
[ "$major" -ge 28 ] || fail "Docker Engine $server is too old: sandboxes need 28 or later for isolated internal networks"
socket=/var/run/docker.sock
[ -S "$socket" ] || fail "No Docker socket at $socket"

[ -f .env ] || cp env.example .env
set_env() {
  if grep -q "^$1=" .env; then
    tmp=$(mktemp .env.XXXXXX)
    awk -v key="$1" -v value="$2" 'BEGIN { FS = OFS = "=" } $1 == key { print key, value; next } { print }' .env > "$tmp"
    mv "$tmp" .env
  else
    printf '%s=%s\n' "$1" "$2" >> .env
  fi
}
set_env DOCKER_GID "$(stat -c %g "$socket")"

repo=$(sed -n 's/^OCTOMUS_GITHUB_REPO=//p' .env)
if [ -z "$repo" ] || [ "$repo" = OWNER/REPOSITORY ]; then
  printf 'GitHub repository to improve (OWNER/REPOSITORY): '
  read -r repo
fi
case "$repo" in
  */*/*|/*|*/|*[!A-Za-z0-9._/-]*) fail "Repository must be OWNER/REPOSITORY, not $repo";;
  */*) ;;
  *) fail "Repository must be OWNER/REPOSITORY, not $repo";;
esac
set_env OCTOMUS_GITHUB_REPO "$repo"

umask 077
mkdir -p secrets
token_shown=
if [ ! -e secrets/operator_token ]; then
  od -An -tx1 -N32 /dev/urandom | tr -d ' \n' > secrets/operator_token
  token_shown=$(cat secrets/operator_token)
fi
if [ ! -e secrets/github_token ]; then
  printf 'Fine-grained GitHub token for %s only, with Contents and Pull requests read/write (input hidden): ' "$repo"
  stty -echo 2>/dev/null || :
  read -r github_token
  stty echo 2>/dev/null || :
  printf '\n'
  [ -n "$github_token" ] || fail 'A GitHub token is required; it stays in the control plane and never enters a sandbox'
  printf '%s\n' "$github_token" > secrets/github_token
  unset github_token
fi

docker compose build octomus sandbox-image
# The control plane runs as uid 10001 and reads its secrets as files; hand them over without widening their mode.
if [ "$(stat -c %u secrets/operator_token)" != 10001 ] || [ "$(stat -c %u secrets/github_token)" != 10001 ]; then
  image=$(sed -n 's/^OCTOMUS_IMAGE=//p' .env)
  docker run --rm --network none --user 0 --entrypoint chown -v "$PWD/secrets:/secrets" "${image:-octomus-agent:local}" \
    10001:10001 /secrets/operator_token /secrets/github_token
fi
docker compose up -d

cat <<NEXT

Octomus is starting, sandboxed. Next:
  1. Sign in a runner (the login lands in the runner volume, never in the control plane):
       docker compose run --rm login codex login --device-auth
     or: docker compose run --rm login opencode auth login
  2. From your computer, open the dashboard through an SSH tunnel:
       ssh -N -L 4200:127.0.0.1:${OCTOMUS_PORT:-4200} <this-host>
     then browse to http://127.0.0.1:4200
  3. In Configuration, run Check connection: it proves the sandbox from inside one before any work starts.
NEXT
if [ -n "$token_shown" ]; then
  printf '\nOperator token (shown once; keep it in your password manager):\n  %s\n' "$token_shown"
fi
