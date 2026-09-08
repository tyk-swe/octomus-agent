#!/bin/sh
# Usage: sh install.sh [vVERSION]. INSTALL_DIR defaults to /usr/local/bin.
set -eu
fail() { echo "octomus installer: $*" >&2; exit 1; }
[ "$(uname -s)" = Linux ] || fail 'Linux is required'
case "$(uname -m)" in
  x86_64|amd64) target=x86_64-unknown-linux-gnu ;;
  aarch64|arm64) target=aarch64-unknown-linux-gnu ;;
  *) fail 'Supported architectures: x86_64 and aarch64' ;;
esac
for tool in curl tar sha256sum mktemp install; do command -v "$tool" >/dev/null || fail "Install $tool first"; done
repo=https://github.com/tyk-swe/octomus-agent
version=${1:-${VERSION:-}}
if [ -z "$version" ]; then
  latest=$(curl --proto '=https' --proto-redir '=https' -fsSL -o /dev/null -w '%{url_effective}' "$repo/releases/latest") || fail 'No downloadable release; check release availability'
  version=${latest##*/}
fi
case "$version" in v[0-9]*) ;; *) fail 'Version must be a release tag such as v0.1.0';; esac
case "$version" in *[!a-zA-Z0-9.+-]*) fail 'Invalid version';; esac
dest=${INSTALL_DIR:-/usr/local/bin}
case "$dest" in /*) ;; *) fail 'INSTALL_DIR must be absolute';; esac
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
asset=octomus-agent-$version-$target.tar.gz
url=$repo/releases/download/$version
curl --proto '=https' --proto-redir '=https' -fsSL "$url/$asset" -o "$stage/$asset" || fail 'Release download failed'
curl --proto '=https' --proto-redir '=https' -fsSL "$url/SHA256SUMS" -o "$stage/SHA256SUMS" || fail 'Checksum download failed'
# Select exactly one checksum; never trust paths supplied by the checksum file.
hash=$(awk -v name="$asset" '$2 == name {print $1}' "$stage/SHA256SUMS")
[ "${#hash}" = 64 ] || fail 'Missing or ambiguous checksum'
case "$hash" in *[!a-fA-F0-9]*) fail 'Invalid checksum';; esac
(cd "$stage" && printf '%s  %s\n' "$hash" "$asset" | sha256sum -c -) || fail 'Checksum mismatch; nothing installed'
# Extract only the expected executable into our private temporary directory.
tar -xOzf "$stage/$asset" octomus-agent/octomus-agent > "$stage/octomus-agent" || fail 'Archive is missing the executable'
[ -s "$stage/octomus-agent" ] || fail 'Empty executable'
if [ -d "$dest" ] && [ -w "$dest" ]; then
  install -m 755 "$stage/octomus-agent" "$dest/.octomus-agent.$$"
  mv -f "$dest/.octomus-agent.$$" "$dest/octomus-agent"
else
  command -v sudo >/dev/null || fail "Create a writable $dest or install sudo"
  sudo mkdir -p "$dest"
  sudo install -m 755 "$stage/octomus-agent" "$dest/.octomus-agent.$$"
  sudo mv -f "$dest/.octomus-agent.$$" "$dest/octomus-agent"
fi
printf '\nInstalled %s to %s/octomus-agent\n' "$version" "$dest"
cat <<'NEXT'
Next, as the service user on your dedicated VM (see the README for prerequisites):
  codex login
  gh auth login && gh auth setup-git
  OCTOMUS_TOKEN='<your saved random token>' octomus-agent
Save the generated token in your password manager before starting; the README shows how.
NEXT
