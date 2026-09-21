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
Status: DONE
Implementation revision: Working tree based on fb71dae; frozen behavior reference remains 3c2b5cd50924033873d7f740f9df44daee5685db.
Delivered output: internal/engine/baseline.go owns explicit clean-baseline checks — expected-config comparison against the saved fingerprint, eligibility serialization under the scheduler gate, a disposable remote clone at the recorded default-branch revision, per-command bounded UTF-8-safe output plus an aggregate cap, durable cancel intent, overall and per-command deadlines, refusal-safe owned-directory cleanup, default-branch observation freshness, and crash/shutdown recovery into cancelled or interrupted records. scheduler.go, actions.go, engine.go and housekeeping.go wire the baseline slot into idle, drained, shutdown, tick, reconciliation and the housekeeping cleanup loop; the default-branch observation moved to the reference's in-memory runtime field. internal/httpapi/ serves the complete authenticated API — every reference route with method-aware dispatch, bearer-token hash comparison with bounded exponential failure backoff, mutation content-type enforcement, the 256 KiB body bound with axum-shaped rejections, JSON redaction on matched responses, security headers, /healthz, embedded dashboard serving with the reference's SPA-fallback rules and --assets override serving — plus api.go/engine support for state view, history, proposals, evidence, config save, control/cycle/task actions, baseline start/latest/detail/cancel, doctor and model catalog. internal/notifications/ delivers the durable webhook worker — URL policy, destination fingerprinting, transactional outbox claiming, bounded retry schedule, terminal failure and expiry handling under the service shutdown scope. cmd/octomus-agent/main.go runs production startup — data-directory permissions, service lock, store, doctor mode, operator token validation, asset checks, notification worker, listener and graceful shutdown with a bounded drain. Go ports of the reference baseline, notifications and core API tests were added alongside the frozen Rust suites.
Acceptance tests and commands: gofmt -l cmd internal (clean); go vet ./...; go build ./cmd/octomus-agent; go test ./...; CGO_ENABLED=1 go test -race ./...; cargo test --locked --test baseline --test notifications; OCTOMUS_TEST_BINARY=bin/octomus-agent-go python3 tests/e2e.py tests/e2e_baseline.py tests/e2e_notifications.py tests/e2e_runners.py tests/e2e_hardening.py tests/evidence_snapshot.py tests/go_foundations.py --go-m1 tests/go_storage.py; dashboard Playwright suite; showcase contract and browser suites; public-site suite.
Results: All Go unit and race tests pass. Reference Rust suites: 11 baseline and 18 notification tests pass. End-to-end: 27 core scenarios, 13 baseline scenarios and 4 notification scenarios pass, plus runner, hardening and evidence-snapshot suites; go_foundations reports 84 frozen CLI cases and real service startup order; go_storage confirms Go reads the Rust-upgraded fixture and Rust reads Go-written state. Dashboard: 115 passed, 1 skipped, 0 failed; showcase: 9 contract and 8 browser tests pass; public site: 16 pass.
Intentional behavior differences: The Go audit launch retains the earlier-milestone remote preflight, so control/audit drops the gate during remote work and revalidates paused+idle before committing; the reference starts the audit worker directly under the gate. Planning grounding keeps the whole-inventory fingerprint recheck (stricter than the reference's remote-identity check) while the default-branch observation merges at the reference's earlier point under same_remote_identity. The notifications worker binds to the app context rather than a child cancellation token; identical shutdown semantics. Doctor warnings go to stderr to match tracing::warn! in addition to the JSON response.
Unrun required checks and blockers: Distribution, systemd and release-package gates are M9 scope and unrun by design; no M7 acceptance check remains unrun.
Next eligible milestone: M8.
```
