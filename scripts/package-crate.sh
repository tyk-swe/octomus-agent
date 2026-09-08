#!/bin/sh
# Cargo considers explicitly included, gitignored dashboard assets dirty.
# Permit those generated assets, but refuse source changes or unexpected inputs.
set -eu
case "${1:-}" in
  ''|--publish-dry-run|--publish) ;;
  *) echo 'Usage: scripts/package-crate.sh [--publish-dry-run|--publish]' >&2; exit 1 ;;
esac
[ "$#" -le 1 ] || exit 1
[ -z "$(git status --porcelain --untracked-files=all)" ] || {
  echo 'Commit reviewed source changes before packaging a release crate.' >&2
  exit 1
}
files=$(mktemp)
trap 'rm -f "$files"' EXIT HUP INT TERM
cargo package --list --locked --allow-dirty > "$files"
while IFS= read -r path; do
  case "$path" in
    web/build/*|Cargo.toml.orig|.cargo_vcs_info.json) ;;
    *) git ls-files --error-unmatch -- "$path" >/dev/null 2>&1 || {
         echo "Unexpected untracked package input: $path" >&2
         exit 1
       } ;;
  esac
done < "$files"
case "${1:-}" in
  '') cargo package --locked --allow-dirty ;;
  --publish-dry-run) cargo publish --dry-run --locked --allow-dirty ;;
  --publish) cargo publish --locked --allow-dirty ;;
esac
