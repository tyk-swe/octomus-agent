# Releasing

Each release publishes x86_64 and aarch64 archives with `SHA256SUMS`, plus signed
`octomus-agent` and `octomus-sandbox` images on GHCR; the [changelog](../CHANGELOG.md)
records what each shipped. Only the release page, the green release workflow run and the
installer's checksum verification prove public download availability, not local
packages, fixture tests or workflow definitions.

## Version

The version lives in the `VERSION` file at the repository root; the binary, `/healthz`,
runner client metadata, the dashboard build and release tooling all read it, so bump it
in one place only. A published version must start with a digit and use only letters,
digits, dots and hyphens, up to 128 characters, because it is also an OCI tag (SemVer
build metadata containing `+` cannot be used).

One bounded validator in `scripts/release-images.py` serves local packaging, release
tags and OCI metadata; `scripts/package.sh` runs it offline through
`scripts/release.py version --tag` before writing an archive.

## Build and package

```bash
npm ci --prefix web
make check
make test
make audit
make package
```

`make audit` runs the pinned govulncheck plus `npm audit` and needs module download
access. Dashboard assets are generated before the Go build and embedded with `go:embed`;
compilation fails if `web/build` is absent, so rebuild after UI edits. The output is
`dist/octomus-agent-vVERSION-TARGET.tar.gz` plus `SHA256SUMS`. GNU/Linux x86_64
and aarch64 are the supported release targets; Ubuntu 24.04 is the release build
and smoke-test baseline. The build is statically linked (`CGO_ENABLED=0`), so
the executable needs no build toolchain or minimum glibc at runtime.

The archive contains `octomus-agent/octomus-agent`, license, policies and operator
documentation. It never packages live state or separate runtime dashboard files.
`scripts/release-files.txt` lists every public input besides the executable, and
`scripts/package.sh` archives exactly those files with normalised permissions, rejecting
any that is not a regular file or has a symlinked or non-canonical path; CI smoke-tests
the executable extracted from each archive. Files not listed there (such as
`AGENTS.md`, `CONTRIBUTING.md` and this document) are not shipped, so shipped documents
link to them by GitHub URL.

The installer validates a single matching SHA-256 entry before extracting only the
executable, then replaces the destination binary; a failed copy or rename leaves the
previous destination intact. Set an absolute writable `INSTALL_DIR` to avoid sudo. The
destination must not be a directory, and an existing symlink is replaced without
following its target. Downloads and checksums come from the same release, and archives
do not include Sigstore signatures.

## GitHub release workflow

After owner clearance, push an existing commit's `v*` tag or dispatch the workflow
with that tag. Preparation resolves annotated or lightweight tags to an exact commit
and verifies its `VERSION`. A new release runs the full reusable CI gate at that
commit: source checks, vulnerability audits, race tests, service and browser tests,
native package smoke tests, client contracts, sandbox tests and production image
acceptance. A failed gate prevents release staging and image publication.

Native Ubuntu 24.04 jobs build the x86_64 and aarch64 archives once, and the image jobs
build the production OCI archives once and test their retained native manifests (see
[Container images](#container-images)). After every required check passes, the publisher
assembles these six release assets:

- `octomus-agent-vVERSION-x86_64-unknown-linux-gnu.tar.gz`
- `octomus-agent-vVERSION-aarch64-unknown-linux-gnu.tar.gz`
- `SHA256SUMS`, covering the two native packages
- `octomus-agent.oci.tar`
- `octomus-sandbox.oci.tar`
- `release-images.json`, containing source and image identities and both native acceptance receipts

The publisher first creates a **draft** with a hidden input receipt in its description
recording the original CI run, tag, source commit, version and SHA-256 of all six assets,
uploads missing assets, verifies all six remote hashes, and only then publishes.
Generated notes are the default unless multiline notes are supplied. Prerelease tags are
marked as prereleases and excluded from the installer's latest-stable lookup. Two native
installer jobs then verify the public release assets and checksums.

| Release state | Supplied notes | Operation |
| --- | --- | --- |
| Absent | Empty or nonempty | Run full CI, stage verified inputs, publish, promote and sign |
| Draft or published | Nonempty | Update only the description; preserve the hidden input receipt |
| Draft or published | Empty | Recover original inputs, finish publication if needed, retry promotion and signing |

Notes-only updates skip building, uploading, publishing and image promotion, and work on
historical releases; preserve the hidden input receipt when editing a description.

Interrupted draft recovery downloads artifacts from the **original** run (retained 90 days)
or permanent release assets and verifies every hash against the receipt; incomplete draft
`starter` assets are replaced, while completed assets are never overwritten. A changed
asset, moved tag, missing receipt or expired artifact stops recovery (never rebuild into an
existing version). Dispatch the same tag with **empty notes** (or **Re-run all jobs**) to
recover. Same-tag releases queue in their own concurrency group. The workflow never pushes
tags, merges PRs or changes visibility; protect release tags, assets, workflows and
registry tags so worker identities cannot trigger or alter releases.

## Container images

The CI gate builds the two Docker images for `linux/amd64` and `linux/arm64`:
`ghcr.io/tyk-swe/octomus-agent:<version>` (the control plane, broker and egress gateway) and
`ghcr.io/tyk-swe/octomus-sandbox:<version>`. The unchanged production Dockerfiles produce
multi-platform OCI archives with source/version/image labels, SBOM and build provenance.
Each native amd64 and arm64 runner imports its manifest without rebuilding, boots the
embedded dashboard, and runs the real pinned Codex and OpenCode clients through the
production broker, helper and egress gateway. A synthetic HTTPS provider supplies
deterministic responses through the normal leased CONNECT gateway, so no live provider
account is used and the TLS and egress policy stay active. The gate requires completed
turns with structured output, persisted-session resume in a fresh container,
cancellation, verification, all eleven containment checks, dashboard boot and complete
cleanup. The host inspects each of the four runner containers before its turn continues,
verifies its exact image, network and mounts and records its distinct container ID;
direct connections to the fixture's address must fail from runner and verification
containers, and complete broker evidence, provider CONNECT counts and zero remaining
sandboxes and leases are required.

`make test-production-images` runs this acceptance on a disposable Docker Engine 28+
host with Compose, Skopeo and OpenSSL, given the retained `dist/images/*.oci.tar`
archives. The synthetic provider sits on a separate internal bridge using
`11.255.254.0/24`, a globally numbered range reserved locally for the test (it contacts
nothing there); the gateway is the only deployment service that also joins that bridge,
the provider publishes no host port, and using a globally classified address keeps the
gateway's private-address denial in force. Temporary keys, provider state and volumes are
removed at teardown.

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
