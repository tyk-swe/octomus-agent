# Octomus Agent

[![Repository checks](https://github.com/tyk-swe/octomus-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/tyk-swe/octomus-agent/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

**Your repository's next improvement, ready for review.**

Octomus is a self-hosted service for one operator and one repository. It discovers
improvements, has independent agents review the proposals, executes accepted work through
Codex or OpenCode, verifies the result, and delivers a pull request. A cycle may reject
everything and do nothing. The operator decides what merges.

Maintenance delivery is an opt-in exception: a small, reviewed and verified maintenance
pull request may be squash-merged only when fresh GitHub checks and repository protections
allow it. It requires the separate [maintenance merge setup](docs/maintenance-merging.md).

![Octomus dashboard](docs/dashboard.png)

*This image uses synthetic browser-test data.*

## Operating model

- Planning grounds proposals in the repository, its instructions, history and owned pull
  requests. External pull requests are read-only targets.
- Each task has its own checkout, a fresh full-diff review and bounded verification. Durable
  state lets the service recover interrupted work and reconcile publication.
- Docker is the default boundary: runners and verification commands run in short-lived,
  non-root sandboxes without the GitHub credential. A dedicated VM can run with
  `--sandbox off`; its service account and host are then the boundary.

Read the [threat model](docs/threat-model.md) and [sandbox guide](docs/sandbox.md) before
running repository content.

## Quick start

For prerequisites, installation and the first run, follow [Getting started](docs/getting-started.md).
The short version is:

1. Deploy the Docker stack or the dedicated-VM service.
2. Sign in the runner and open the loopback dashboard through SSH.
3. Enter and save the repository, routes and verification commands.
4. Check the saved configuration, then start with **Run an audit** or **Run once**.

The dashboard starts paused. Session admissions are operating limits, not dollar budgets;
see [cost and usage](docs/cost.md).

## Documentation

| Document | Use it for |
| --- | --- |
| [Getting started](docs/getting-started.md) | Prerequisites and the first run |
| [Deployment](docs/deployment.md) | Docker/VM installation, operations, recovery, backups, CLI and API |
| [Configuration](docs/configuration.md) | Saved policy fields, routes, checks and limits |
| [Model routing](docs/model-routing.md) | Codex and OpenCode route setup |
| [Configuration example](docs/configuration.example.json) | Complete saved-policy shape and defaults |
| [Architecture](docs/architecture.md) | Durable runtime and design contract |
| [Sandbox](docs/sandbox.md) | Isolation lifecycle, egress and self-test |
| [Threat model](docs/threat-model.md) | Trust boundaries and residual risk |
| [Run evidence](docs/run-evidence.md) | `RunEvidenceV1` export and its limits |
| [Cost](docs/cost.md) | Session admissions and usage reporting |
| [Maintenance merging](docs/maintenance-merging.md) | Destination protections for automatic maintenance merges |
| [Releasing](docs/releasing.md) | Packaging and release workflow |

## Security and contribution

Repository content, dependencies and model output are untrusted. Keep the dashboard on
loopback behind SSH, use a random operator token and restrict the GitHub token to this
repository. Report vulnerabilities privately using [SECURITY.md](SECURITY.md).

Contribution rules and the repository map are in [CONTRIBUTING.md](CONTRIBUTING.md) and
[AGENTS.md](AGENTS.md). License: [Apache-2.0](LICENSE).
