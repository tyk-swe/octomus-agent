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
Status: BLOCKED
Implementation revision: tyk/retire-rust @ bfc28f8 (Rust retirement), based on
  main @ 529b63f. Live cutover not started.
Delivered output: The owner authorized retiring the Rust tree before the live
  canary (2026-09-22). Removed src/, Cargo.toml, Cargo.lock, build.rs,
  tests/*.rs and tests/common, the cross-language helpers go_storage.py,
  go_upgrade.py, go_measure.py and tests/go/rollbackfixture, the
  test-go-storage target, the Rust CI job and cargo contract step, /target/ from
  .gitignore, and compatibility_capture.py's Rust-only --update path. The
  frozen reference stays at 3c2b5cd; 529b63f is the last commit with the
  comparison and rollback-rehearsal helpers (docs/releasing.md).
  Replacement coverage (AC4): every Rust behavior test without a clear Go
  equivalent was read and ported before deletion — pr_capacity (5 new:
  shared-branch slots, published-output fallback, fresh observations, refresh
  error clearing, cancelled/archived checkpoints never reseeded; 3 already
  covered), pr_context (7 new: inventory paging/dedupe/sort, ownership,
  fail-closed parsing, external-context bounds and sorting, target resolution,
  legacy grounding), review_findings (2 new, 1 already covered), legacy route,
  legacy cycle and held-webhook-during-scheduling ports, and whole-config route
  validation (unsupported effort, repair route, audit readiness). No port
  exposed a behavior difference.
  Remaining compatibility helpers and their purpose (AC7): internal/jsoncompat
  (byte-compatible JSON for Rust-written state), tests/fixtures/compatibility
  goldens with compatibility_capture.py and go_foundations.py (frozen
  state/API/CLI contracts). Managed-repository paths kept (AC6): the unit's
  .cargo/bin PATH, distribution.py .cargo/.rustup exclusions, cargo sample data.
  Docs (AC5): AGENTS, CONTRIBUTING, getting-started, releasing, architecture
  (fresh Go scale measurement), threat-model, showcase, README, CHANGELOG.
Acceptance tests and commands: From a clean clone of bfc28f8 with cargo/rustc
  absent from PATH: `npm ci --prefix web`; `make check` PASS; `make test` PASS
  (go test and -race all packages; compatibility_capture, go_foundations
  --go-m1 84 CLI cases, evidence_snapshot, e2e, e2e_baseline, e2e_notifications,
  e2e_runners, e2e_hardening, distribution, package_guards; dashboard browser
  115 passed / 1 desktop-only skip on mobile; showcase 8; site 16);
  `make audit` PASS (govulncheck "No vulnerabilities found", npm 0);
  `make package` PASS (amd64 archive sha256 278cba95…);
  `python3 tests/distribution.py --package …x86_64…tar.gz` PASS;
  `sudo python3 tests/systemd.py` PASS; `shellcheck install.sh scripts/*.sh` PASS.
Results: AC4–AC7 pass for the code retirement: Octomus builds, tests, audits,
  packages and installs with no Rust toolchain. AC1–AC3 are unrun: no cutover,
  live canary, restart or no-loss evidence exists, and none is claimed.
Intentional behavior differences: The worker prompt's final sentence names
  "the Octomus orchestrator" instead of "the Rust orchestrator"; nothing else.
Unrun required checks and blockers: AC1–AC3 and the cutover sequence remain
  blocked on (a) explicit owner authorization for live model calls, GitHub
  writes and a bounded canary on the owner's repository/bot/configured routes;
  (b) access to the owner's dedicated deployment environment; (c) owner
  acceptance of the recorded canary outcome. Rollback to Rust after retirement
  builds the frozen reference from 3c2b5cd and re-rehearses with the helpers at
  529b63f; the rollback rules above still govern. Native arm64 execution stays
  pending the first tagged release (M9).
Next eligible milestone: None; the roadmap completes when an authorized cutover
  supplies real evidence.
```
