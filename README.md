# Octomus Agent

[![Repository checks](https://github.com/tyk-swe/octomus-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/tyk-swe/octomus-agent/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

Octomus finds useful improvements in a repository, challenges them with two independent
reviewers, and delivers verified pull requests through Codex or OpenCode. It can reject
every proposal and do nothing.

Think **Dependabot, with features**: you choose the repository and the boundaries, and it
discovers the work. Delivery stops at a pull request for you to review and merge.

![Octomus dashboard](docs/dashboard.png)

*This image uses synthetic browser-test data.*

## How it decides

Grounding inspects code, `AGENTS.md`, history and existing owned pull requests, with
bounded, source-attributed contributor and fork context to help identify overlapping
work. External pull requests are never writable targets, and recorded coverage names
omitted or truncated context.

Eight to ten discovery agents explore complementary areas; two independent adversaries
challenge their proposals. The orchestrator records a reason for every decision. An
execution cycle queues accepted work; an audit only records recommendations, and a later
execution cycle plans afresh rather than executing an old audit result.

Each task gets its own execution thread and checkout. Every code review uses a **fresh
reviewer** and the **complete accumulated diff**. Repairs use one **persistent repair
thread** per task. Configured verification must pass on the reviewed revision before
Octomus publishes or updates a pull request. Interrupted work and publication are
reconciled from durable state.

## See a run before installing anything

The [showcase](docs/showcase.md) is a standalone static build of recorded run evidence —
no service, account, token or database. Three commands from a clean checkout:

```sh
npm ci --prefix web
npm run showcase --prefix web -- --mode fixture --input showcase/synthetic.public.json
python3 -m http.server 4307 --bind 127.0.0.1 --directory dist
```

Then open `http://127.0.0.1:4307/showcase/`.

## Getting started

Octomus is built from source; release binaries and the crates.io package are not
published yet. You need a dedicated Ubuntu 24.04 VM, your own Codex or OpenCode
subscription, a GitHub identity reserved for the agent, and a clone of the target
repository on a persistent path.

```sh
git clone https://github.com/tyk-swe/octomus-agent.git
cd octomus-agent
npm ci --prefix web
make build
sudo install -m 755 target/release/octomus-agent /usr/local/bin/octomus-agent
```

The dashboard listens on loopback and starts paused. The first run is four explicit
moves: enter the configuration, save it, check the connection, then choose **Run an
audit** or **Run once**.

**[Full installation and first-run walkthrough →](docs/getting-started.md)**

## Documentation

| Document | What it covers |
| --- | --- |
| [Getting started](docs/getting-started.md) | Prerequisites, install, service user, first run |
| [Architecture](docs/architecture.md) | Cycle, task and review contracts; storage and trust |
| [Model routing](docs/model-routing.md) | Per-role Codex and OpenCode selection |
| [Deployment](docs/deployment.md) | systemd, controls, limits, retention, backup, HTTP API |
| [Run evidence](docs/run-evidence.md) | What `RunEvidenceV1` reports, and what it never claims |
| [Showcase](docs/showcase.md) | The standalone static run explorer |
| [Cost](docs/cost.md) | What a session admission is, and what is not measured |
| [Threat model](docs/threat-model.md) | Trust boundaries, prompt injection, redaction limits |
| [Releasing](docs/releasing.md) | Packaging, release workflow, crate handoff |

## Security

The dedicated VM is the sandbox: runner and verification commands have the service
user's permissions, and repository prompt injection is not prevented by design. Keep the
single-operator dashboard on loopback behind SSH, with a random private token and
repository-restricted GitHub authentication. Read the
[threat model](docs/threat-model.md) before running work, and report vulnerabilities
privately to **mail@mail.tyk.sh** using [SECURITY.md](SECURITY.md).

## Contributing

Octomus runs on Codex or OpenCode, single-operator and single-repository. See
[contributing](CONTRIBUTING.md), the [changelog](CHANGELOG.md) and the
[configuration example](docs/configuration.example.json).

License: [Apache-2.0](LICENSE).
