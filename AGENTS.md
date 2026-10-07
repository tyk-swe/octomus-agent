# Working on Octomus Agent

Octomus is a single-operator service that discovers repository improvements,
reviews proposals, executes accepted tasks through Codex or OpenCode, and delivers
GitHub PRs. It is a Go service (`cmd/octomus-agent`, `internal/`); the SvelteKit
dashboard builds to static assets embedded in the binary (`web/embed.go`). By
default every runner turn and verification command runs in a Docker sandbox built
by the sandbox broker (`docs/sandbox.md`); `--sandbox off` runs them on the host.

The service uses a Go-owned SQLite schema; v0.1.0 shipped version 7, the oldest
upgradable baseline. `internal/store/schema.go`'s `releaseMigrations` upgrade it
forward-only at startup: `Open` checks the version before any schema or journal
change, writes a verified `state.db.v<N>-backup-<UTC>` beside the database, then
runs each migration in its own transaction. A fresh database applies `schema.sql`,
the complete latest DDL, and never replays migrations. Pre-release and newer
databases are refused untouched; read-only exports refuse an older schema until the
service upgrades it. Golden databases under `internal/store/testdata`, produced by
`scripts/golden-state.py`, must keep opening on `main`. `internal/wirejson` owns
strict typed JSON boundaries for saved records and API requests.

## Repository map

- `cmd/octomus-agent`: `main.go` (CLI flags, the one mode that runs instead of the
  service, read-only exports and service startup), `service.go` (scheduler and HTTP
  server lifecycle), `deploy.go` (Docker deployment settings and the trusted
  checkout), `secrets.go` (secret files and the git credential helper) and
  `sandbox.go` (sandbox backend, egress gateway, login lease and broker entry points).
- `internal/engine`: scheduler and cycle orchestration: `engine.go` (App
  construction, options, Run/Shutdown, Config/Control and failure reporting),
  `runtime.go` (runtime state and jobs, the recovery barrier and cleanup claims),
  `scheduler.go` (tick, dispatch, planning start, capacity waits and task
  workers), `plan_rules.go` (the one dependency validator planning and dispatch
  share, proposal validation, target resolution, assessment and consolidation
  checks and external PR context), `planning.go` (grounding, discovery, proposal
  review, consolidation and plan commit), `memory.go` (decision memory and
  rediscovery requests), `invocation.go` (role invocation: every agent turn's
  admission, session start or resume, session record and redaction),
  `execution.go` (task supervision, execution, repair and publication),
  `review.go` (trusted change set and code-review turns), `verification.go`
  (task verification commands, sandbox evidence and the shared output bound),
  `recovery.go` (restart recovery and the orphan scans every tick repeats),
  `controls.go` (operating-mode and configuration controls, audit start,
  planning preflight and cycle start), `actions.go` (per-record cycle and task
  actions, reconciliation and owned-root removal), `views.go` (state, baseline,
  doctor, model-catalog and sandbox posture views and the containment
  self-test), `capacity.go` (open-PR inventory claims and capacity),
  `observation.go` (default-branch and open-PR observation, context fingerprint),
  `housekeeping.go` (retention and storage measurement), `baseline.go`
  (clean-baseline checks, separate from task verification), `deployment.go`
  (deployment pinning, scratch roots, sandbox readiness and route validation),
  `prompts.go` (the planning roles' prompts).
- `internal/runner`: runner-neutral model discovery, exact routing and dispatch
  (`runner.go`); `codex.go` (app-server protocol) and `opencode.go` (HTTP/SSE, with
  `opencode_policy.go` for the worker policy and `opencode_catalog.go`) implement it
  over `stream.go`, the bounded line splitter behind the NDJSON reader and the SSE
  framer.
  `owned.go` joins and cleans up both owned runner children and explains a failed
  connect with a redacted stderr tail. `runnertest` is the scripted adapter tests
  inject as the runner connector (engine `WithRunnerConnector`) in place of a runner
  process.
- `internal/config`, `internal/model`, `internal/store`: policy, durable records
  and SQLite. In the store, `store.go` holds records, transactions and admission
  reservation; `schema.sql` is the complete fresh DDL, whose triggers maintain the
  record projections, counts, batch membership, proposal rows, PR reservation
  release and the attention outbox; `schema.go` checks the version, creates fresh
  databases and runs the backup-then-migrate upgrade path over `migrations/`, with
  `export_test.go`'s plan hooks and the `testdata/state-v*.db` goldens exercising
  it; `queries.go`, `capacity.go`, `notifications.go` and `baseline.go` serve
  operational views, PR capacity, the outbox and baseline-check admission.
- `internal/sandbox`: where every untrusted child starts (`Backend`: `Host` for
  `--sandbox off`, `Remote` for the broker), owned-root preparation, OpenCode
  readiness and its stream connection (`opencode.go`), the in-sandbox helper
  (`helper.go`, `--sandbox-init`: OpenCode HTTP/2 bridge and version probe) and the
  containment probe (`probe.go` runs it, `containment.go` checks from inside the
  sandbox). `sandbox/wire` is the broker wire contract in one file: requests, stream
  frames, the broker info document, the runner names, the programs both backends
  start, the owned-root layout, the probe-target contract and the literals both
  sides share. `sandbox/broker` is
  `--sandboxd`: `config.go` (deployment settings), `startup.go` (daemon, network
  and volume checks, helper install), `image.go` (tag resolution and the
  runner-version probe), `listen.go` (peer-credential socket), `serve.go` (HTTP and
  the upgraded stream), `validate.go` (request and owned-root checks), `spec.go`
  (the golden container spec, `testdata/spec-*.json`), `lifecycle.go` (create,
  attach, run and evidence, granting egress leases) and `teardown.go` (teardown
  bounds, removal, reaper and sweep) and `dockerapi.go` (its minimal Docker Engine
  client). `internal/egress` is the `--egress` gateway: `policy.go`
  (allowlists, `PolicyFromEnv`), `gateway.go` (the CONNECT gateway) and `serve.go`
  (listener bounds and the lease sweep). It owns its contract with the broker:
  `lease.go` (lease files and proxy credentials) and `collector.go` (the summary
  collector).
- `internal/redact`: the one secret scrubber and display bound, shared by every
  package that records or returns text: `redact.go` (patterns, environment secrets,
  `Text` and `JSON`; `internal/config` names the operator environment variables) and `fragment.go`
  (`Parts`, `Streams` and `Fragment` for text already cut by a capture or read limit).
- `internal/notifications`: opt-in webhook delivery. `internal/export`: the
  read-only exports of saved records (`export.go` opens one snapshot and
  redacts, `usage.go` is the usage report, `evidence.go` the `RunEvidenceV1`
  export). `internal/httpapi`: authenticated controls and embedded dashboard
  serving. `internal/schemas`: structured-output schemas and validation; its
  tests hold each schema to the `internal/model` type that decodes its answers.
- `internal/git`: `git.go` (exec plumbing on the trusted `repo.git`), `clone.go`
  (split clones and the deployment's remote clone), `remote.go` (origin validation,
  fetches and snapshots), `github.go` (gh inventory and PR reads) and `publish.go`
  (publication). `internal/process`: `command.go` (the process-group child),
  `capture.go` (bounded capture, redacted failure text and `RunMachine`, `RunText`
  and `RunPredicate`), `status.go` (how a child ended) and `deadline.go` (`Bounded`
  and `WithDeadline`). `internal/workspace`: managed-directory safety.
- `web/src`: the dashboard. `routes/+page.svelte` is the shell: login, navigation,
  list paging, the refresh coordinator and the operator controls. `lib/` holds the
  views (`Overview`, `Proposals`, `Notices`, `Settings` with `RouteEditor`,
  `SetupChecklist` and `BaselineCheck`, the `TaskDetail` and `RunEvidence` dialogs)
  and the pieces they share (`Badge`, `RecoveryNotice`, `Sha`, `SandboxRun`),
  `types.ts` (the TypeScript mirror of the Go records), `api.ts` (the authenticated
  client), `format.ts` (relative times, sizes and short hashes), `evidence.ts`
  (verdict rules), `sandbox.ts` (the sandbox verdict), `setup.ts` (checklist steps),
  `modelRoutes.ts` (route labels) and `limits.ts` (numeric bounds).
- Go behavior tests sit beside each package (`*_test.go`); `internal/testutil`
  holds their shared polling and process helpers, a POSIX-shell `git`/`gh`
  dispatcher (`InstallFixtureCommands`) that relays into a fixture's `bin/`
  without a Python interpreter start, and the sandbox tests' unix-socket
  servers, protocol upgrades and `SyncBuffer` (`sockets.go`).
  Pinned real-client contracts (`internal/runner`) and the scale check
  (`internal/store`, `OCTOMUS_SCALE_TEST=1`) skip unless their environment is
  provided.
- `tests/e2e.py` holds the service scenarios (`normal`, `normal-opencode`,
  `normal-mixed`, `interrupt-publication`, `audit`, `chain`, `pr-outcome`,
  `baseline`, `notify`, `pr-context`, `upgrade`), one function each, over `tests/harness.py` and
  `tests/fixtures`: deterministic Codex/OpenCode/GitHub peers with real local Git.
  `tests/distribution.py` checks the executable as shipped (HTTP, state lock
  release, listener warning); CI also runs it on each extracted release archive.
  `tests/serve_ui.py` serves synthetic data to the
  browser tests.
- Release and deployment inputs: `VERSION` (the one version, read by `version.go`,
  the dashboard build and release tooling), `scripts/package.sh`, `install.sh`,
  `scripts/golden-state.py` (per-release golden state databases), `deploy/docker`
  (images, compose file, `setup.sh`, `env.example`) and the
  unsandboxed `deploy/octomus-agent.service`. `tests/e2e_sandbox.py` runs the compose
  stack with test images from `tests/docker`.
- `web/tests`: dashboard browser tests against the synthetic service
  `tests/serve_ui.py` starts, with the shared fixtures in `synthetic.ts` (login,
  navigation, the configuration mock, `patchState` and recorded-evidence builders):
  `dashboard.spec.ts` (the tour, task tabs, model routing, mobile navigation),
  `configuration.spec.ts` (revision conflicts, drafts, the setup checklist),
  `controls.spec.ts` (planning capacity, queued state refreshes, control eligibility
  and an audit), `recovery.spec.ts` (list retries and refused actions),
  `run-evidence.spec.ts`, `sandbox.spec.ts`, `evidence.spec.ts` and
  `control-eligibility.spec.ts`, which check dashboard rules without a page, and
  `launch-assets.spec.ts`, which captures `docs/dashboard.png` only under `npm run launch:assets`.
- `docs/architecture.md` describes the operating contract.

## Build and verify

Use Go (per `go.mod`), Node 22.12+, npm, Python 3, Git and a C compiler for the
race detector. Install dashboard dependencies with
`npm ci --prefix web`. Install browser test prerequisites with
`npx --prefix web playwright install --with-deps chromium`.

- `make check`: gofmt/`go vet`, Svelte/TypeScript and Prettier checks.
- `make test`: the stage targets in order: the `-race` Go suite, fixture
  startup behavior checks, then service and browser tests against the production
  binary and dashboard build (`tests/distribution.py`, `tests/e2e.py` and Playwright).
  Ordering also holds under parallel Make. CI runs service and browser tests in
  separate required matrix entries.
- `make build`: production binary (`bin/octomus-agent`) and dashboard.
- `make test-go`: the regular Go suite, the quick local loop.
  `make test-go-race`: the same tests under the race detector. Both run with
  `-shuffle=on`; a failure prints its seed to reproduce. Both accept
  `GO_TEST_PACKAGES` (default `./...`), for example
  `make test-go GO_TEST_PACKAGES=./internal/engine`. Reproduce a seed with
  `GO_TEST_FLAGS='-timeout 30m -shuffle=12345'`.
- `make test-ui-logic` / `npm run test:unit --prefix web`: seven page-free dashboard
  rule tests, requiring only npm dependencies. They also remain in the full browser suite.
- `make test-integration` (fixture startup behavior tests, `tests/distribution.py`, then
  `tests/e2e.py`) / `make test-browser`: one stage each. `test-integration` accepts
  `SCENARIOS` names; `test-browser` accepts `PLAYWRIGHT_ARGS`. Browser tests
  run four workers; the `mobile` project reruns only specs tagged `@responsive`.
  Host service, distribution and browser fixtures bind an OS-allocated port and
  discover it from that process's listening announcement before checking health.
- `make test-sandbox` (opt-in, needs Docker Engine 28+): the broker against the real
  daemon (`OCTOMUS_DOCKER_TEST=1`) and `tests/e2e_sandbox.py` against the compose stack.
- `make audit` (govulncheck and `npm audit`; needs module downloads) and `make package`
  (release archive and `SHA256SUMS` in `dist/`) also run in CI.
- Focused integration: `make build`, then
  `OCTOMUS_TEST_BINARY="$PWD/bin/octomus-agent" python3 tests/e2e.py [SCENARIO...]`.
  An unknown name lists the scenarios. These tests use fixtures, not live
  accounts or model calls.

Run relevant behavior tests while editing and the full checks before delivery.
Keep Go and dashboard types aligned; `web/types_contract_test.go` compares
types, vocabularies and limits. Keep records in every
checked-in golden database loadable when adding fields: a pointer, a
`wire:"default"` tag, or a migration.

## Conventions and boundaries

- Name branches created by Octomus `tyk/{branch-name}`. Never use `codex/`.
- Make cohesive changes with meaningful verification. Preserve existing features,
  full-diff review, fresh reviewer threads and persistent per-task repair threads.
- Never silently substitute model/effort routes or weaken verification to publish.
- Error strings follow one convention: text an operator may read (API responses,
  task and cycle errors, CLI diagnostics) is a capitalised sentence fragment;
  errors that only wrap an internal mechanism stay lowercase Go style.
- The service delivers PRs; it does not merge, deploy or migrate production systems.
  The one exception is the opt-in maintenance delivery mode: when an operator enables it,
  the orchestrator may squash-merge a small, clean-reviewed, verified maintenance PR only
  after fresh GitHub checks, review state and protections allow it. Workers must not push,
  publish or merge; the orchestrator owns publication and any merge.
- Every untrusted child (runner, verification command, probe) starts through
  `internal/sandbox`; never add another path. Orchestrator git on a work tree goes
  through `gitops.WorkGit` and trusted `repo.git`. A change to a golden container
  spec changes the isolation boundary; review it as one.
- Do not edit `.octomus/`, credentials, account configuration, or other workspaces.
  Fixtures and generated build artifacts are not live evidence.
- Keep secrets, raw runner transcripts and private billing screenshots out of Git.
  Record redacted observations and evidence references instead.
- Do not claim live validation without real evidence. Live operation needs
  the owner's dedicated VM and bot; the development workspace is not that VM.
- Defer LICENSE/NOTICE ownership edits until the owner supplies cleared facts.
