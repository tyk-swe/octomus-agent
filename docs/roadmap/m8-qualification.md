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
Status: DONE
Implementation revision: tyk/go-m8-m10 @ 4de80ee (qualification commits 1c395cd,
  3f4e92c, e7adf30a merged there); M8 added tests and the measurement harness only —
  the single production-code change is the StateView correction below.
Delivered output: internal/store/scale_test.go (both history_scale ports),
  internal/store/full_test.go (disk-exhaustion and transaction rollback),
  internal/httpapi/cycles_test.go (cycle evidence auth + detail-after-archive),
  internal/runner/smoke_test.go (OpenCode protocol smoke), internal/engine/
  lifecycle_test.go (repeated lifecycle leak test), tests/go_measure.py (frozen
  measurement protocol), `make build-race` + bin/octomus-agent-race, and the
  committed-cycle-activity fix in internal/engine/api.go.
Acceptance tests and commands: All run against the Go executable
  (OCTOMUS_TEST_BINARY=bin/octomus-agent) unless named otherwise.
  - `make check`: PASS (gofmt clean, go vet, Svelte/TS, showcase, site, Prettier).
  - `go test ./...`: PASS all packages.
  - `CGO_ENABLED=1 go test -race ./...`: PASS all packages (engine 243s,
    notifications 132s, store 154s under race detector).
  - Shared Python fixture suites (executable-selectable): compatibility_capture
    PASS; go_foundations --go-m1 PASS (84 frozen CLI cases); evidence_snapshot
    PASS; e2e 27 scenarios PASS; e2e_baseline 13 PASS; e2e_notifications 5 PASS
    (deliver, restart, service-error, env-strip, env-strip-opencode);
    e2e_runners 14 PASS; e2e_hardening 22 modes + reconciliation deadline PASS
    (full rerun after the StateView fix; audit-absorbed previously exposed the
    visibility window and now passes 9/9 focused reruns plus the full suite).
  - distribution.py PASS; package_guards.py PASS; go_upgrade.py PASS.
  - Race-instrumented service (bin/octomus-agent-race, GORACE=halt_on_error=1):
    14 concurrency-heavy Python scenarios PASS — parallel, interrupt-publication,
    cancel-route, cap1-interrupt, dependencies, unordered, chain, stale-retry,
    supersede, obsolete, fork, interrupt-planning, audit-queued,
    cancel-restart — zero race reports.
  - `OCTOMUS_SCALE_TEST=1 go test ./internal/store -run Scale`: both ports PASS.
Results: (1) Inventory reconciliation: every original Rust behavior-test group is
  covered — ported verbatim where Rust-specific (history_scale -> scale_test.go;
  contracts OpenCode smoke -> runner/smoke_test.go), reused unchanged (all Python
  fixture suites are executable-selectable and run against the Go binary), or
  replaced by named equivalent Go assertions (engine/store/httpapi mirror tests,
  enumerated per-milestone in M2–M7 records). Three partial gaps closed here:
  disk-exhaustion injection, cycle evidence route auth/unknown-cycle isolation,
  cycle detail readability after archival. (2) Differential fixtures: /api/state
  responses byte-identical to the frozen Rust reference at 1k/10k/100k tasks;
  exports byte-equal apart from generated_at (M2 record). (3) Failure matrix —
  every row has named tests:
  admission commit: TestFailedAdmissionsRollBackCounterAndLedgerTogether,
    TestAdmissionAndCounterCommitTogetherAcrossDaysAndRestarts,
    TestPlanningAdmissionBudgetIsAtomicUnderConcurrency;
  planning persistence: TestFailedPlanningCommitsNoPartialQueueOrDecisionMemory,
    e2e failed-discovery;
  concurrent retry/cancel: engine TaskAction conflict tests, e2e cancel-route,
    stale-retry, supersede, cancel-restart;
  runner startup/streaming: e2e failed-start, missing-executor-session,
    missing-repair-session, TestOpenCodeFailuresNeverReturnSuccessfulEvidence;
  review/verification: e2e malformed-review, incomplete-review,
    failed-verification, TestExecutionMalformedAndIncompleteReviewsNeverPublish,
    TestExecutionFailedVerificationExhaustsRepairBudget;
  checkpoint before push: e2e interrupt-publication,
    TestExecutionRestartReconcilesPublicationCheckpoint;
  push/PR before ack: e2e published-duplicate, published-case-change,
    published-trimmed-title, closed-after-publication, reconcile-controls,
    TestPublicationChecksEveryIdentityFieldAndClosedReconciliation;
  notification delivery: e2e_notifications restart (stable identity + bounded
    retries across restart), TestDeliveryTimeoutIsBoundedAndVisible,
    TestSlowDeliveryDoesNotCauseACatchUpBurst;
  DB write/lock/disk exhaustion: TestDiskFullRecordWriteAcknowledgesNothing
    (PRAGMA max_page_count injection), TestFailedLedgerWritesRollBackTheWholeTransaction;
  cleanup/unsafe paths: TestCleanupLeavesUnrelatedProcessesUntouched,
    TestBaselineCleanupRemovesTheOwnedCloneAndRefusesSymlinks,
    TestPausedHousekeepingPreservesUnresolvedEvidenceAndRejectsSymlink,
    RemoveOwnedDir refusal tests.
  (4) Scale: TestBoundedHistoryScale and TestDuplicateHistoryScale pass with flat
  ~790KB total-alloc growth 1k->100k rows; indexed duplicate lookup stays bounded.
  (5) Leaks: TestRepeatedLifecycleLeavesNoLeaks (18 start/cancel/shutdown
  iterations: fd delta 0, goroutines <= +2) and TestStartupFailureLeaksNothing;
  e2e scenarios assert owned child-process reaping through /proc.
  (6) Measurements per the frozen M0 protocol (same host, fixed fixtures,
  equivalent optimized builds — Rust --release --locked vs Go -trimpath
  -ldflags=-s -w — same prebuilt frontend; raw record at
  ~/octomus-work/notes/m8-measurements.md): clean-build medians over 5 fresh
  target/cache dirs — Rust 192.0s, Go 27.2s; incremental medians over 10 timed
  builds after a function-body literal change (1 discarded warmup) — Rust
  90.65s, Go 1.42s (63.9x improvement, reported target only); /api/state over 5
  fresh service runs x 1000 timed sequential requests after 100 warmups —
  Rust p95 10.17/9.99/10.21ms and Go p95 28.74/28.65/28.56ms at 1k/10k/100k;
  the 100k budget max(2x Rust p95, 50ms) = 50ms is met (28.56ms); p95 growth
  0.99x <= 2x; response bytes identical to Rust at every scale (96450/96752/
  97054 B, +0.63% <= 5%); idle VmRSS after 30s — Rust ~14-15MiB, Go ~21-23MiB
  (reported; no budget). Synthetic fixture latency is not provider-performance
  evidence.
Intentional behavior differences: Consolidated across milestones — (a) M2: Go
  refuses user_version > 6 before any write; WAL/synchronous applied after the
  check; go_foundations read-only expectations. (b) M3: Capture bounds post-kill
  joining at 30s; Deadline struct; Duration wrap semantics; RemoveOwnedDir message
  site. (c) M4: explicit Close + context ownership; extra OpenCode policy checks;
  16MB outbound Codex JSON ceiling. (d) M5/M6: contexts/mutexes/Shutdown instead
  of tokens/drop; engine-level operator controls. (e) M7: audit launch drops the
  remote-preflight gate ordering; grounding fingerprint recheck; notifications
  worker binding; doctor stderr. (f) M8 additions: scale tests substitute
  runtime.ReadMemStats for the Rust global allocator (OCTOMUS_SCALE_TEST gated);
  StateView counts a committed running cycle record as active and derives its
  mode, closing the window where /api/state showed cycle_active=false beside a
  durable running cycle (regression: e2e_hardening audit-absorbed; rationale:
  the durable record is the authoritative committed fact); OpenCode smoke runs
  under OCTOMUS_OPENCODE_SMOKE_BINARY; disk exhaustion injected via PRAGMA
  max_page_count.
Unrun required checks and blockers: None. Every required check ran and passed on
  the recorded revision. Synthetic fixtures only; no live provider, account, or
  production claims.
Next eligible milestone: M9 (already merged on this branch; its record stands in
  m9-release-and-migration.md).
```
