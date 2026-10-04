# Releasing

Public GitHub releases remain pending. Local packages, fixture tests and
workflow definitions do not prove public download availability.
Employer/name clearance and LICENSE/NOTICE ownership facts remain owner gates.

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
`scripts/release-files.txt` lists every public input besides the executable;
`tests/distribution.py --package` checks that the archive holds exactly those
files. Packaging works without Git metadata
and rejects symlinked manifest files, listed inputs and their parent directories.
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
that path with the final owner-written v0.1.0 notes. If the release already exists,
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
cosign verify ghcr.io/tyk-swe/octomus-agent:0.1.0 \
  --certificate-identity-regexp '^https://github.com/tyk-swe/octomus-agent/.github/workflows/release.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

To deploy release images instead of building locally, set `OCTOMUS_IMAGE` and
`OCTOMUS_SANDBOX_IMAGE` in `deploy/docker/.env` and pull them before `docker compose up -d`; the
broker never pulls. Image publication has not run yet: the first release will exercise it.

## State format

This release creates version-7 SQLite state. It refuses earlier database versions
before changing their schema or journal settings. Back up existing state and use
a fresh data directory when installing this release.
