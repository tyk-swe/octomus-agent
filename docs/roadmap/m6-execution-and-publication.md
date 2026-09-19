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
Status: TODO
Implementation revision: Not started.
Delivered output: None.
Acceptance tests and commands: Not run.
Results: No acceptance evidence recorded.
Intentional behavior differences: None approved.
Unrun required checks and blockers: All acceptance checks unrun; depends on M5.
Next eligible milestone: M7 after M6 is DONE.
```
