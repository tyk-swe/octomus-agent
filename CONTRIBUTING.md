# Contributing

Octomus serves one operator and one repository. Start with a concrete problem,
reproduction or proposal; a justified no-change outcome is welcome. Report security issues
privately using [SECURITY.md](SECURITY.md).

## Before you change code

Read [AGENTS.md](AGENTS.md) for the repository map, [build and verify commands](AGENTS.md#build-and-verify),
generated-file rules and boundaries. Read the owning design reference before changing a contract:
[Architecture](docs/architecture.md), [Sandbox](docs/sandbox.md), [Threat model](docs/threat-model.md),
[Run evidence](docs/run-evidence.md) or [Releasing](docs/releasing.md).

## Pull requests

Explain the concrete problem, resulting behavior, validation and limitations. Keep changes
cohesive, add behavior tests, and keep Go types, `web/src/lib/types.ts`, saved records,
checked-in golden databases and documentation aligned. Follow the branch naming,
publication, sandbox and secret-handling rules in [AGENTS.md](AGENTS.md).
