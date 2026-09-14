#!/bin/sh
set -eu
version=$1
target=$2
binary=$3
out=${4:-dist}
case "$version" in v[0-9]*) ;; *) echo 'Version must start with v and a digit' >&2; exit 1;; esac
case "$version" in *[!a-zA-Z0-9.+-]*) echo 'Invalid version' >&2; exit 1;; esac
case "$target" in x86_64-unknown-linux-gnu|aarch64-unknown-linux-gnu) ;; *) echo 'Unsupported release target' >&2; exit 1;; esac
mkdir -p "$out"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
mkdir "$stage/octomus-agent"
cp "$binary" "$stage/octomus-agent/octomus-agent"
cp LICENSE README.md SECURITY.md CHANGELOG.md CONTRIBUTING.md AGENTS.md "$stage/octomus-agent/"
cp -R docs deploy "$stage/octomus-agent/"
# Private operational state, credentials and transcripts are never package inputs.
tar -czf "$out/octomus-agent-$version-$target.tar.gz" -C "$stage" octomus-agent
