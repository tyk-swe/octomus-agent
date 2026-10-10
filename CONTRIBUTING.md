# Contributing

Octomus serves one operator and one repository. Start with a concrete problem,
reproduction or proposal; a justified no-change outcome is welcome. Report security issues
privately using [SECURITY.md](SECURITY.md).

## Before you change code

Read [AGENTS.md](AGENTS.md) for the repository map, security boundaries, branch naming,
build and verification commands, and generated-file rules. Read the relevant design
reference before changing a contract:

- [Architecture](docs/architecture.md) for runtime and state behavior.
- [Sandbox](docs/sandbox.md) and [threat model](docs/threat-model.md) for isolation.
- [Run evidence](docs/run-evidence.md) for exported evidence.
- [Releasing](docs/releasing.md) for versioning and package contents.

The dashboard is embedded in the Go binary. Build its assets before Go analysis, and run
the focused package checks while editing. The complete command inventory and test stages
are maintained in [AGENTS.md#build-and-verify](AGENTS.md#build-and-verify); do not copy that
list into another guide.

## Pull requests

Explain the concrete problem, resulting behavior, validation and relevant limitations.
Keep changes cohesive, preserve full-diff review and exact model routes, and add behavior
tests for meaningful changes. Keep Go and dashboard types, saved records and documentation
aligned; database changes must preserve the checked-in golden states.

Workers must not push or publish; the orchestrator owns publication. Do not merge, deploy
or migrate production systems, edit live `.octomus/` state, or commit credentials, raw
transcripts or private billing images. Octomus-created branches use `tyk/`. Defer
LICENSE/NOTICE ownership changes until the owner supplies cleared facts.
