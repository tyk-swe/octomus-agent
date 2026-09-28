# Contributing

Octomus discovers useful work and delivers reviewed PRs for a single operator and
repository. Start with a concrete problem, reproduction or proposal; a justified
no-change outcome is welcome. Report security issues privately using
[SECURITY.md](SECURITY.md).

## Development

Use Linux, Go (per `go.mod`), Node 22.12+, npm, Python 3 and Git; the race
detector additionally needs a C compiler. Codex, OpenCode and GitHub credentials
are not needed for fixture tests.

```bash
npm ci --prefix web
npx --prefix web playwright install --with-deps chromium
make check
make test
```

The dashboard must be built before compiling Go because it is embedded in the
executable. Make targets handle this order. For focused Go work, first run
`npm run build --prefix web`, then `go test ./...`. After UI edits rebuild
the dashboard and binary so browser tests exercise current assets.

`make build` creates the production executable at `bin/octomus-agent`, and
`make build-race` produces a race-instrumented variant. The opt-in
`make test-race-e2e` (about seven minutes; needs a C compiler) runs `tests/e2e.py`
against that variant so the race detector sees real HTTP, scheduler and runner
interleavings; it is not part of `make test`. `--assets web/build`
explicitly serves a development dashboard instead of the embedded copy. The
application version lives in the root `VERSION` file; change it in one place.
See [architecture](docs/architecture.md) and [AGENTS.md](AGENTS.md) for the code map.

## Meaningful evidence

`make test` runs the regular Go suite and then the race suite, then these
suites against the freshly built binary. The service suites run the real service,
SQLite and local Git with deterministic Codex, OpenCode and GitHub peers
(`tests/fixtures`) in temporary directories, without live model calls or network
writes:

- `tests/binary_contract.py`: startup order, signal shutdown and lock release, and the
  embedded dashboard.
- `tests/evidence_snapshot.py`: the documented SQLite backup and `--export-run`
  examples on a synthetic database, and `docs/configuration.example.json` against
  `--print-config`.
- `tests/helpers/public_payload.test.mjs` (`node --test`): the private-payload gate the
  run-evidence example imports.
- `tests/integration.py`: one runner over every service suite, which is what
  `make test` uses. Arguments select whole suites by alias (`e2e`, `baseline`,
  `notifications`, `runners`, `hardening`) or single scenarios by qualified name,
  for example `python3 tests/integration.py baseline notifications` or
  `python3 tests/integration.py hardening/chain`; an unknown name lists the
  suites and qualified scenarios.
- `tests/e2e.py`: discovery, reviews, repairs, publication recovery and audits.
- `tests/e2e_baseline.py`: clean-baseline checks, cancellation, restart and cleanup.
- `tests/e2e_notifications.py`: attention webhook delivery and URL non-leakage.
- `tests/e2e_runners.py`: both runner protocols and mixed routes.
- `tests/e2e_hardening.py`: operational regressions against a real temporary remote.
- `tests/distribution.py`: the executable and installer using local release fixtures.
- `tests/package_guards.py`: `scripts/package.sh` rejection cases and the release
  archive allowlist.
- `npm test --prefix web`: the dashboard browser tests, including the evidence display
  rules `web/tests/evidence.spec.ts` checks without a page.

Focused targets run one stage each against the built binary: `make test-go`
(regular Go suite), `make test-go-race` (race suite explicitly),
`make test-contracts`, `make test-integration` (with `INTEGRATION_SCENARIOS`
suite aliases or qualified names) and `make test-browser` (with
`PLAYWRIGHT_ARGS` such as `--project=desktop`).

The e2e suites share `tests/harness.py` and accept scenario names, for example
`python3 tests/e2e_hardening.py chain fork`; an unknown name lists the available ones.
Scenarios run with up to four workers; `OCTOMUS_TEST_JOBS` sets the limit
(1 runs serially). Set `OCTOMUS_TEST_BINARY` to test another executable. Go tests share polling and
process helpers through `internal/testutil`, and inject `internal/runner/runnertest`
in place of runner processes; `internal/schemas/schematest` holds each structured-output
schema to the Go type that decodes its answers. `web/types_contract_test.go` and
`internal/config/dashboard_test.go` hold the dashboard's TypeScript types, limits and
vocabularies to the Go records and validation. Browser tests use clearly synthetic
data; their screenshots are not live operating evidence. `tests/systemd.py` requires
root on a disposable systemd VM and exercises the unit's write restrictions and child
cleanup.

Some checks run only in CI, because each needs something a working copy does not have:
`make package` followed by `tests/distribution.py --package`, which needs a real
release build; `tests/systemd.py`, which needs root; `shellcheck`; and the
`client-contracts` job, which runs the runner adapters against the real Codex and
OpenCode CLIs at the versions pinned in `.github/workflows/ci.yml`. Run any of them
locally before changing packaging, the installer, the unit file or a runner adapter.

Tests that need something external skip unless an environment variable provides it:

- `OCTOMUS_CONTRACT_CODEX_BINARY` and `OCTOMUS_CONTRACT_OPENCODE_BINARY`: a pinned CLI for
  `TestPinnedCodexContract` and `TestPinnedOpenCodeContract`, which drive real turns
  against a synthetic provider.
- `OCTOMUS_OPENCODE_SMOKE_BINARY`: a pinned OpenCode for
  `TestPinnedOpenCodeProtocolSmokeWithoutModelCalls`.
- `OCTOMUS_SCALE_TEST=1`: the store's scale and latency checks.

Install the CLI version the `client-contracts` job pins into a scratch prefix, as the
job does; for example, for Codex and the store checks:

```bash
npm install --prefix /tmp/octomus-contract --no-audit --no-fund @openai/codex@0.153.4
OCTOMUS_CONTRACT_CODEX_BINARY=/tmp/octomus-contract/node_modules/.bin/codex \
  go test ./internal/runner -run '^TestPinnedCodexContract$' -count=1 -v
OCTOMUS_SCALE_TEST=1 go test ./internal/store -run 'Scale|Bounded|Duplicate' -v -count=1
```

Dashboard regressions cover configuration drafts in tab memory, saved-configuration
checks, keyboard navigation and list recovery. Keep drafts across view changes,
clear them at session boundaries, and use entered executable paths for catalogs.
The dashboard browser tests include axe WCAG A/AA and overflow checks. To refresh
`docs/dashboard.png`, build the current dashboard and binary, then run
`npm run launch:assets --prefix web`. The capture script starts a temporary
fixture server, blocks dashboard writes, and labels the screenshot as synthetic.
Inspect the generated image before committing it.

Run relevant behavior tests while editing and full `make check`/`make test` before
delivery. `make audit` runs govulncheck and `npm audit` for dependency
advisories; it needs module download access. CI retains browser failure traces.

## A good pull request

Explain the concrete problem, resulting behavior, validation and limitations.
Keep changes cohesive and preserve full-diff review, fresh reviewer threads,
persistent repair threads, exact model routes and verification before publication.
Add behavior tests for meaningful changes; avoid tests that merely duplicate code.
Keep Go/dashboard types and documentation aligned. Saved configuration and task
snapshots must still load after upgrades.

Workers must not push or publish; the orchestrator owns publication. Do not merge, deploy,
migrate production systems, edit live `.octomus/` state, or commit credentials,
raw transcripts or private billing images. Octomus-created branches use `tyk/`.
License/NOTICE ownership changes await owner-cleared facts.

Requests for additional backends are welcome; Codex and OpenCode are the supported
backends.
