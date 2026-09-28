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
- `internal/engine`: scheduler and cycle orchestration: `engine.go` (App
  construction, options, runtime state and restart recovery), `scheduler.go`
  (tick, dispatch, pauses and idle backoff), `planning.go` (grounding, discovery,
  proposal review and plan validation), `execution.go` (execution, review, repair and
  verification, with their role prompts), `invocation.go` (role invocation: every
  agent turn's admission, session start or resume, session record and redaction),
  `memory.go` (decision memory and rediscovery requests), `housekeeping.go`
  (retention, storage measurement and remote observation), `baseline.go`
  (clean-baseline checks, separate from task verification), `capacity.go` (open-PR
  observation and owned-PR admission), `actions.go`/`control.go`/`api.go` (operator
  controls and views).
- `internal/runner`: runner-neutral model discovery, exact routing and dispatch
  (`runner.go`); `codex.go` (app-server protocol) and `opencode.go` (HTTP/SSE, with
  `opencode_policy.go`, `opencode_sse.go` and `opencode_catalog.go`) implement it.
  `owned.go` joins and cleans up both owned runner children and explains a failed
  connect with a redacted stderr tail. `runnertest` is the scripted adapter tests
  inject as the runner connector (engine `WithRunnerConnector`) in place of a runner
  process.
- `internal/config`, `internal/model`, `internal/store`: policy, durable records
  and SQLite. In the store, `store.go` holds records, transactions and admission
  reservation; `schema.sql` is the embedded fresh DDL whose triggers maintain
  counts, batch membership, proposal rows, PR reservation release and the attention
  outbox; `schema.go` creates it and generates the record projections; `queries.go`,
  `capacity.go` and `notifications.go` serve operational views, PR capacity and the
  outbox.
- `internal/redact`: the one secret scrubber and display bound, shared by every
  package that records or returns text, the token and webhook variable names, and
  `Fragment` for text already cut by a capture or read limit.
- `internal/notifications`: opt-in webhook delivery. `internal/report`: read-only
  usage reporting. `internal/evidence`: read-only `RunEvidenceV1` export.
  `internal/httpapi`: authenticated controls and embedded dashboard serving.
  `internal/schemas`: structured-output schemas and validation; its test-only
  `schematest` holds each schema to the Go type that decodes its answers.
- `internal/git`, `internal/process`, `internal/workspace`: Git/GitHub
  publication, owned process groups and managed-directory safety.
- `web/src`: dashboard, shared TypeScript types, settings, setup checklist and
  run/task evidence.
- Go behavior tests sit beside each package (`*_test.go`); `internal/testutil`
  holds their shared polling and process helpers. Pinned real-client contracts
  (`internal/runner`) and the scale checks (`internal/store`,
  `OCTOMUS_SCALE_TEST=1`) skip unless their environment is provided.
- `tests/e2e.py`, `e2e_runners.py`, `e2e_hardening.py`, `e2e_baseline.py`, and
  `e2e_notifications.py` share `tests/harness.py` and `tests/fixtures`:
  deterministic Codex/OpenCode/GitHub peers with real local Git.
  `tests/integration.py` is the aggregate runner `make test` uses; it selects
  whole suites by alias or single scenarios by `suite/scenario` name, and the
  direct suite files remain focused entry points.
  `binary_contract.py` checks executable startup and embedded assets.
  `distribution.py`, `package_guards.py` and `systemd.py` cover packaging and
  deployment; `evidence_snapshot.py` runs the documented backup and export examples
  on synthetic data, against the private-payload gate in `tests/helpers`.
  `tests/fixturedb` creates a fresh state database for the Python tests.
- Release and deployment inputs: `VERSION` (the one version, read by `version.go`,
  the dashboard build and release tooling), `scripts/package.sh`, `install.sh` and
  `deploy/octomus-agent.service`. `web/scripts/render-launch-assets.mjs` captures
  `docs/dashboard.png`.
- `web/tests`: dashboard browser tests against the synthetic service
  `tests/serve_ui.py` starts, with shared synthetic fixtures in `synthetic.ts`;
  `evidence.spec.ts` checks the evidence display rules and badge styles without a page.
  `docs/architecture.md` describes the operating contract.

## Build and verify

Use Go (per `go.mod`), Node 22.12+, npm, Python 3, Git and a C compiler for the
race detector. Install dashboard dependencies with
`npm ci --prefix web`. Install browser test prerequisites with
`npx --prefix web playwright install --with-deps chromium`.

- `make check`: gofmt/`go vet`, Svelte/TypeScript and Prettier checks, including
  `tests/helpers`.
- `make test`: Go tests (regular and `-race` suites), production
  binary, dashboard build, all service suites through `tests/integration.py`,
  and browser tests.
- `make build`: production binary (`bin/octomus-agent`) and dashboard.
- `make test-go`: the regular Go suite. `make test-go-race`: the race suite
  explicitly. `make test-contracts` / `make test-integration` /
  `make test-browser`: one stage each. `test-integration` accepts
  `INTEGRATION_SCENARIOS` suite aliases or `suite/scenario` names;
  `test-browser` accepts `PLAYWRIGHT_ARGS`. Browser tests run four workers.
- `make test-race-e2e` (opt-in, about seven minutes): `tests/e2e.py` against the
  race-instrumented build.
- `make audit` (govulncheck and `npm audit`; needs module downloads) and `make package`
  (release archive and `SHA256SUMS` in `dist/`) also run in CI.
- Focused integration: `make build`, then
  `OCTOMUS_TEST_BINARY="$PWD/bin/octomus-agent" python3 tests/integration.py [SUITE_OR_SUITE/SCENARIO...]`;
  direct suite files like `tests/e2e.py [SCENARIO...]` work the same way.
  An unknown selection lists the suites and qualified names. These
  tests use fixtures, not live accounts or model calls.

Run relevant behavior tests while editing and the full checks before delivery.
Keep Go and dashboard types aligned; `web/types_contract_test.go` and
`internal/config/dashboard_test.go` compare them. Keep saved version-7 records
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
