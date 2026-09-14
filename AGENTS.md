# Working on Octomus Agent

Octomus is a single-operator Rust service that discovers repository improvements,
reviews proposals, executes accepted tasks through Codex or OpenCode, and delivers
GitHub PRs. The SvelteKit dashboard builds to static assets served by Rust.

## Repository map

- `src/engine.rs`: scheduler and cycle orchestration, with `src/engine/planning.rs`
  (discovery and proposal review), `execution.rs` (execution, review, repair and
  verification), `memory.rs` (grounding and history) and `housekeeping.rs`
  (retention, workspace and disk limits).
- `src/runner.rs`: runner-neutral model discovery, exact routing and dispatch;
  `src/codex.rs` (app-server protocol) and `src/opencode.rs` (HTTP/SSE) implement it.
- `src/config.rs`, `src/model.rs`, `src/store.rs` with `src/store/queries.rs`:
  policy, durable records, SQLite and indexed operational views.
- `src/report.rs`: read-only usage reporting; `src/evidence.rs`: read-only
  `RunEvidenceV1` export; `src/api.rs`: authenticated controls; `src/assets.rs`:
  embedded dashboard serving; `src/schemas.rs`: structured output.
- `src/git.rs`, `src/process.rs`: Git/GitHub publication and owned process groups.
- `web/src`: dashboard, shared TypeScript types, settings, setup checklist and
  run/task evidence. `web/showcase`: standalone public showcase build of an approved
  evidence wrapper (`docs/launch/showcase.md`).
- Rust behavior tests: `tests/core.rs`, `usage.rs`, `runners.rs`, `hardening.rs`,
  `review_findings.rs`, `review_regressions.rs`, `evidence.rs`, `process_lifecycle.rs`,
  `history_scale.rs`, and `contracts.rs` (ignored unless a pinned real client binary
  is provided).
- `tests/e2e.py`, `e2e_runners.py`, `e2e_hardening.py` with `tests/fixtures`:
  deterministic Codex/OpenCode/GitHub peers with real local Git. `distribution.py`,
  `crate.py`, `crate_guards.py` and `systemd.py` cover packaging and deployment;
  `evidence_snapshot.py` runs the documented backup and export examples on synthetic data.
- `web/tests`: dashboard browser tests served by `tests/serve_ui.py`;
  `web/showcase-tests`: showcase contract and browser tests. `docs/architecture.md`
  describes the operating contract.

## Build and verify

Use Rust 1.88+ with rustfmt/clippy, Node 22.12+, npm, Python 3, Git and a C compiler.
Install dashboard dependencies with `npm ci --prefix web`. Install browser test
prerequisites with `npx --prefix web playwright install --with-deps chromium`.

- `make check`: Rust formatting/clippy, Svelte/TypeScript, showcase and Prettier checks.
- `make test`: Rust tests, debug binary, dashboard build, integration, browser and
  showcase tests.
- `make build`: production binary and dashboard.
- Focused integration: `npm run build --prefix web`, `cargo build --locked`, then
  `python3 tests/e2e.py`. These tests use fixtures, not live accounts or model calls.

Run relevant behavior tests while editing and the full checks before delivery.
Keep Rust and dashboard configuration types aligned. Preserve backward-compatible
loading of saved configuration and task snapshots when adding fields.

## Conventions and boundaries

- Name branches created by Octomus `tyk/{branch-name}`. Never use `codex/`.
- Make cohesive changes with meaningful verification. Preserve existing features,
  full-diff review, fresh reviewer threads and persistent per-task repair threads.
- Never silently substitute model/effort routes or weaken verification to publish.
- The service delivers PRs; it does not merge, deploy or migrate production systems.
  Workers must not push or publish; the Rust orchestrator owns publication.
- Do not edit `.octomus/`, credentials, account configuration, or other workspaces.
  Fixtures and generated build artifacts are not live evidence.
- Keep secrets, raw runner transcripts and private billing screenshots out of Git.
  Record redacted observations and evidence references instead.
- Do not claim live validation without real evidence. Live operation needs
  the owner's dedicated VM and bot; the development workspace is not that VM.
- Defer LICENSE/NOTICE ownership edits until the owner supplies cleared facts.
