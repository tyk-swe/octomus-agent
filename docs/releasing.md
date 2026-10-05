# Releasing

[v0.2.0 is published](https://github.com/tyk-swe/octomus-agent/releases/tag/v0.2.0)
with x86_64 and aarch64 archives
and `SHA256SUMS`, plus signed `octomus-agent` and `octomus-sandbox` images on
GHCR. Local packages, fixture tests and workflow definitions do not prove
public download availability; the release page, the green release workflow run
and the installer's checksum verification do. LICENSE/NOTICE ownership facts
remain owner-supplied: AGENTS.md defers those edits until the owner supplies
cleared facts. Its [release workflow](https://github.com/tyk-swe/octomus-agent/actions/runs/37286084064)
passed both native build/install checks; the upgrade rehearsal exercised the
released-image schema-7-to-8 upgrade and restore-based rollback.

## Version

The application version lives in the `VERSION` file at the repository root. The
Go binary embeds it at compile time, so `--version`, `/healthz` and runner
client metadata all report the same string. The dashboard build shows it too, and
release tooling reads the same file for tag validation and archive names. Bump it
in one place only.

## Build and package

```bash
npm ci --prefix web
make check
make test
make audit
make package
```

`make audit` runs govulncheck (`go run golang.org/x/vuln/cmd/govulncheck@v1.8.0`,
pinned) plus `npm audit`; it needs module download access. Dashboard assets are
generated before the Go build and embedded with `go:embed`. Compilation fails if
`web/build` output is absent. Rebuild after UI edits. The output is
`dist/octomus-agent-vVERSION-TARGET.tar.gz` plus `SHA256SUMS`. GNU/Linux x86_64
and aarch64 are the supported release targets; Ubuntu 24.04 is the release build
and smoke-test baseline. The build is statically linked (`CGO_ENABLED=0`), so
the executable needs no build toolchain or minimum glibc at runtime.

The archive contains `octomus-agent/octomus-agent`, license, policies and operator
documentation. It never packages live state or separate runtime dashboard files.
`scripts/release-files.txt` lists every public input besides the executable, and
`scripts/package.sh` archives exactly those files with normalised permissions; CI
smoke-tests the executable extracted from each archive. Packaging works without Git
metadata and rejects any listed input that is not a regular file or whose path has a
symlinked or non-canonical component.
The installer validates a single matching SHA-256 entry before extracting only
the executable, then replaces the destination binary. Set an absolute writable
`INSTALL_DIR` to avoid sudo. A failed copy or rename leaves the previous
destination intact and removes the temporary executable. The destination must
not be a directory; an existing symlink is replaced without following its target.
Downloads and checksums come from the same release;
release archives do not include Sigstore signatures.

## GitHub release workflow

After owner clearance, a pushed `v*` tag runs the reusable full checks and native
Ubuntu 24.04 builds on x86_64 and aarch64. The tag must equal `v` plus the
`VERSION` file contents. Both tarballs must pass the archive-contents check and the
embedded HTTP smoke test before the publishing job receives contents-write permission.
Checksums cover both archives; generated release notes are the default.
Prerelease tags are marked as prereleases and excluded from the installer's
latest-stable lookup.

The manual workflow accepts an existing tag and optional multiline notes. Use
that path with the final owner-approved release notes. If the release already exists,
nonempty supplied notes update only its description; published assets are never
replaced. A rerun without notes fails for an existing release.
The workflow does not push tags, merge PRs or change visibility.
Before enabling live workers, configure release-tag protections so their GitHub
identity cannot trigger a release by pushing a tag. These repository controls
are an owner setup action; worker prompts are not an authorization boundary.

## Container images

The same tag also builds the two Docker images for `linux/amd64` and `linux/arm64`:
`ghcr.io/tyk-swe/octomus-agent:<version>` (the control plane, broker and egress gateway) and
`ghcr.io/tyk-swe/octomus-sandbox:<version>`. The release workflow pushes each with an SBOM and
build provenance attached, and signs the pushed digest with cosign keyless signing, bound to
the workflow's GitHub identity. Verify a pulled image before using it:

```bash
cosign verify ghcr.io/tyk-swe/octomus-agent:0.2.0 \
  --certificate-identity-regexp '^https://github.com/tyk-swe/octomus-agent/.github/workflows/release.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

To deploy release images instead of building locally, set `OCTOMUS_IMAGE` and
`OCTOMUS_SANDBOX_IMAGE` in `deploy/docker/.env` and pull them before `docker compose up -d`; the
broker never pulls. Every release tag publishes both images to GHCR; verify a
pulled image with the command above before deploying it.

## State format

v0.1.0 shipped schema version 7, the oldest database a release upgrades. Migrations are
forward-only: `releaseMigrations` in `internal/store/schema.go` lists one step per
version, appended only, each upgrading the schema by exactly one version inside a single
`BEGIN IMMEDIATE` transaction that also sets `user_version`. A migration must not begin,
commit or change the journal mode.

`internal/store/schema.sql` stays the complete DDL of the latest version; a fresh database
never replays migrations. A field added to a saved record is a pointer, tagged
`wire:"default"`, or filled in by the migration. `TestFreshSchemaMatchesOpenedGolden`
fails when `schema.sql` drifts from a migrated golden.

After tagging a release, generate its golden database:

```bash
python3 scripts/golden-state.py --ref vX.Y.Z --scenario chain --expect-version 8 --output internal/store/testdata/state-vX.Y.Z.db
```

Use the release's actual schema version (`8` for v0.2.0). Check it in with its provenance (commit, scenario, sha256) and extend the golden tests so
later releases keep opening every checked-in golden. Before tagging a schema change,
rehearse the upgrade on a copy of real state with the released images, following
[Backup and upgrade](deployment.md#backup-and-upgrade), and record the image
identities and results with the release.
