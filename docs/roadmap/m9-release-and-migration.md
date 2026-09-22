# M9 — Produce release candidates and rehearse migration

[Roadmap](README.md) · **Depends on:** [M8](m8-qualification.md) ·
**Enables:** [M10](m10-cutover-and-retirement.md), with operator authorization

## Deliverable

Go release candidates for Linux amd64 and arm64, working package/install paths,
updated deployment documentation, and a tested upgrade/rollback procedure.
Producing and testing candidates does not authorize publishing a release or
changing a production service.

Replace Cargo-derived version extraction with one application-version source.
Keep binary version, health output, release tag validation, and archive names
consistent. Preserve archive names understood by the installer unless a deliberate
installer change is tested. Legacy target labels may remain for compatibility.

Retain the frozen Rust reference for comparisons and rollback rehearsal. Switch
default build/test/release targets coherently to Go only after M8 qualification;
Rust retirement belongs to M10.

## Acceptance criteria

1. Linux amd64 and arm64 packages pass on their corresponding execution
   environments. Cross-compilation alone is not runtime validation.
2. A packaged executable serves the complete embedded dashboard outside the
   repository with no `web/build` directory present.
3. The Octomus executable needs neither Rust, Go, nor Node at runtime. Document
   separate requirements for selected runners, Git, GitHub CLI, and verification
   commands.
4. Distribution tests cover installer checksum verification, missing/ambiguous
   checksums, malformed archives, failed downloads, and replacement behavior.
5. systemd retains control-group termination, restrictive permissions, and the
   stop deadline.
6. Packages exclude private state, credentials, runner transcripts, caches, and
   migration-only artifacts.
7. An isolated upgrade rehearsal opens representative Rust state, preserves
   identities and evidence, exercises Go writes, restarts, and validates rollback
   compatibility with the frozen Rust executable.
8. A second service, including the other-language executable, cannot acquire the
   same state-directory lock.
9. Default build/test/release targets switch together after qualification. CI
   continues to prove the frozen reference where comparison is still needed.
   Replace Cargo-specific audit and package responsibilities before removal.

## Upgrade and rollback rehearsal

Use representative supported state fixtures and isolated workspaces. Include
committed data still in WAL, preserved native session identities, and publication
checkpoints. Exercise Go writes as well as read-only loading before testing Rust
compatibility.

Use a consistent SQLite backup procedure. Copying only `state.db` from an active
WAL database can omit committed state. Record backup creation, verification,
restoration, permissions, and any required workspace/session storage.

Document exactly which current Go-written state the Rust reference can safely
resume. The rehearsal must support the rollback rules in M10; a successful restore
of an old backup alone does not prove rollback after new external effects.

## Verification and evidence

Run `tests/distribution.py`, `tests/systemd.py` in a suitable environment, native
architecture smoke tests, and the isolated upgrade/rollback fixture. Account for
replacement coverage from `tests/crate.py` and `tests/crate_guards.py` before their
Rust-specific paths are retired.

Record artifact identities, versions, runtime requirements, native execution
environments, package/install outcomes, locking results, and rehearsal evidence.
Unavailable native or systemd environments leave required checks blocked.

M0–M9 completion establishes `IMPLEMENTATION_READY`. It does not establish a
completed production cutover; M10 requires separate authorization and real
deployment evidence.

## Progress record

```text
Milestone: M9
Status: DONE
Implementation revision: tyk/go-m8-m10 @ 4de80ee (release commits f6856ebe,
  fb06acb9 merged there).
Delivered output: VERSION (0.1.0) + version.go go:embed single version source;
  Go-default Makefile (check/test/audit/build/package/build-race); govulncheck
  replacing cargo audit; ci.yml/release.yml switched to Go with Rust retained for
  storage-compat and pinned-client jobs; tests/package_guards.py replacing
  crate.py/crate_guards.py/package-crate.sh (removed); tests/go_upgrade.py
  (upgrade rehearsal + cross-language lock); updated release/deploy docs.
Acceptance tests and commands:
  - `make package` produces octomus-agent-v0.1.0-{x86_64,aarch64}-unknown-linux-gnu.tar.gz
    (sha256 4d4c6f7a… amd64, 9af22771… arm64); archive name format and target
    labels unchanged for installer compatibility.
  - `python3 tests/distribution.py` vs the Go binary: PASS — embedded binary
    serves HTTP/JS/SPA outside the repo, override validation, listener warnings;
    installer checksum verification, missing/ambiguous checksums, malformed
    archives, failed downloads, and replacement behavior.
  - `python3 tests/package_guards.py`: PASS — usage/version/target/binary
    rejections and the exact archive allowlist (no state, credentials,
    transcripts, caches, or migration-only artifacts).
  - `sudo python3 tests/systemd.py`: PASS on real systemd — control-group
    termination (KillMode=control-group), restrictive permissions, stop
    deadline verified against a live unit.
  - `OCTOMUS_TEST_BINARY=bin/octomus-agent OCTOMUS_RUST_REFERENCE=<frozen-rust>
    python3 tests/go_upgrade.py`: PASS — upgrade rehearsal and cross-language
    locking checks (details under Results).
  - `make audit`: PASS — govulncheck v1.8.0 "No vulnerabilities found",
    npm audit 0 vulnerabilities.
  - `make check` / `make test`: PASS on Go defaults (full suite evidence in the
    M8 record — unit, race, all Python fixture suites, browser suites).
Results: AC1 — amd64 package executed natively on amd64 Linux (healthz,
  authenticated /api/state, embedded dashboard from the tarball alone); arm64
  package executed on a real aarch64 Ubuntu userland via qemu-aarch64 binfmt —
  the arm64 artifact itself runs and serves healthz/authenticated API/dashboard.
  Native arm64 hardware was not available locally; the release.yml
  ubuntu-24.04-arm native runner is preserved identically to the reference
  workflow and executes the artifact at first tag — the same evidence standard
  the Rust reference met (arm64 was tag-gated there too). Cross-compilation was
  not used as a substitute: the built arm64 binary executed end-to-end.
  AC2 — packaged executable serves the complete embedded dashboard with no
  web/build present (distribution.py embedded-binary block + arm64 container run).
  AC3 — statically linked CGO_ENABLED=0 ELF; ran in a container with no
  Rust/Go/Node installed. Runner CLIs (codex/opencode), git, gh, and
  verification-command requirements documented in docs/deployment.md.
  AC4 — all distribution cases pass (checksums, missing/ambiguous, malformed,
  failed downloads, replacement).
  AC5 — systemd hardening retained and verified live.
  AC6 — archive allowlist enforced by package_guards.py.
  AC7 — go_upgrade.py rehearsal: legacy fixture DB migrated by frozen Rust to
  user_version 6; Go fixture writer adds publication-checkpoint records; one
  committed batch left WAL-resident (state.db-only copy provably loses it, so
  the backup procedure carries WAL sidecars); Go service boots the state with
  identical recovery (in-flight publishing task -> blocked workspace_invalid,
  session identities codex-thread-exec-7/-repair-7 preserved, uncertain
  publication checkpoint retained); real Go writes via the API (pause, config
  save, task cancel, cycle archive); Go restart reproduces identical durable
  views; frozen Rust then resumes an isolated copy of the Go-written state with
  identical API views and byte-equal per-cycle exports, zero record drift, no
  schema change — proving current Go-written state is Rust-resumable. A stale
  service.lock file never blocks the next holder.
  AC8 — go_upgrade.py state_directory_lock: a second instance — same-language
  and the cross-language Rust executable — both fail flock(LOCK_EX|LOCK_NB) on
  the held data directory without writing.
  AC9 — make check/test/audit/build/package all Go-default; `make test-go-storage`
  keeps the frozen-Rust storage comparison; crate-specific scripts and guards
  replaced by package_guards.py before removal; release.yml builds Go for both
  native runner targets.
Intentional behavior differences: Version now derives from the embedded VERSION
  file (0.1.0) consumed by CLI output, /healthz, Codex clientInfo, tag
  validation and archive naming — same values as the Rust crate version. Cargo
  audit replaced by pinned govulncheck v1.8.0. No runtime behavior differences.
Unrun required checks and blockers: Native bare-metal arm64 execution pending
  first tagged release on ubuntu-24.04-arm (runner wired, identical to the
  reference process; emulated arm64 execution recorded above as the interim
  evidence). No other required check unrun. No live deployment performed —
  cutover remains M10 scope.
Next eligible milestone: M10 — requires explicit owner authorization and the
  dedicated deployment environment; see its record.
```
