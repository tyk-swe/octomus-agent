# M0 — Freeze the reference and establish executable contracts

[Roadmap](README.md) · **Depends on:** none · **Enables:** [M1](m1-go-foundations.md)

## Deliverable

A reproducible Rust reference, executable compatibility fixtures, a complete test
inventory, and fixed qualification budgets. Keep Rust as the default build and
release implementation; introduce Go alongside the existing tree.

Identify the exact source behind the supplied `octomus-agent.zip` plan. Record a
Git revision when available and a source/archive hash otherwise. A ZIP or a
package version does not establish a Git revision. If the archive is unavailable
or differs from the checkout, resolve and record the reference choice before
freezing expected output.

Pin the supported stable Go toolchain and initial dependencies, including the
candidate SQLite driver. The full driver compatibility gate belongs to
[M2](m2-storage-and-exports.md). Fix the measurement protocol and qualification
budgets proposed in [M8](m8-qualification.md#measurement-protocol) before seeing Go
measurements.

Standardize executable-based tests on `OCTOMUS_TEST_BINARY`. Extend helpers that
still hard-code the Rust binary, including `tests/serve_ui.py` and
`tests/evidence_snapshot.py`. Capture supported current and older configuration,
task records, database layouts, CLI output, API responses, and evidence exports.

## Acceptance criteria

1. The reference builds, and its existing `make check` and `make test` results are
   recorded. Identify pre-existing failures by test and failure signature.
2. Every Rust behavior-test group, including inline `#[cfg(test)]` modules, has a
   destination milestone. No assertion disappears because it is inconvenient to
   port.
3. The same Python fixture tests can select either executable without changing
   behavioral assertions.
4. Compatibility expectations come from the Rust reference and existing contracts,
   not Go-generated output approved after the fact.
5. Any behavior needing a correction instead of literal parity has a reproducer
   and expected corrected result.
6. The reference identity, pinned toolchain/dependency choices, test inventory,
   measurement protocol, and qualification budgets are recorded before dependent
   implementation begins.

## Test inventory

This is the starting allocation from the current repository. Reconcile it with
the frozen reference and assign individual cases from mixed-scope suites during
M0. Extend this table in place; do not create a separate parity document. M8
reconciles the final inventory against passing coverage.

| Reference tests | Destination |
| --- | --- |
| `src/config.rs`, `src/model.rs` inline tests | M1 |
| `src/store.rs` inline tests; `tests/usage.rs`, `tests/review_regressions.rs` | M2; allocate any orchestration cases to M5/M6 |
| `tests/evidence.rs`, `src/evidence.rs` inline tests | M2 exports and store semantics; M7 complete evidence/API integration |
| `tests/process_lifecycle.rs` | M3 |
| `tests/runners.rs`, `tests/contracts.rs` | M4 |
| `tests/core.rs`, `tests/hardening.rs` | M1–M7 by behavior; process/Git M3, planning M5, publication/recovery M6 |
| `tests/pr_capacity.rs`, `tests/pr_context.rs`, `src/engine/memory.rs` inline tests | M5 |
| `tests/review_findings.rs` | M6 |
| `tests/baseline.rs`, `tests/notifications.rs`, `src/api.rs` inline tests | M7 |
| `tests/history_scale.rs` | M8, including bounded history and duplicate lookup |
| `tests/e2e.py`, `tests/e2e_runners.py`, `tests/e2e_hardening.py` | M4–M6 as features land; consolidated in M8 |
| `tests/e2e_baseline.py`, `tests/e2e_notifications.py`, `tests/evidence_snapshot.py` | M2 exports; complete M7/M8 integration |
| `web/tests`, `web/showcase-tests`, `web/site-tests`; `tests/serve_ui.py`, `tests/serve_site.py` | M7/M8 |
| `tests/distribution.py`, `tests/crate.py`, `tests/crate_guards.py`, `tests/systemd.py` | M9; replace Rust-specific coverage before M10 retirement |
| `tests/common/`, `tests/fixtures/`, build/check/audit and CI jobs | Shared support; preserve each caller’s assertions and qualification responsibility |

## Verification and evidence

Record reference identity, tool versions, commands, results, and fixture paths in
the progress record. Keep a reproducible reference available for differential
tests and M9 rollback rehearsal.

Normalize nondeterministic IDs, timestamps, and temporary paths only where
necessary. Preserve identity relationships, ordering, Git ancestry, and
revision-equality assertions. Never normalize away the behavior under test.

## Progress record

```text
Milestone: M0
Status: TODO
Implementation revision: Not started.
Delivered output: None.
Acceptance tests and commands: Not run.
Results: No reference or qualification evidence recorded.
Intentional behavior differences: None approved.
Unrun required checks and blockers: All acceptance checks unrun; exact reference not yet frozen.
Next eligible milestone: M1 after M0 is DONE.
```
