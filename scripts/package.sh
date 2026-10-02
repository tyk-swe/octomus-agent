#!/bin/sh
set -eu
case $# in 3|4) ;; *) echo 'Usage: scripts/package.sh <vVERSION> <target> <binary> [outdir]' >&2; exit 1;; esac
version=$1
target=$2
binary=$3
out=${4:-dist}
case "$version" in v[0-9]*) ;; *) echo 'Version must start with v and a digit' >&2; exit 1;; esac
case "$version" in *[!a-zA-Z0-9.+-]*) echo 'Invalid version' >&2; exit 1;; esac
case "$target" in x86_64-unknown-linux-gnu|aarch64-unknown-linux-gnu) ;; *) echo 'Unsupported release target' >&2; exit 1;; esac
[ -f "$binary" ] || { echo "Missing binary: $binary" >&2; exit 1; }

# The manifest is the complete public release input set, also in source archives
# without .git. Never discover inputs by walking operator-populated directories.
manifest=scripts/release-files.txt
require_release_file() {
    check=$1
    while :; do
        [ ! -L "$check" ] || { echo "Symlink release input: $check" >&2; exit 1; }
        case "$check" in */*) check=${check%/*} ;; *) break ;; esac
    done
    [ -f "$1" ] || { echo "Missing regular release input: $1" >&2; exit 1; }
}
invalid_manifest() {
    echo 'Invalid release manifest' >&2
    exit 1
}
require_release_file "$manifest"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
mkdir -m 755 "$stage/octomus-agent"
cp -p "$binary" "$stage/octomus-agent/octomus-agent"
seen='|'
while IFS= read -r input || [ -n "$input" ]; do
    case "$input" in ''|*[!a-zA-Z0-9._/-]*|/*|*/|*//*) invalid_manifest ;; esac
    case "/$input/" in */./*|*/../*) invalid_manifest ;; esac
    case "$input" in
        LICENSE|README.md|SECURITY.md|CHANGELOG.md|CONTRIBUTING.md|AGENTS.md|docs/*|deploy/*) ;;
        *) invalid_manifest ;;
    esac
    case "$seen" in *"|$input|"*) invalid_manifest ;; esac
    seen="$seen$input|"
    require_release_file "$input"
    case "$input" in
        */*)
            parent=${input%/*}
            # Create each parent with release permissions even under umask 077.
            directory="$stage/octomus-agent"
            old_ifs=$IFS
            IFS=/
            for component in $parent; do
                directory="$directory/$component"
                [ -d "$directory" ] || mkdir -m 755 "$directory"
            done
            IFS=$old_ifs
            ;;
    esac
    cp -p "$input" "$stage/octomus-agent/$input"
done < "$manifest"
[ "$seen" != '|' ] || invalid_manifest
mkdir -p "$out"
tar -czf "$out/octomus-agent-$version-$target.tar.gz" -C "$stage" octomus-agent
