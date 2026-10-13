# Working on Octomus Agent

Octomus is a single-operator Go service (`cmd/octomus-agent`, `internal/`) that discovers
repository improvements, reviews proposals, executes accepted tasks via Codex or OpenCode,
and delivers GitHub PRs. The SvelteKit dashboard builds to embedded static assets
(`web/embed.go`). Runner turns and verification commands run in Docker sandboxes via the
broker (`docs/sandbox.md`) by default, or on the host with `--sandbox off`.

State uses a Go-owned SQLite schema (v0.1.0 baseline: version 7). At startup `Open`
checks the version before any schema or journal change, writes a verified
`state.db.v<N>-backup-<UTC>` beside `state.db`, then applies `releaseMigrations`
(`internal/store/schema.go`) forward-only, one transaction per step. Fresh databases
apply `schema.sql` directly without replaying migrations. Pre-release and newer schemas
are refused untouched; read-only exports refuse older schemas until upgraded. Golden
databases in `internal/store/testdata` (`scripts/golden-state.py`) must keep opening.

## Repository map

- `cmd/octomus-agent`: CLI flags and one-shot modes (`main.go`), service lifecycle
  (`service.go`), Docker/trusted-checkout setup (`deploy.go`), secrets (`secrets.go`)
  and sandbox wiring (`sandbox.go`).
- `internal/engine`: scheduler and cycle orchestration (the only layer HTTP reaches
  state through). `engine.go` builds `App`; `scheduler.go`, `planning.go`, `execution.go`,
  `review.go`, `verification.go`, `recovery.go`, `automerge.go` and `automerge_barriers.go`
  run the stages; `task_commit.go` and `task_worker.go` own task checkpoints and worker
  settlement; `controls.go`, `actions.go` and `eligibility.go` own operator actions;
  `requests.go` owns operator work requests; `reads.go` and `api_views.go` own API queries
  and response types.
- `internal/runner`: model discovery and routing (`runner.go`) over Codex app-server
  (`codex.go`) and OpenCode HTTP/SSE (`opencode.go`); `runnertest` is the scripted test
  adapter (`WithRunnerConnector`).
- `internal/config`, `internal/model`: policy and durable record types; action, event and
  lifecycle contracts in `model/actions.go`, `model/events.go` and `model/task_lifecycle.go`.
- `internal/store`: SQLite records, transactions and read models (`pages.go`, `queries.go`).
  `schema.sql` is the fresh DDL with projection/count/outbox triggers; `schema.go` and
  `migrations/` upgrade `testdata/state-v*.db` goldens; `keys.go` types record keys;
  `task_lifecycle.go` guards task transitions; `requests.go` stores operator work
  requests and their guarded updates; `insights.go` reads usage/outcome summaries.
- `internal/wirejson`: strict typed JSON boundaries for saved records and API requests.
- `internal/sandbox`: untrusted child launcher (`Host` for `--sandbox off`, `Remote` for
  the broker), in-sandbox helper and containment probe. `wire` is the broker protocol;
  `broker` is `--sandboxd` with golden specs in `broker/testdata/spec-*.json`.
- `internal/egress`: `--egress` allowlisting CONNECT gateway, leases and collector.
- `internal/redact`: shared secret scrubber and display bounding.
- `internal/notifications`: opt-in webhook delivery. `internal/export`: read-only exports
  (`RunEvidenceV1`, usage report). `internal/schemas`: structured-output schemas.
- `internal/httpapi`: authenticated controls and embedded dashboard (`routes.go`).
- `internal/git`: orchestrator git/GitHub plumbing, shared-checkout gate (`checkout.go`),
  publication and guarded maintenance merge. `internal/process`: process-group children
  and bounded capture. `internal/workspace`: managed-directory safety.
- `internal/testutil`: shared test helpers (`git`/`gh` dispatcher and unix-socket servers).
- `web`: `embed.go` embeds the build; `src/routes/+page.svelte` is the shell; `src/lib`
  holds views (`Requests.svelte` operator requests, `Insights.svelte` usage insights) and
  state owners
  (`stateSession.ts`, `resource.ts`, `configurationDraft.ts`).
  Regenerate `src/lib/types.ts` with
  `OCTOMUS_UPDATE_API_CONTRACT=1 go test ./web -run TestAPIContractGenerated`.
- `web/tests`: Playwright specs against `tests/serve_ui.py` (`synthetic.ts`). `mobile`
  reruns `@responsive` specs; `web/playwright.unit.config.ts` lists pure-logic specs;
  `launch-assets.spec.ts` updates `docs/dashboard.png` via `npm run launch:assets`.
- `tests`: `e2e.py` service scenarios over `harness.py` and `fixtures/`; `distribution.py`
  binary smoke test; `e2e_sandbox.py` compose stack tests; `production_images.py` and
  `productionimage/` release-image acceptance; `test_*.py` tooling unit tests.
- `scripts`: `package.sh` (`release-files.txt`), `release.py`, `release-images.py` and
  `golden-state.py`. `deploy`: Dockerfiles, compose files, `setup.sh`, `env.example` and
  `octomus-agent.service`. `install.sh`: release installer. `VERSION`: single version file.
- `docs`: `architecture.md` is the operating contract; each other guide owns one topic.

## Build and verify

Requires Go (per `go.mod`), Node 22.12+, npm, Python 3, Git and a C compiler for `-race`.
Setup: `npm ci --prefix web` and `npx --prefix web playwright install --with-deps chromium`.

- `make check`: `gofmt`, `go vet`, Svelte/TypeScript and Prettier checks.
- `make fmt`: rewrites formatting verified by `make check` (`gofmt` and Prettier).
- `make build`: builds the dashboard and `bin/octomus-agent`.
- `make test`: `test-go-race`, `test-integration`, then `test-browser`.
- `make test-go` / `make test-go-race`: Go suite (without/with `-race`), `-shuffle=on`.
  Filter with `GO_TEST_PACKAGES=./internal/engine` or set `GO_TEST_FLAGS='-timeout 30m -shuffle=N'`.
- `make test-ui-logic` (`npm run test:unit --prefix web`): pure dashboard logic specs.
- `make test-integration`: `make test-python`, `tests/distribution.py`, then `tests/e2e.py`
  (accepts `SCENARIOS`). Focused run: `make build && OCTOMUS_TEST_BINARY="$PWD/bin/octomus-agent" python3 tests/e2e.py [SCENARIO...]`.
- `make test-browser`: Playwright suite (accepts `PLAYWRIGHT_ARGS`).
- `make test-sandbox` (opt-in, Docker 28+): `OCTOMUS_DOCKER_TEST=1` broker tests and `tests/e2e_sandbox.py`.
- `make test-production-images` (opt-in, disposable Docker 28+ host, Skopeo, OpenSSL):
  tests pinned clients in `dist/images` OCI archives.
- `make audit` (govulncheck + `npm audit`) and `make package` (archives + `SHA256SUMS` in `dist/`).

Keep Go and `types.ts` aligned. Keep checked-in golden databases loadable when adding
record fields (use a pointer, `wire:"default"`, or a migration). Pinned client contracts
(`internal/runner`) and `OCTOMUS_SCALE_TEST=1` (`internal/store`) skip unless enabled.

## Conventions and boundaries

- Name Octomus branches `tyk/{branch-name}`, never `codex/`.
- Make cohesive changes with behavior tests. Preserve existing features, full-diff review,
  fresh reviewer threads and persistent per-task repair threads.
- Never silently substitute model/effort routes or weaken verification to publish.
- Operator-facing errors (API, task/cycle errors, CLI diagnostics) are capitalised
  sentence fragments; internal wrapper errors stay lowercase Go style.
- The service delivers PRs; it never merges, deploys or migrates production systems,
  except in opt-in maintenance mode where the orchestrator may squash-merge a small,
  clean-reviewed, verified maintenance PR once fresh GitHub checks, reviews and
  protections allow it. Workers never push, publish or merge.
- Launch every untrusted child (runner, verification command, probe) through
  `internal/sandbox`. Run orchestrator work-tree git via `gitops.WorkGit` and trusted
  `repo.git`. Treat `broker/testdata/spec-*.json` changes as isolation boundary changes.
- Do not edit `.octomus/`, credentials, account config or other workspaces. Keep secrets,
  raw transcripts and private billing screenshots out of Git.
- Do not claim live validation without real evidence from the owner's dedicated VM/bot.
- Defer LICENSE/NOTICE ownership edits until the owner supplies cleared facts.
