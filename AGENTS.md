# Working on Octomus Agent

Octomus is a single-operator service that discovers repository improvements,
reviews proposals, executes accepted tasks through Codex or OpenCode, and delivers
GitHub PRs. It is a Go service (`cmd/octomus-agent`, `internal/`); the SvelteKit
dashboard builds to static assets embedded in the binary (`web/embed.go`).

The service uses a Go-owned SQLite schema at version 7. Existing databases from
earlier versions are refused before schema or journal changes; start this release
with a fresh data directory and keep any old state backed up. `internal/wirejson`
owns strict typed JSON boundaries for saved records and API requests.

## Repository map

- `cmd/octomus-agent`: CLI flags, read-only exports and service startup.
- `internal/engine`: scheduler and cycle orchestration: `planning.go` (discovery
  and proposal review), `execution.go` (execution, review, repair and
  verification), `invocation.go` (role invocation: every agent turn's
  admission, session start or resume, session record and redaction),
  `memory.go` (grounding and history), `housekeeping.go`
  (retention, workspace and disk limits), `baseline.go` (clean-baseline checks,
  separate from task verification), `capacity.go` (owned-PR admission),
  `actions.go`/`control.go`/`api.go` (operator controls and views).
- `internal/runner`: runner-neutral model discovery, exact routing and dispatch;
  `codex.go` (app-server protocol) and `opencode.go` (HTTP/SSE) implement it.
  `runnertest` is the scripted adapter tests inject as the runner connector
  (engine `WithRunnerConnector`) in place of a runner process.
- `internal/config`, `internal/model`, `internal/store` (with `queries.go`,
  `schema.go`, `capacity.go`, `notifications.go`): policy, durable records,
  SQLite, fresh schema creation, indexed operational views and the attention outbox.
- `internal/notifications`: opt-in webhook delivery. `internal/report`: read-only
  usage reporting. `internal/evidence`: read-only `RunEvidenceV1` export.
  `internal/httpapi`: authenticated controls and embedded dashboard serving.
  `internal/schemas`: structured output.
- `internal/git`, `internal/process`, `internal/workspace`: Git/GitHub
  publication, owned process groups and managed-directory safety.
- `web/src`: dashboard, shared TypeScript types, settings, setup checklist and
  run/task evidence.
- Go behavior tests sit beside each package (`*_test.go`). Pinned real-client
  contracts (`internal/runner`) and the scale checks (`internal/store`,
  `OCTOMUS_SCALE_TEST=1`) skip unless their environment is provided.
- `tests/e2e.py`, `e2e_runners.py`, `e2e_hardening.py`, `e2e_baseline.py`, and
  `e2e_notifications.py` with `tests/fixtures`: deterministic Codex/OpenCode/GitHub
  peers with real local Git. `binary_contract.py` checks executable startup and
  embedded assets. `distribution.py`,
  `package_guards.py` and `systemd.py` cover packaging and deployment;
  `evidence_snapshot.py` runs the documented backup and export examples on synthetic data.
- `web/tests`: dashboard browser tests served by `tests/serve_ui.py`.
  `docs/architecture.md` describes the operating contract.

## Build and verify

Use Go (per `go.mod`), Node 22.12+, npm, Python 3, Git and a C compiler for the
race detector. Install dashboard dependencies with
`npm ci --prefix web`. Install browser test prerequisites with
`npx --prefix web playwright install --with-deps chromium`.

- `make check`: gofmt/`go vet`, Svelte/TypeScript and Prettier checks.
- `make test`: Go tests (including `-race`), production binary, dashboard build,
  integration and browser tests.
- `make build`: production binary (`bin/octomus-agent`) and dashboard.
- Focused integration: `npm run build --prefix web`, `make build`, then
  `OCTOMUS_TEST_BINARY="$PWD/bin/octomus-agent" python3 tests/e2e.py`. These
  tests use fixtures, not live accounts or model calls.

Run relevant behavior tests while editing and the full checks before delivery.
Keep Go and dashboard configuration types aligned. Keep saved version-7 records
loadable when adding fields.

## Conventions and boundaries

- Name branches created by Octomus `tyk/{branch-name}`. Never use `codex/`.
- Make cohesive changes with meaningful verification. Preserve existing features,
  full-diff review, fresh reviewer threads and persistent per-task repair threads.
- Never silently substitute model/effort routes or weaken verification to publish.
- The service delivers PRs; it does not merge, deploy or migrate production systems.
  Workers must not push or publish; the orchestrator owns publication.
- Do not edit `.octomus/`, credentials, account configuration, or other workspaces.
  Fixtures and generated build artifacts are not live evidence.
- Keep secrets, raw runner transcripts and private billing screenshots out of Git.
  Record redacted observations and evidence references instead.
- Do not claim live validation without real evidence. Live operation needs
  the owner's dedicated VM and bot; the development workspace is not that VM.
- Defer LICENSE/NOTICE ownership edits until the owner supplies cleared facts.

## Agent skills

### Issue tracker

Issues and specs live as GitHub issues in `tyk-swe/octomus-agent`, managed with
the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical triage roles map to same-named label strings
(`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`).
See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` plus `docs/adr/` at the repo root, created
lazily by `/domain-modeling`. See `docs/agents/domain.md`.
