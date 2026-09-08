# Distribution and release preparation

Public GitHub releases and crates.io publication remain pending. Local packages,
fixture tests and workflow definitions do not prove public download availability.
Employer/name clearance and LICENSE/NOTICE ownership facts remain owner gates.

## Build and package

```bash
npm ci --prefix web
make check
make test
make audit
make package
```

Install cargo-audit first with `cargo install cargo-audit --locked`. Dashboard
assets are generated before Cargo and embedded using include_dir. The build fails
with instructions if `web/build/200.html` is absent. Rebuild Rust after UI edits.
The output is `dist/octomus-agent-vVERSION-TARGET.tar.gz` plus `SHA256SUMS`.
GNU/Linux x86_64 and aarch64 are the supported release targets; Ubuntu 24.04 is the
release build and smoke-test baseline. Older glibc distributions are not promised.

The archive contains `octomus-agent/octomus-agent`, license, policies and operator
documentation. It never packages live state or separate runtime dashboard files.
The installer validates a single matching SHA-256 entry before extracting only
the executable, then replaces the destination binary. Set an absolute writable
`INSTALL_DIR` to avoid sudo. Downloads and checksums come from the same release;
Sigstore signing is deferred.

## GitHub release workflow

After owner clearance, a pushed `v*` tag runs the reusable full checks and native
Ubuntu 24.04 builds on x86_64 and aarch64. The tag must equal `v` plus Cargo.toml's
version. Both tarballs must pass embedded HTTP and installer smoke tests before
the publishing job receives contents-write permission. Checksums cover both
archives; generated release notes are the default. Prerelease tags are marked as
prereleases and excluded from the installer's latest-stable lookup.

The manual workflow accepts an existing tag and optional multiline notes. Use
that path with the final owner-written v0.1.0 notes. If the release already exists,
nonempty supplied notes update only its description; published assets are never
replaced. A rerun without notes fails for an existing release.
The workflow does not push tags, merge PRs, change visibility or publish crates.
Before enabling live workers, configure release-tag protections so their GitHub
identity cannot trigger a release by pushing a tag. These repository controls
are an owner setup action; worker prompts are not an authorization boundary.

## crates.io handoff

Cargo metadata uses the existing provisional repository/package name. The root-anchored package
inclusion list explicitly contains generated `web/build` assets, despite their
Git ignore rule. Do not remove them or make Cargo run npm on an end user's host. Root anchoring also prevents generic README/LICENSE patterns from pulling in Node dependency files.

After all changes are committed, validate:

```bash
npm run build --prefix web
./scripts/package-crate.sh
cargo package --list --allow-dirty
python3 tests/crate.py
```

The package verification builds the extracted crate. Confirm its binary serves the
dashboard outside the checkout without Node, as covered by the distribution test.
Cargo treats explicitly included generated assets as dirty even when Git ignores
them. The wrapper requires clean tracked source and no ordinary untracked files,
checks package inputs against Git (allowing only generated dashboard assets and
Cargo metadata), then passes `--allow-dirty` for the asset files. Its provenance
metadata honestly records those generated files as dirty. Do not bypass the source
check for publication. Direct `cargo package --locked --allow-dirty` is acceptable
for local, uncommitted development verification only.

Only after the owner clears naming/ownership and controls the registry account:

```bash
./scripts/package-crate.sh --publish-dry-run
./scripts/package-crate.sh --publish
```

These commands are a handoff, not a record of execution. Record the registry URL
and install evidence before advertising `cargo install octomus-agent --locked` as
available. No credentials belong in this repository.

## Community setup handoff

The owner can apply `.github/labels.json` with authenticated GitHub CLI:

```bash
python3 - <<'PY'
import json, subprocess
with open('.github/labels.json') as f:
    for label in json.load(f):
        subprocess.run(['gh', 'label', 'create', label['name'], '--color', label['color'],
                        '--description', label['description'], '--force'], check=True)
PY
```

Enable **Discussions** in the repository's Settings → General → Features after
clearance. Confirm `@tyk-swe` has the access required for CODEOWNERS review requests.
Issue forms and PR templates ship as repository files; labels, Discussions and
actual private-report delivery are external setup checks, not completed here.
