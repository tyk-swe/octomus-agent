#!/bin/sh
# Packages bin/octomus-agent and the files scripts/release-files.txt lists into
# dist/octomus-agent-vVERSION-TARGET.tar.gz and refreshes dist/SHA256SUMS.
set -eu
fail() { echo "$*" >&2; exit 1; }
version=v$(cat VERSION)
case "$version" in v[0-9]*) ;; *) fail 'VERSION must start with a digit' ;; esac
case "$version" in *[!a-zA-Z0-9.+-]*) fail 'Invalid VERSION' ;; esac
case "$(go env GOARCH)" in
  amd64) target=x86_64-unknown-linux-gnu ;;
  arm64) target=aarch64-unknown-linux-gnu ;;
  *) fail "Unsupported release architecture: $(go env GOARCH)" ;;
esac
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
mkdir "$stage/octomus-agent"
cp -p bin/octomus-agent "$stage/octomus-agent/"
# The manifest is the complete public input set; never walk operator-populated directories.
# A listed input must be a regular file whose path has no symlink or non-canonical component.
while IFS= read -r input || [ -n "$input" ]; do
  [ -f "$input" ] && [ "$(realpath -e -- "$input")" = "$PWD/$input" ] || fail "Invalid release input: $input"
  cp -p --parents -- "$input" "$stage/octomus-agent/"
done < scripts/release-files.txt
chmod -R u+w,go-w,a+rX "$stage"
mkdir -p dist
tar -czf "dist/octomus-agent-$version-$target.tar.gz" -C "$stage" octomus-agent
cd dist && sha256sum -- octomus-agent-*.tar.gz > SHA256SUMS
