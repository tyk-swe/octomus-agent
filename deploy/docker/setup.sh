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

# Let Compose resolve shell precedence, .env quoting and interpolation before asking for a scoped token. This
# config-only document permits an empty value so first-run setup can prompt before the real stack requires it.
selection=$(docker compose --env-file .env -f - config --environment <<'COMPOSE'
services:
  repository-selection:
    image: scratch
    environment:
      OCTOMUS_GITHUB_REPO: ${OCTOMUS_GITHUB_REPO-}
COMPOSE
) || fail 'Could not read the repository selection from Docker Compose'
repo=$(printf '%s\n' "$selection" | sed -n 's/^OCTOMUS_GITHUB_REPO=//p')
unset selection
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
# Every subsequent Compose command must use the exact repository named in the credential prompt, including when
# the operator entered a value after an empty exported variable. Save it in .env for later shells as well.
OCTOMUS_GITHUB_REPO=$repo
export OCTOMUS_GITHUB_REPO

umask 077
mkdir -p secrets
token_shown=
echo_off=
# However the script ends, the terminal gets its echo back, and a token this run generated is shown: a later run
# finds the file and cannot show it again.
finish() {
  if [ -n "$echo_off" ]; then stty echo 2>/dev/null || :; fi
  if [ -n "$token_shown" ]; then
    printf '\nOperator token (shown once; keep it in your password manager):\n  %s\n' "$token_shown"
  fi
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# The GitHub token comes first, so an abandoned prompt leaves no operator token nobody has seen.
if [ ! -e secrets/github_token ]; then
  printf 'Fine-grained GitHub token for %s only, with Contents and Pull requests read/write and Checks/Commit statuses read (input hidden): ' "$repo"
  if stty -echo 2>/dev/null; then echo_off=1; fi
  read -r github_token || github_token=
  if [ -n "$echo_off" ]; then stty echo 2>/dev/null || :; echo_off=; fi
  printf '\n'
  [ -n "$github_token" ] || fail 'A GitHub token is required; it stays in the control plane and never enters a sandbox'
  printf '%s\n' "$github_token" > secrets/github_token
  unset github_token
fi
if [ ! -e secrets/operator_token ]; then
  od -An -tx1 -N32 /dev/urandom | tr -d ' \n' > secrets/operator_token
  token_shown=$(cat secrets/operator_token)
fi

docker compose build octomus sandbox-image
# The control plane runs as uid 10001 and reads its secrets as files; hand them over without widening their mode.
if [ "$(stat -c %u secrets/operator_token)" != 10001 ] || [ "$(stat -c %u secrets/github_token)" != 10001 ]; then
  # Compose applies shell overrides and .env quoting. The selected service and
  # its dependencies all use the control-plane image, so remove duplicate names.
  images=$(docker compose config --images octomus)
  image=$(printf '%s\n' "$images" | sort -u)
  case "$image" in
    ''|*'
'*) fail 'Expected one control-plane image from Docker Compose';;
  esac
  docker run --rm --network none --user 0 --entrypoint chown -v "$PWD/secrets:/secrets" "$image" \
    10001:10001 /secrets/operator_token /secrets/github_token
fi
docker compose up -d

# The tunnel's far end is the port Compose published: the shell's OCTOMUS_PORT, else the one in .env.
port=${OCTOMUS_PORT:-$(sed -n 's/^OCTOMUS_PORT=//p' .env | tail -n 1 | tr -d "\"' ")}
case "$port" in ''|*[!0-9]*) port=4200;; esac
cat <<NEXT

Octomus is starting, sandboxed. Next:
  1. Sign in a runner (the login lands in the runner volume, never in the control plane):
       docker compose run --rm login codex login --device-auth
     For OpenCode, first add models.opencode.ai and your provider's sign-in host to OCTOMUS_EGRESS_MODEL_HOSTS
     in .env and run docker compose up -d, then:
       docker compose run --rm login opencode auth login
  2. From your computer, open the dashboard through an SSH tunnel:
       ssh -N -L 4200:127.0.0.1:$port <this-host>
     then browse to http://127.0.0.1:4200
  3. In Configuration, run Check connection: it proves the sandbox from inside one before any work starts.
NEXT
if [ -z "$token_shown" ]; then
  printf '\nThe operator token is unchanged. To read it again: sudo cat %s/secrets/operator_token\n' "$PWD"
fi
