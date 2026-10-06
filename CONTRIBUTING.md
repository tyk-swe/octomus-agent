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

`make build` creates the production executable at `bin/octomus-agent`. `--assets web/build`
explicitly serves a development dashboard instead of the embedded copy. The
application version lives in the root `VERSION` file; change it in one place.
See [architecture](docs/architecture.md) and [AGENTS.md](AGENTS.md) for the code map.

## Meaningful evidence

`make test` runs the regular Go suite, the race suite, then three checks against the
freshly built binary: `tests/distribution.py` (the executable as shipped; CI also
runs it on each extracted release archive), `tests/e2e.py` (the nine service scenarios against
deterministic Codex, OpenCode and GitHub peers, without live model calls or network
writes) and `npm test --prefix web` (the dashboard browser tests). AGENTS.md lists the
suites, the fixtures and the shared test helpers.

Focused targets run one stage each: `make test-go`, `make test-go-race`,
`make test-integration` (the distribution smoke, then the scenarios; with `SCENARIOS`, for example
`make test-integration SCENARIOS="chain pr-outcome"`) and `make test-browser` (with
`PLAYWRIGHT_ARGS` such as `--project=desktop`). `tests/e2e.py` also takes scenario
names directly and lists them on an unknown name; `OCTOMUS_TEST_JOBS` bounds its
workers and `OCTOMUS_TEST_BINARY` points it at another executable. The Go suites run
with `-shuffle=on`, and a failure prints the seed that reproduces its order.

The Go and Python fixtures isolate inherited Git configuration and
repository-locating environment variables, so personal signing settings, hooks and
shell overrides cannot change fixture setup or redirect its writes. Fixture lookup
resolves the source checkout before tests run, so `-trimpath` builds work when
launched from inside the checkout.

Tests that need something external skip unless an environment variable provides it:

- `OCTOMUS_CONTRACT_CODEX_BINARY` and `OCTOMUS_CONTRACT_OPENCODE_BINARY`: a pinned CLI for
  `TestPinnedCodexContract` and `TestPinnedOpenCodeContract`, which drive real turns
  against a synthetic provider. CI's `client-contracts` job runs them at the versions
  pinned in `.github/workflows/ci.yml`; run them locally before changing a runner adapter.
- `OCTOMUS_SCALE_TEST=1`: the store's history scale check, `TestBoundedHistoryScale`.
- `OCTOMUS_DOCKER_TEST=1` (through `make test-sandbox`): the broker against a real
  Docker Engine and the compose stack.

```bash
npm install --prefix /tmp/octomus-contract --no-audit --no-fund @openai/codex@0.153.4
OCTOMUS_CONTRACT_CODEX_BINARY=/tmp/octomus-contract/node_modules/.bin/codex \
  go test ./internal/runner -run '^TestPinnedCodexContract$' -count=1 -v
OCTOMUS_SCALE_TEST=1 go test ./internal/store -run TestBoundedHistoryScale -v -count=1
```

The browser tests use clearly synthetic data and include axe WCAG A/AA and overflow
checks; their screenshots are not live operating evidence. To refresh
`docs/dashboard.png`, build the current dashboard and binary, then run
`npm run launch:assets --prefix web` and inspect the image before committing it.

Run relevant behavior tests while editing and full `make check`/`make test` before
delivery. `make audit` runs govulncheck and `npm audit` for dependency advisories; it
needs module download access. CI retains browser failure traces.

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
