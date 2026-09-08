# Working on Octomus Agent

Octomus is a single-operator Rust service that discovers repository improvements,
reviews proposals, executes accepted tasks through Codex app-server, and delivers
GitHub PRs. The SvelteKit dashboard builds to static assets served by Rust.

## Repository map

- `src/engine.rs`: scheduler, discovery, review/repair, verification and recovery.
- `src/codex.rs`: app-server protocol, exact model routes, unattended requests.
- `src/config.rs`, `src/model.rs`, `src/store.rs`: policy, durable records, SQLite.
- `src/report.rs`: read-only usage reporting; `src/api.rs`: authenticated controls.
- `src/git.rs`, `src/process.rs`: Git/GitHub publication and owned process groups.
- `web/src`: dashboard, shared TypeScript types, settings and task evidence.
- `tests/core.rs`, `tests/usage.rs`: Rust behavior tests; `tests/e2e.py` and
  `tests/fixtures`: deterministic Codex/GitHub peers with real local Git.
- `web/tests`: browser tests. `docs/architecture.md` describes the operating contract.

## Build and verify

Use Rust 1.88+ with rustfmt/clippy, Node 22.12+, npm, Python 3, Git and a C compiler.
Install dashboard dependencies with `npm ci --prefix web`. Install browser test
prerequisites with `npx --prefix web playwright install --with-deps chromium`.

- `make check`: Rust formatting/clippy, Svelte/TypeScript and Prettier checks.
- `make test`: Rust tests, debug binary, dashboard build, integration and browser tests.
- `make build`: production binary and dashboard.
- Focused integration: `npm run build --prefix web`, `cargo build --locked`, then
  `python3 tests/e2e.py`. These tests use fixtures, not live accounts or model calls.

Run relevant behavior tests while editing and the full checks before delivery.
Keep Rust and dashboard configuration types aligned. Preserve backward-compatible
loading of saved configuration and task snapshots when adding fields.

## Conventions and boundaries

- Name branches created by Codex `tyk/{branch-name}`. Never use `codex/`.
- Make cohesive changes with meaningful verification. Preserve existing features,
  full-diff review, fresh reviewer threads and persistent per-task repair threads.
- Never silently substitute model/effort routes or weaken verification to publish.
- The service delivers PRs; it does not merge, deploy or migrate production systems.
  Workers must not push or publish; the Rust orchestrator owns publication.
- Do not edit `.octomus/`, credentials, account configuration, or other workspaces.
  Fixtures and generated build artifacts are not live evidence.
- Keep secrets, raw Codex transcripts and private billing screenshots out of Git.
  Record redacted observations and evidence references instead.
- Do not mark release gates complete without real evidence. Week 1 operation needs
  the owner's dedicated VM and bot; the development workspace is not that VM.
- Defer LICENSE/NOTICE ownership edits until the owner supplies cleared facts.
