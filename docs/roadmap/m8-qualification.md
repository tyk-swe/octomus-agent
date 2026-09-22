# M8 — Qualify behavior, failure handling, concurrency, and scale

[Roadmap](README.md) · **Depends on:** [M7](m7-operator-experience.md) ·
**Enables:** [M9](m9-release-and-migration.md)

## Deliverable

A feature-complete Go candidate with passing consolidated tests and a concise
compatibility and measurement record in this milestone. Qualify the integrated
implementation using existing scenarios and fixtures; do not create another
verification framework.

## Acceptance criteria

1. Every preserved contract has passing coverage. Every original behavior-test
   group is ported, reused, or replaced by an explicitly equivalent assertion.
   Reconcile the inventory in M0 with final coverage.
2. Executable-selectable fixture suites pass against the Go candidate.
3. Differential fixtures have no unexplained semantic differences. Intentional
   corrections have named regressions and recorded rationale.
4. Go race tests pass, including selected concurrency-heavy Python scenarios
   against a race-instrumented service binary.
5. Failure injection covers every row of the matrix below with the required
   outcome.
6. Large-history behavior remains indexed and bounded. Port both bounded-history
   and duplicate-lookup cases from `tests/history_scale.rs`.
7. Repeated start/cancel/shutdown tests leave no owned child processes or steadily
   growing descriptor/goroutine population.
8. Build and operational measurements follow the frozen protocol. All required
   budgets pass; report the incremental-build improvement target honestly.
   Synthetic model latency is not evidence of real-provider performance.

## Required failure matrix

| Failure point | Required result |
| --- | --- |
| Before/after admission transaction commit | Counter and ledger agree; no extra admission appears. |
| During planning persistence | No executable partial plan survives. |
| During concurrent retry/cancel actions | One valid action wins; cancellation intent and checkpoints are retained. |
| During runner startup or event streaming | Bounded failure, no fabricated completion, owned processes cleaned up. |
| During review or verification | Incomplete or stale evidence cannot authorize publication. |
| After output checkpoint but before push | Retry inspects saved evidence and remote state first. |
| After push or PR creation but before local acknowledgement | Reconciliation recognizes completed work or blocks ambiguity. |
| During notification delivery | Stable event identity and bounded retries survive restart. |
| During DB write, lock contention, or simulated disk exhaustion | No successful acknowledgement for an uncommitted operation. |
| During cleanup or with an unsafe workspace path | Evidence is retained and unrelated paths are untouched. |

## Measurement protocol

M0 freezes this protocol and its budgets before Go results are known. Use the same
host, fixed fixture data, equivalent optimized builds, and the same prebuilt
frontend. Separate dependency-download time from compilation. Record toolchains,
build flags, host details, sample counts, warmup, and timing method so comparisons
are reproducible.

Measure backend clean build, incremental build, idle RSS, and `/api/state` latency
at 1,000, 10,000, and 100,000 historical tasks. Compare equivalent incremental
changes and report measured results rather than claiming a language-level speedup.

The proposed required budgets are:

- At 100,000 tasks, Go `/api/state` p95 is no greater than the larger of twice the
  Rust reference p95 or 50 ms.
- Go `/api/state` p95 grows by no more than 2× between the 1,000- and 100,000-task
  fixtures.
- State-response size grows by no more than 5% across those fixtures.

Backend incremental-build median should improve over Rust. Record the actual
result as an improvement target, alongside clean-build time and idle RSS; those
metrics have no additional numeric budget in this proposal.

These are proposed acceptance targets, not obtained measurements. Any budget
change must include a rationale and its impact on qualification in this record.
Never silently relax a failed gate.

## Verification and evidence

Run the consolidated Go commands from the
[verification entry points](README.md#verification-entry-points), shared fixture
suites, frontend checks, and pinned-client contract jobs. Instrument the actual
service binary used in selected Python concurrency tests, as well as Go unit
tests.

Record the final inventory reconciliation, differential results, failure-matrix
coverage, race/leak results, and measurements here with stable evidence references.
Required skipped checks prevent `DONE`.

## Progress record

```text
Milestone: M8
Status: IN_PROGRESS
Implementation revision: Underway on the Go qualification branch.
Delivered output: None.
Acceptance tests and commands: Not run.
Results: No qualification or measurement evidence recorded; budgets await M0 freeze.
Intentional behavior differences: None approved.
Unrun required checks and blockers: All acceptance checks unrun; depends on M7.
Next eligible milestone: M9 after M8 is DONE.
```
