# Releasing

Public GitHub releases remain pending. Local packages, fixture tests and
workflow definitions do not prove public download availability.
Employer/name clearance and LICENSE/NOTICE ownership facts remain owner gates.

## Version

The application version lives in the `VERSION` file at the repository root. The
Go binary embeds it at compile time, so `--version`, `/healthz` and runner
client metadata all report the same string, and release tooling reads the same
file for tag validation and archive names. Bump it in one place only.

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
the executable needs neither Rust, Go, Node, nor a minimum glibc at runtime.

The archive contains `octomus-agent/octomus-agent`, license, policies and operator
documentation. It never packages live state or separate runtime dashboard files.
The installer validates a single matching SHA-256 entry before extracting only
the executable, then replaces the destination binary. Set an absolute writable
`INSTALL_DIR` to avoid sudo. Downloads and checksums come from the same release;
release archives do not include Sigstore signatures.

## GitHub release workflow

After owner clearance, a pushed `v*` tag runs the reusable full checks and native
Ubuntu 24.04 builds on x86_64 and aarch64. The tag must equal `v` plus the
`VERSION` file contents. Both tarballs must pass embedded HTTP and installer
smoke tests before the publishing job receives contents-write permission.
Checksums cover both archives; generated release notes are the default.
Prerelease tags are marked as prereleases and excluded from the installer's
latest-stable lookup.

The manual workflow accepts an existing tag and optional multiline notes. Use
that path with the final owner-written v0.1.0 notes. If the release already exists,
nonempty supplied notes update only its description; published assets are never
replaced. A rerun without notes fails for an existing release.
The workflow does not push tags, merge PRs, change visibility or publish crates.
Before enabling live workers, configure release-tag protections so their GitHub
identity cannot trigger a release by pushing a tag. These repository controls
are an owner setup action; worker prompts are not an authorization boundary.

## Rust reference

Octomus was originally implemented in Rust. The Go service replaced it, and the
Rust sources were removed from this tree. The frozen Rust reference is commit
`3c2b5cd`; commit `529b63f` is the last with the Rust tree and the cross-language
storage, upgrade and lock checks (`make test-go-storage`). Build the reference from
either commit only for a rollback under the
[cutover rules](roadmap/m10-cutover-and-retirement.md#rollback-rules); Go-written
state was rehearsed as Rust-resumable at that revision.
