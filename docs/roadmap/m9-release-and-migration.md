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
Status: TODO
Implementation revision: Not started.
Delivered output: None.
Acceptance tests and commands: Not run.
Results: No release-candidate or migration-rehearsal evidence recorded.
Intentional behavior differences: None approved.
Unrun required checks and blockers: All acceptance checks unrun; depends on M8; native amd64/arm64 and systemd environments required.
Next eligible milestone: M10 after M9 is DONE and the owner authorizes cutover.
```
