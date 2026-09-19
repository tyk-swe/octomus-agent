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
Status: TODO
Implementation revision: Not started.
Delivered output: None.
Acceptance tests and commands: Not run.
Results: No acceptance evidence recorded.
Intentional behavior differences: None approved.
Unrun required checks and blockers: All acceptance checks unrun; depends on M2, M3, and M4.
Next eligible milestone: M6 after M5 is DONE.
```
