# M6 — Port execution and publication end to end

[Roadmap](README.md) · **Depends on:** [M5](m5-planning-and-scheduling.md) ·
**Enables:** [M7](m7-operator-experience.md)

## Deliverable

A complete task lifecycle using both runners: task controls, execution, review,
repair, verification, publication, and restart reconciliation.

Preserve independent isolated clones. Keep task context immutable and publication
authority in the orchestrator. Use fixture GitHub peers for implementation tests;
real publication remains separately authorized.

## Acceptance criteria

1. Tasks retain their saved configuration, route, verification commands, source
   context, and comparison base.
2. Explicit retry adopts the intended current attempt limits without rewriting
   immutable context. Automatic recovery retains its existing policy semantics.
3. Every code-review round uses a fresh session and the full accumulated diff.
   Repairs use a separate persistent per-task repair session.
4. Incomplete or malformed reviews cannot become clean. Verification runs only
   after a valid clean review.
5. Every configured verification command passes on the reviewed commit.
   HEAD/tracked-state mutation records failed evidence and stops remaining
   commands.
6. No-progress limits, repair limits, cancellation, dependency advancement,
   stale-base detection, and workspace initialization survive restart correctly.
7. Before publication, persist the output checkpoint and validate repository
   identity, ownership, source/default revisions, branch, ancestry, worktree,
   review, and verification. Reject empty net changes and stale evidence.
8. Push only the assigned `tyk/` branch using an exact lease and the trusted
   configured checkout’s destination. Never use an agent-editable task remote as
   publication authority or create a `codex/` branch.
9. PR creation and follow-up updates preserve task markers, maintainer
   descriptions, and final repository/base/head validation.
10. Restart after push, PR creation, comment submission, or lost acknowledgement
    reconciles remote state before another write. Handle closed/merged matches and
    ambiguous matches explicitly.

## Verification and evidence

Run applicable Python end-to-end suites against Go. Port
`tests/review_findings.rs` and publication/recovery cases from
`tests/hardening.rs`, plus the M6 cases allocated in M0.

Required fixture publication outcomes are one new PR, one owned-PR follow-up,
rejected stale context, rejected unowned target, and restart recovery without
duplicate publication. Assert that the reviewed commit, verified commit, output
checkpoint, and delivered PR head agree.

## Progress record

```text
Milestone: M6
Status: DONE
Implementation revision: Working tree based on 98e403d; frozen behavior reference remains 3c2b5cd50924033873d7f740f9df44daee5685db.
Delivered output: internal/engine/execution.go owns the complete task lifecycle — supervised dispatch with durable timeout/cancellation/failure distinction, remote-preflight and dependency validation, workspace initialization and intact-clone resumption, executor turns through Codex and OpenCode, one fresh reviewer session per round against the full accumulated diff with strict structured validation, a persistent per-task repair session, per-command verification evidence bound to the reviewed revision with worktree/HEAD integrity checks before and after each command, repair and no-progress budgets, the durable output checkpoint persisted before any publication attempt, and the publishing transition into the Git publication layer (identity, ownership, source/default revisions, branch namespace, ancestry, worktree, review and verification validation; exact-lease push of the assigned octomus/ branch to the trusted configured checkout destination only; marker-idempotent PR creation and comment-only owned-PR follow-up; remote-state reconciliation after uncertain push, PR creation or comment submission, including explicit closed/merged and ambiguous matches). internal/engine/actions.go owns operator task controls (cancel, retry, supersede, archive, discard, reconcile) with eligibility under gate, gate-released remote preflights, durable-state revalidation, live attempt-policy adoption, fresh repair-round budgets, and the publication reconciliation worker guarded by runtime.reconcilingPublication so the scheduler never plans over a branch under reconciliation. engine.go wires superviseTask as the production TaskRunner with panic recovery and the worker-exit durable-error guard; scheduler.go checks the publication gate at the same tick position as the reference. Deterministic fixture tests cover the full lifecycle on both runners, malformed/incomplete reviews, verification mutation and stream evidence, repair and no-progress budgets, remote conflicts, cancellation mid-turn, task timeout, failed executor start retry, restart reconciliation for push/PR-creation/lost-acknowledgement/closed-PR checkpoints, workspace requeue, owned-PR follow-up, dependency ordering and rollback, worker panic, supersede/archive/discard, stale-base retry rejection, concurrent retry serialization, retry policy recheck and live command-timeout adoption, and preflight gate release under concurrent controls; git_test.go covers every publication identity field and closed/merged reconciliation; store_test.go covers live policy surviving restart.
Acceptance tests and commands: go build ./...; go vet ./...; gofmt -l cmd internal tests/go web/embed*.go (empty); go test ./... -count=1; make check-go; make test-go; make test-go-storage; git diff --check.
Results: PASS on Linux amd64 with Go 1.27.1, Rust 1.98.0, Node 26.8.2, npm 11.19.1 and Python 3.14.4. go test ./... passed for every package (internal/engine 76s, internal/runner 33s). make test-go passed the complete suite under CGO_ENABLED=1 go test -race ./... (internal/engine 182s, no data races), tests/go_foundations.py --go-m1 (84 frozen CLI cases, relocated executable, real dashboard embed) and tests/evidence_snapshot.py. make test-go-storage passed cross-language storage both directions against the frozen Rust reference. Required fixture publication outcomes all hold deterministically: exactly one new PR with the task marker (TestExecutionDeliversFullLifecycle), one owned-PR follow-up appended as a comment without touching the maintainer-authored body (TestExecutionExistingPrAppendsComment, TestFixtureFollowUpAppendsComment), stale context rejected as stale_base (TestExecutionRemoteConflictBlocksStaleBase, TestRetryOnStaleBaseStaysBlocked, TestPublishRejectsStaleBase), unowned or identity-mismatched targets rejected at planning validation and at every publication identity field including head/base repository and ownership (TestRejectedInvalidTargetIsNeverExecutable, TestPublicationChecksEveryIdentityFieldAndClosedReconciliation), and restart recovery reconciles remote state without duplicate publication writes (TestExecutionRestartReconcilesPublicationCheckpoint). The reviewed commit, verified commit, output checkpoint and delivered PR head are asserted equal in the lifecycle tests.
Intentional behavior differences: No execution, review, verification, retry, cancellation, dependency, publication or recovery contract difference. Go uses contexts, ordinary mutexes and explicit Shutdown ownership in place of Rust cancellation tokens and drop semantics. Operator controls land at engine level (App.TaskAction, App.Pause) because the authenticated HTTP router and service startup are M7 scope; the engine conflicts surface the same eligibility/concurrency outcomes the reference API maps to HTTP 409. The reconcile control's baseline-check gate cannot be ported yet because baseline jobs are an M7 subsystem with no Go runtime state; every other reconcile check (active-task, checkpoint, preflight, revalidation, deadline) is implemented. The Python HTTP end-to-end suites (tests/e2e.py, e2e_runners.py, e2e_hardening.py) remain inapplicable to the Go executable — they require the --listen service allocated to M7 — so the applicable Go suites (go_foundations.py, go_storage.py, evidence_snapshot.py) were run instead, and the M6 fixture peers are exercised directly at engine level. Rust remains the default service through M8.
Unrun required checks and blockers: tests/e2e.py, e2e_runners.py and e2e_hardening.py against the Go executable, blocked on the M7 service startup they exercise; their deterministic fixture behaviors are covered at engine level as documented above. No live model, credential, GitHub write, production operation, release or cutover was performed or required; all integration evidence used deterministic local fixtures and real local Git.
Next eligible milestone: M7.
```
