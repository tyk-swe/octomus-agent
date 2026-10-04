# Working on Octomus Agent

Octomus is a single-operator service that discovers repository improvements,
reviews proposals, executes accepted tasks through Codex or OpenCode, and delivers
GitHub PRs. It is a Go service (`cmd/octomus-agent`, `internal/`); the SvelteKit
dashboard builds to static assets embedded in the binary (`web/embed.go`). By
default every runner turn and verification command runs in a Docker sandbox built
by the sandbox broker (`docs/sandbox.md`); `--sandbox off` runs them on the host.

The service uses a Go-owned SQLite schema at version 7. Existing databases from
earlier versions are refused before schema or journal changes; start this release
with a fresh data directory and keep any old state backed up. `internal/wirejson`
owns strict typed JSON boundaries for saved records and API requests.

## Repository map

- `cmd/octomus-agent`: `main.go` (CLI flags, the one mode that runs instead of the
  service, read-only exports and service startup), `service.go` (scheduler and HTTP
  server lifecycle), `deploy.go` (Docker deployment settings and the trusted
  checkout), `secrets.go` (secret files and the git credential helper) and
  `sandbox.go` (sandbox backend, egress gateway, login lease and broker entry points).
- `internal/engine`: scheduler and cycle orchestration: `engine.go` (App
  construction, options, runtime state and restart recovery), `scheduler.go`
  (tick, dispatch, pauses and idle backoff), `planning.go` (grounding, discovery,
  proposal review and consolidation), `planning_validate.go` (proposal validation,
  target resolution and external PR context), `execution.go` (execution, repair and
  publication), `review.go` (trusted change set and code-review turns),
  `verification.go` (task verification commands and sandbox evidence), `invocation.go`
  (role invocation: every agent turn's admission, session start or resume, session
  record and redaction),
  `memory.go` (decision memory and rediscovery requests), `housekeeping.go`
  (retention, storage measurement and remote observation), `baseline.go`
  (clean-baseline checks, separate from task verification), `capacity.go` (open-PR
  observation and owned-PR admission), `actions.go`/`control.go`/`api.go` (operator
  controls and views).
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
  reservation; `schema.sql` is the embedded fresh DDL whose triggers maintain
  counts, batch membership, proposal rows, PR reservation release and the attention
  outbox; `schema.go` creates it and generates the record projections; `queries.go`,
  `capacity.go` and `notifications.go` serve operational views, PR capacity and the
  outbox.
- `internal/sandbox`: where every untrusted child starts (`Backend`: `Host` for
  `--sandbox off`, `Remote` for the broker), owned-root preparation, OpenCode
  readiness and its stream connection (`opencode.go`), the in-sandbox helper
  (`helper.go`, `--sandbox-init`: OpenCode HTTP/2 bridge and version probe) and the
  containment probe (`probe.go` runs it, `containment.go` checks from inside the
  sandbox). `sandbox/wire` is the broker wire contract in one file: requests, stream
  frames, the broker info document, the runner names, the programs both backends
  start, the owned-root layout and the literals both sides share. `sandbox/broker` is
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
  `Text`, `JSON` and the token and webhook variable names) and `fragment.go`
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
- `web/src`: dashboard, shared TypeScript types, settings, setup checklist and
  run/task evidence.
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
  `baseline`, `notify`), one function each, over `tests/harness.py` and
  `tests/fixtures`: deterministic Codex/OpenCode/GitHub peers with real local Git.
  `tests/distribution.py` checks the executable as shipped (HTTP, state lock
  release, listener warning) and, with `--package`, the release archive against
  `scripts/release-files.txt`. `tests/serve_ui.py` serves synthetic data to the
  browser tests.
- Release and deployment inputs: `VERSION` (the one version, read by `version.go`,
  the dashboard build and release tooling), `scripts/package.sh`, `install.sh`,
  `deploy/docker` (images, compose file, `setup.sh`, `env.example`) and the
  unsandboxed `deploy/octomus-agent.service`. `tests/e2e_sandbox.py` runs the compose
  stack with test images from `tests/docker`. `web/scripts/render-launch-assets.mjs` captures
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

- `make check`: gofmt/`go vet`, Svelte/TypeScript and Prettier checks.
- `make test`: the stage targets in order: Go tests (regular and `-race`
  suites), then against the production binary and dashboard build
  `tests/distribution.py`, `tests/e2e.py` and the browser tests.
- `make build`: production binary (`bin/octomus-agent`) and dashboard.
- `make test-go`: the regular Go suite. `make test-go-race`: the race suite
  explicitly. Both run with `-shuffle=on`; a failure prints its seed to
  reproduce. `make test-contracts` / `make test-integration` /
  `make test-browser`: one stage each. `test-integration` accepts
  `SCENARIOS` names; `test-browser` accepts `PLAYWRIGHT_ARGS`. Browser tests
  run four workers.
- `make test-race-e2e` (opt-in, about a minute): `tests/e2e.py` against the
  race-instrumented build.
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
types, vocabularies and limits. Keep saved version-7 records
loadable when adding fields.

## Conventions and boundaries

- Name branches created by Octomus `tyk/{branch-name}`. Never use `codex/`.
- Make cohesive changes with meaningful verification. Preserve existing features,
  full-diff review, fresh reviewer threads and persistent per-task repair threads.
- Never silently substitute model/effort routes or weaken verification to publish.
- The service delivers PRs; it does not merge, deploy or migrate production systems.
  Workers must not push or publish; the orchestrator owns publication.
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
