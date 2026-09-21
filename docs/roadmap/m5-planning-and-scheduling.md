# M5 — Port planning, scheduling, and admission control

[Roadmap](README.md) · **Depends on:** [M2](m2-storage-and-exports.md),
[M3](m3-processes-and-git.md), [M4](m4-runner-adapters.md) ·
**Enables:** [M6](m6-execution-and-publication.md)

## Deliverable

The scheduler, durable operating modes, planning pipeline, audits, proposal
validation, decision memory, PR inventory/capacity handling, and the API controls
needed to operate them.

Preserve the distinction between complete planning and delivered work. Accepted
proposals are not successful deliveries. Preserve repository grounding, source
attribution, external PR context, and maintenance cadence from the reference.

## Acceptance criteria

1. Discovery preserves the configured eight-to-ten agents, independent
   `adversary-a` and `adversary-b` reviews, and orchestrator consolidation.
2. Every original proposal has an accounted-for decision. Reject unknown
   dependencies, cycles, invalid targets, duplicate work, and invalid same-PR
   ordering.
3. Planning and review clones remain unchanged, with independent workspace and
   session identities.
4. Successful cycle, accepted queue, decision memory, lineage, and RunOnce phase
   transition commit atomically. Interrupted planning cannot dispatch a partial
   plan after restart.
5. Audits require paused/idle eligibility, leave existing queued work untouched,
   and never dispatch accepted proposals.
6. Paused, RunOnce, and Continuous retain their distinct durable behavior. Later
   retries cannot erase a one-shot failure or silently join an earlier batch.
7. Planning affordability checks precede side effects. Per-role admission reserves
   atomically against the current UTC day and live policy.
8. PR capacity uses complete owned-open inventory plus unrepresented durable
   reservations, independently of dashboard windows. Stale or failed observations
   cannot authorize new work.
9. Existing-PR and already-reserved tasks remain selectable behind a full queue of
   new-PR tasks. Same-branch writers serialize.
10. Slow remote preflights do not block unrelated controls. Revalidate current
    authoritative state before using preflight results to commit an action.

## Verification and evidence

Port relevant cases from `tests/core.rs`, `tests/hardening.rs`,
`tests/pr_capacity.rs`, `tests/pr_context.rs`, and decision-memory inline tests.
Use the M0 inventory to account for all planning and admission regressions.

Use synchronization barriers and controlled clocks where needed. Race tests must
not rely on arbitrary sleeps. Record atomicity, restart, and concurrent-control
evidence alongside normal planning outcomes.

## Progress record

```text
Milestone: M5
Status: DONE
Implementation revision: Working tree based on d819651; frozen behavior reference remains 3c2b5cd50924033873d7f740f9df44daee5685db.
Delivered output: internal/engine with durable Paused, RunOnce and Continuous scheduling; restart recovery and housekeeping; complete grounding, discovery, independent adversarial review, consolidation and audit pipelines; proposal, dependency and shared-branch validation; decision memory and rediscovery; complete PR inventory, process-local capacity authority and durable reservation reconciliation; unlocked remote preflights with authoritative revalidation; and the narrow TaskRunner boundary required by M6. Supporting store transactions atomically admit planning sessions, start affordable RunOnce batches, begin affordable cycles, commit complete plans and admit new-PR tasks. The Git package exposes the existing read-only publication lookup and task-marker checks needed for reservation reconciliation. Unit, race, deterministic fixture-integration and cross-language storage tests cover the milestone contract.
Acceptance tests and commands: go test ./internal/engine ./internal/store ./internal/git -count=1; CGO_ENABLED=1 go test -race ./internal/engine ./internal/store -count=1; make check-go; make test-go; make test-go-storage; make check; make test; git diff --check.
Results: PASS on Linux amd64 with Go 1.27.1, Rust 1.98.0, Node 26.8.2, npm 11.19.1 and Python 3.14.4. Deterministic Codex/Git/GitHub fixtures prove a complete pass uses the configured discovery-agent count plus grounding, adversary-a, adversary-b and consolidation, with one unique session and unchanged independent workspace per role; a deliberately mutated planning clone fails closed, preserves terminal evidence and queues no work. Proposal accounting, invalid targets, duplicates, dependency cycles and total same-PR ordering fail closed. Complete-plan commit, decision memory, lineage and RunOnce phase changes are atomic; interrupted Planning pauses while committed Executing survives restart. Audit isolation, immutable batch membership, late retry failure accounting, UTC-day admission concurrency, live-policy affordability, complete inventory plus reservation capacity, stale/error/cancellation fail-closed behavior, queue-window independence, dependency/branch serialization and unlocked stale-preflight rejection all passed. Full Go unit and race suites, frozen Rust M5 core/hardening/PR-capacity/PR-context coverage, deterministic Python E2E, cross-language storage, packaging, dashboard, showcase and public-site stages passed. One initial full make test attempt hit a timing-sensitive process-lifecycle fixture after its descendant had already exited; its focused four-case rerun and the subsequent complete make test rerun both passed.
Intentional behavior differences: No planning, scheduling, admission or capacity contract difference. Go uses contexts, ordinary mutexes and explicit Shutdown ownership in place of Rust cancellation tokens and drop semantics. M5 exposes engine-level operator controls; authenticated HTTP routing and service startup remain assigned to M7, and task execution/publication remains behind the M6 TaskRunner boundary. Rust remains the default service through M8.
Unrun required checks and blockers: None. No live model, credential, GitHub write, production operation, release or cutover was performed or required; all integration evidence used deterministic local fixtures and real local Git.
Next eligible milestone: M6.
```
