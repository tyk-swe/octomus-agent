# M7 — Complete the operator experience and operational features

[Roadmap](README.md) · **Depends on:** [M6](m6-execution-and-publication.md) ·
**Enables:** [M8](m8-qualification.md)

## Deliverable

The complete authenticated API, existing dashboard integration, baseline checks,
notifications, housekeeping, history, reports, run evidence, and graceful shutdown.

Complete the API controls introduced in M5/M6 against the frozen reference. Keep
Rust and dashboard contracts aligned during coexistence, then preserve those
contracts in Go. Do not rewrite Svelte components to conceal incompatible API
output.

## Acceptance criteria

1. Every route in reference `src/api.rs` has compatible methods, success/error
   shapes, status semantics, and action eligibility.
2. Preserve bearer-token validation, authentication backoff, valid-token
   responsiveness, mutation content-type checks, response redaction, security
   headers, and configured body/output bounds.
3. `/healthz`, embedded assets, `--assets`, HEAD requests, missing assets, encoded
   traversal attempts, and unknown API routes behave correctly. API errors cannot
   fall through to the SPA.
4. Baseline checks remain explicit paused/idle operations in disposable clones.
   They create no task, cycle, model admission, or publication evidence.
5. Baseline fingerprint freshness, revision freshness, bounded output,
   cancellation, restart interruption, and cleanup failure remain distinct.
6. Webhook enqueueing remains transactional. Preserve destination identity and
   rotation, stable event IDs, bounded retries, timeout behavior, backlog limits,
   and frozen payloads.
7. Lost acknowledgement may cause webhook retransmission; preserve that behavior
   without claiming exactly-once external delivery.
8. Housekeeping works while paused, retains unresolved evidence, respects
   archive/discard semantics, rejects unsafe paths, and keeps application storage
   distinct from runner storage.
9. `RunEvidenceV1` preserves reviewer slots, latest-review/latest-command semantics,
   explicit gaps, and cycle-scoped proposal identity. Compute evidence facts before
   presentation redaction.
10. Shutdown drains owned activity within the systemd stop budget and leaves
    restart-recoverable records when interrupted. Accepted durable work belongs to
    the application lifecycle, independent of browser connection lifetime.

## Verification and evidence

Run `tests/e2e_baseline.py`, `tests/e2e_notifications.py`,
`tests/evidence_snapshot.py`, and the dashboard, showcase, and site suites.
Port remaining cases from `tests/baseline.rs`, `tests/notifications.rs`,
`tests/evidence.rs`, and API/history tests allocated in M0.

Record API compatibility, evidence semantics, frontend results, and shutdown
behavior. Retain distinct outcomes for task verification and explicit baseline
checks; neither may stand in for the other.

## Progress record

```text
Milestone: M7
Status: TODO
Implementation revision: Not started.
Delivered output: None.
Acceptance tests and commands: Not run.
Results: No acceptance evidence recorded.
Intentional behavior differences: None approved.
Unrun required checks and blockers: All acceptance checks unrun; depends on M6.
Next eligible milestone: M8 after M7 is DONE.
```
