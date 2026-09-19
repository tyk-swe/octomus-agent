# M10 — Perform authorized cutover and retire Rust

[Roadmap](README.md) · **Depends on:** [M9](m9-release-and-migration.md) and explicit
operator authorization · **Completes:** the backend rewrite

## Deliverable

An operator-authorized Go deployment with real run evidence, followed by removal
of the obsolete Rust backend and migration scaffolding.

M0–M9 readiness does not authorize deployment, live model calls, GitHub writes, or
resuming ordinary operation. Use the owner’s dedicated deployment environment,
repository, bot, and configured runtime routes for the authorized canary.

## Cutover sequence

1. Pause the Rust service and wait for tasks, planning, baseline work, and
   publication activity to become idle. Pausing alone does not stop active work.
2. Stop the service and verify that its owned processes have exited.
3. Take and verify a consistent backup of required state and workspaces. Preserve
   permissions and native runner-session storage needed for recovery.
4. Install the qualified Go artifact and start it against the same state paths in
   paused operation.
5. Check history, configuration, usage, run evidence, model diagnostics, and
   baseline behavior before admitting new work.
6. Run one explicitly authorized, bounded canary using the owner’s repository,
   bot, and configured routes.
7. Confirm the exact reviewed and verified commit is the delivered PR head.
8. Restart while paused and verify durable state and publication reconciliation.
9. Resume ordinary operation only after the owner accepts the recorded outcome.

Do not simplify product configuration or substitute runtime model routes for the
canary. Record live evidence separately from fixtures and synthetic providers.

## Rollback rules

Before Go has made external writes or recorded new durable work, the verified
pre-cutover backup can support a controlled restore.

After Go has pushed, created a PR, or recorded new durable work, do not blindly
restore the old database. Restoring would discard checkpoints without undoing
GitHub side effects or accounting for intervening work.

Switch back to Rust using current state only when the M9 rehearsal proves it
safe. Otherwise stop admissions, preserve current evidence, reconcile remote
effects, and use a forward fix or an explicitly validated recovery procedure.

## Acceptance criteria

1. The owner-authorized canary has real evidence, distinct from fixture and
   synthetic-provider results.
2. There is no state loss, unexpected model substitution, duplicate publication,
   orphaned owned process, or unexplained migration difference.
3. The Go application remains correct after restart.
4. After replacement coverage and distribution functions are in place, remove
   Rust backend sources, Cargo metadata, `build.rs`, Rust-only CI steps, crate
   packaging, and obsolete migration-only helpers.
5. README, AGENTS, contributing, deployment, release, and architecture documents
   accurately describe the Go implementation.
6. Octomus’s own build and ordinary test suite need no Rust toolchain. Preserve
   Rust-related examples or environment paths still needed for managed
   repositories.
7. The final clean checkout passes Go build, test, audit, package, and installation
   gates. Temporary duplicate targets are removed unless they retain a documented
   purpose.

## Verification and evidence

Record owner authorization, qualified artifact identity, backup verification,
redacted canary evidence references, exact commit relationships, restart results,
and owner acceptance. Never commit credentials or private runner transcripts.

After retiring Rust, run the authoritative `make check`, `make test`, `make audit`,
`make build`, and `make package` targets and installation checks from a clean
checkout. Confirm that no hidden Rust dependency remains in Octomus’s build or
ordinary tests.

Without authorized deployment access, M10 cannot complete. Once work reaches that
boundary, record `BLOCKED` and the required access/authorization. Completed M0–M9
may be reported as `IMPLEMENTATION_READY`; the full roadmap remains incomplete.

## Progress record

```text
Milestone: M10
Status: TODO
Implementation revision: Not started.
Delivered output: None.
Acceptance tests and commands: Not run.
Results: No cutover, live-canary, or Rust-retirement evidence recorded.
Intentional behavior differences: None approved.
Unrun required checks and blockers: All acceptance checks unrun; depends on M9 and explicit owner authorization/access.
Next eligible milestone: None; complete the roadmap only after every M10 criterion passes.
```
