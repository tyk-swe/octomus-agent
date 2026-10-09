# Releasing

Each release publishes x86_64 and aarch64 archives with `SHA256SUMS`, plus signed
`octomus-agent` and `octomus-sandbox` images on GHCR; the [changelog](../CHANGELOG.md)
records what each one shipped. Local packages, fixture tests and workflow definitions do
not prove public download availability; the release page, the green release workflow run
and the installer's checksum verification do.

## Version

The application version lives in the `VERSION` file at the repository root. The
Go binary embeds it at compile time, so `--version`, `/healthz` and runner
client metadata all report the same string. The dashboard build shows it too, and
release tooling reads the same file for tag validation and archive names. Bump it
in one place only. A published version must start with a digit and use only letters,
digits, dots and hyphens, up to 128 characters: it is also an OCI tag. SemVer build
metadata containing `+` cannot be used as an image tag.

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

After owner clearance, push an existing commit's `v*` tag or dispatch the workflow
with that tag. Preparation resolves annotated or lightweight tags to an exact commit
and verifies its `VERSION`. A new release runs the full reusable CI gate at that
commit: source checks, vulnerability audits, race tests, service and browser tests,
native package smoke tests, client contracts, sandbox tests and production image
acceptance. A failed gate prevents release staging and image publication.

The native Ubuntu 24.04 package jobs build the x86_64 and aarch64 archives once.
The image jobs build the production OCI archives once and test their retained native
manifests as described below. After every required check passes, the publisher
assembles these six release assets:

- `octomus-agent-vVERSION-x86_64-unknown-linux-gnu.tar.gz`
- `octomus-agent-vVERSION-aarch64-unknown-linux-gnu.tar.gz`
- `SHA256SUMS`, covering the two native packages
- `octomus-agent.oci.tar`
- `octomus-sandbox.oci.tar`
- `release-images.json`, containing source and image identities and both native acceptance receipts

The publisher first creates a **draft** with a hidden input receipt in its description.
That receipt records the original CI run, tag, source commit, version and SHA-256 of
all six assets. It exists before any upload. The publisher uploads missing assets,
checks all six required remote assets against those hashes and only then publishes the
release. Generated notes are the default; supplied multiline notes replace them.
Prerelease tags are marked as prereleases and excluded from the installer's
latest-stable lookup. The two native installer jobs then exercise the public release
assets and checksums.

The manual workflow has three paths:

| Release state | Supplied notes | Operation |
| --- | --- | --- |
| Absent | Empty or nonempty | Run full CI, stage verified inputs, publish, promote and sign |
| Draft or published | Nonempty | Update only the description; preserve the hidden input receipt |
| Draft or published | Empty | Recover original inputs, finish publication if needed, retry promotion and signing |

Notes updates do not build, test, install, upload assets, publish a draft or promote
images. They also work for historical releases whose tags predate the current scripts
or state schema: the workflow uses its own exact revision's helper code and skips
source/version checks for this operation. Do not remove the hidden input receipt when
editing a release description manually.

An interrupted draft upload downloads the artifacts from the **original** run and
requires every reconstructed asset to match the receipt. It never uses a later run's
build. Package, image and acceptance artifacts retain 90 days, subject to repository
retention limits. Once all assets are uploaded, retries read the permanent release
assets and no longer need Actions artifacts. A failed upload's incomplete `starter`
asset may be removed and retried only while the release is a draft; completed assets
are never overwritten. A changed asset, moved tag, missing receipt or expired original
artifact stops recovery. Recover the original verified inputs or release a new version;
rebuilding into an existing version is not a recovery method. Legacy releases without
retained inputs still support notes updates, but cannot use image recovery.

For recovery, start a fresh manual dispatch with the same tag and **empty notes**.
GitHub's **Re-run failed jobs** can reuse an earlier preparation result of `build`;
if a draft was created since that result, staging safely refuses it. A fresh dispatch
or **Re-run all jobs** reruns preparation and selects the retained-input recovery path.

Releases with the same tag queue rather than cancel one another. Repository CI and
release CI have separate concurrency groups. Draft discovery uses the paginated release
list as well as get-by-tag; GitHub exposes drafts only to identities with push access,
so the preparation job has contents-write permission even though it only reads.

The workflow does not push tags, merge PRs or change repository/package visibility. Before enabling live
workers, configure release-tag protections so their GitHub identity cannot trigger a
release by pushing a tag. Restrict changes to release descriptions, assets, workflow
code and registry tags to trusted release administrators. These repository controls
are an owner setup action; worker prompts are not an authorization boundary.

## Container images

The CI gate builds the two Docker images for `linux/amd64` and `linux/arm64`:
`ghcr.io/tyk-swe/octomus-agent:<version>` (the control plane, broker and egress gateway) and
`ghcr.io/tyk-swe/octomus-sandbox:<version>`. The unchanged production Dockerfiles produce
multi-platform OCI archives with source/version/image labels, SBOM and build provenance.
Separate native amd64 and arm64 runners load those exact manifests. They exercise the
real pinned Codex and OpenCode clients through the production broker, helper and egress
gateway, using a synthetic HTTPS provider with the normal TLS and egress policy active.
The acceptance gate requires structured completion, session resume in new containers,
cancellation, verification, containment, dashboard boot and complete cleanup. Host
inspections bind each recorded runner to its actual image, networks and mounts. See
[Sandbox validation](sandbox.md#checked-so-far-and-what-is-still-yours-to-check) for the topology.

After the complete release becomes public, the image job downloads its retained assets
and checks their hashes, manifests, source identity and both native receipts. Skopeo
copies the complete OCI indexes with `--all --preserve-digests`; there is no Docker build
in the promotion job. A version tag already pointing to the accepted index is reused.
A different existing digest is refused. Each exact index digest is signed with cosign
keyless signing, bound to the workflow's GitHub identity. If copying or signing fails,
dispatch the same tag with empty notes to retry the retained bytes. Existing registry
tags are checked before and after copying; external writers with registry permission
remain trusted because registry tag updates are not a compare-and-swap operation.

Verify a pulled image before using it:

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
python3 scripts/golden-state.py --ref vX.Y.Z --scenario chain --expect-version 10 --output internal/store/testdata/state-vX.Y.Z.db > internal/store/testdata/state-vX.Y.Z.json
```

Use the release's actual schema version (`10` for the current schema). Check in the database and the provenance JSON the script prints (commit, scenario, sha256), and extend the golden tests so
later releases keep opening every checked-in golden. Before tagging a schema change,
rehearse the upgrade on a copy of real state with the released images, following
[Backup and upgrade](deployment.md#backup-and-upgrade), and record the image
identities and results with the release.
