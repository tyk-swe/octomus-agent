# Contributing

Octomus serves one operator and one or more repositories. Start with a concrete problem,
reproduction or proposal; a justified no-change outcome is welcome. Report security issues
privately using [SECURITY.md](SECURITY.md).

## Before you change code

Read [AGENTS.md](AGENTS.md) for contribution rules and [Build and verify](#build-and-verify)
for local checks. Read the owning design reference before changing a contract:
[Architecture](docs/architecture.md), [Sandbox](docs/sandbox.md), [Threat model](docs/threat-model.md),
[Run evidence](docs/run-evidence.md) or [Releasing](docs/releasing.md).

## Build and verify

Use the Go version in [go.mod](go.mod) and the Node version in [.node-version](.node-version),
then install dashboard dependencies with `npm ci --prefix web`. The [Makefile](Makefile)
defines the build and verification targets.

Run `make check` for Go formatting and vet, Svelte type checks and dashboard formatting.
It builds the dashboard first. Use `make test-go GO_TEST_PACKAGES=./internal/<package>`
for an affected Go package, `make test-ui-logic` for dashboard logic, or `make test-python`
for Python helpers. Run `make build` first when a check needs `bin/octomus-agent`.

Leave broad race, integration, browser, sandbox, packaging and production-image checks
to [CI](.github/workflows/ci.yml) unless the change needs a local reproduction. The opt-in
sandbox and production-image targets need the host tools listed in the Makefile.
Record which checks ran and which remain deferred.

## Pull requests

Explain the concrete problem, resulting behavior, validation and limitations. Keep changes
cohesive, add behavior tests, and keep Go types, `web/src/lib/types.ts`, saved records,
checked-in golden databases and documentation aligned. Validate with the smallest check in
[Build and verify](#build-and-verify) and leave broad suites to CI. Follow the branch
naming, publication, sandbox and secret-handling rules in [AGENTS.md](AGENTS.md).
