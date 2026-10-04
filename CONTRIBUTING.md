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
`make test-race-e2e` (about a minute; needs a C compiler) runs `tests/e2e.py`
against that variant so the race detector sees real HTTP, scheduler and runner
interleavings; it is not part of `make test`. `--assets web/build`
explicitly serves a development dashboard instead of the embedded copy. The
application version lives in the root `VERSION` file; change it in one place.
See [architecture](docs/architecture.md) and [AGENTS.md](AGENTS.md) for the code map.

## Meaningful evidence

`make test` runs the regular Go suite and then the race suite, then these
checks against the freshly built binary:

- `tests/distribution.py`: the executable as shipped: embedded dashboard over HTTP,
  state lock and its release on SIGTERM, listener warning. With `--package` (CI and
  the release workflow) it also checks the release archive against
  `scripts/release-files.txt`.
- `tests/e2e.py`: the service scenarios. They run the real service, SQLite and local
  Git with deterministic Codex, OpenCode and GitHub peers (`tests/fixtures`) in
  temporary directories, without live model calls or network writes: `normal`,
  `normal-opencode` and `normal-mixed` (planning, two deliveries with reviews,
  repairs, verification, redacted publication and the doctor), `interrupt-publication`,
  `audit`, `chain` (dependent tasks on one PR, then a duplicate plan refused),
  `pr-outcome`, `baseline` and `notify`.
- `npm test --prefix web`: the dashboard browser tests, including the evidence display
  rules `web/tests/evidence.spec.ts` checks without a page.

Focused targets run one stage each against the built binary: `make test-go`
(regular Go suite), `make test-go-race` (race suite explicitly),
`make test-contracts`, `make test-integration` (with `SCENARIOS`, for example
`make test-integration SCENARIOS="chain pr-outcome"`) and `make test-browser` (with
`PLAYWRIGHT_ARGS` such as `--project=desktop`).

`tests/e2e.py` accepts scenario names directly, for example
`python3 tests/e2e.py chain pr-outcome`; an unknown name lists the available ones.
Scenarios run with up to four workers; `OCTOMUS_TEST_JOBS` sets the limit
(1 runs serially).
Set `OCTOMUS_TEST_BINARY` to test another executable. The Python fixture harness
and the Go engine, Git and HTTP fixture suites isolate inherited Git configuration
and repository-location environment variables, so personal signing settings,
hooks and shell Git overrides cannot change fixture setup or redirect its writes.
Repository-local settings and explicit test environment overrides still apply.
Fresh-fixture scenario admission assertions compare the complete ledger and summed
daily counters, so a run crossing UTC midnight still checks every turn. Use
current-day counts only when testing daily budget behavior itself.
Go tests share polling and process helpers through `internal/testutil`, and inject `internal/runner/runnertest`
in place of runner processes; `internal/schemas/schematest` holds each structured-output
schema to the Go type that decodes its answers. `web/types_contract_test.go` holds the
dashboard's TypeScript types, limits and vocabularies to the Go records and
validation. Browser tests use clearly synthetic
data; their screenshots are not live operating evidence.

Go tests dispatch `git`/`gh` through a small POSIX shell relay
(`internal/testutil.InstallFixtureCommands`) that walks up from the working
directory to the nearest fixture root and execs that fixture's `bin/git` or
`bin/gh`, so fixture setup costs a shell fork instead of a Python interpreter
start. Both the Go and Python suites redirect the fixture's GitHub identity
through the same `tests/fixtures/git.sh`. Fixture lookup also supports
`go test -trimpath` and `GOFLAGS=-trimpath`: the source checkout is resolved before
tests run, so later working-directory changes do not redirect fixtures. Standalone
trimmed test binaries that use these fixtures must be launched from inside the
source checkout; the scripts are not embedded in the test executable.
`make test-go` and
`make test-go-race` run with `-shuffle=on`; a failure prints its seed
(`-test.shuffle N`) so any hidden test-order coupling reproduces.

Some checks run only in CI, because each needs something a working copy does not have:
`make package` followed by `tests/distribution.py --package`, which needs a real
release build; `shellcheck`; and the
`client-contracts` job, which runs the runner adapters against the real Codex and
OpenCode CLIs at the versions pinned in `.github/workflows/ci.yml`. Run any of them
locally before changing packaging, the installer or a runner adapter.

Tests that need something external skip unless an environment variable provides it:

- `OCTOMUS_CONTRACT_CODEX_BINARY` and `OCTOMUS_CONTRACT_OPENCODE_BINARY`: a pinned CLI for
  `TestPinnedCodexContract` and `TestPinnedOpenCodeContract`, which drive real turns
  against a synthetic provider.
- `OCTOMUS_SCALE_TEST=1`: the store's history scale check, `TestBoundedHistoryScale`.

Install the CLI version the `client-contracts` job pins into a scratch prefix, as the
job does; for example, for Codex and the store check:

```bash
npm install --prefix /tmp/octomus-contract --no-audit --no-fund @openai/codex@0.153.4
OCTOMUS_CONTRACT_CODEX_BINARY=/tmp/octomus-contract/node_modules/.bin/codex \
  go test ./internal/runner -run '^TestPinnedCodexContract$' -count=1 -v
OCTOMUS_SCALE_TEST=1 go test ./internal/store -run TestBoundedHistoryScale -v -count=1
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
