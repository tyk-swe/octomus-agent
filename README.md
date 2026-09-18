# Octomus Agent

[![Repository checks](https://github.com/tyk-swe/octomus-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/tyk-swe/octomus-agent/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

**Your repo’s next improvement. Ready for review.**

Octomus finds useful improvements in your repository, challenges them with two independent
reviewers, and turns accepted work into reviewed, verified pull requests through Codex or
OpenCode. It can reject every proposal and do nothing. You decide what merges.

**Self-hosted preview · One operator · One repository · Your Codex or OpenCode access**

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

**[Homepage](https://octomus-agent.tyk.sh/) · [Documentation](https://octomus-agent.tyk.sh/docs/) · [Explore a sample run](https://octomus-agent.tyk.sh/showcase/)**

Explore a useful fix, a rejected rewrite, a deferred optimization, and a blocked task in the
guided demo at [octomus.tyk.sh](https://octomus.tyk.sh/). The public homepage at
[octomus-agent.tyk.sh](https://octomus-agent.tyk.sh/) and [showcase](docs/showcase.md) need
no service, account, token, or database. All sample records and screenshots are explicitly
synthetic.

```sh
npm ci --prefix web
npm run site:build --prefix web
python3 tests/serve_site.py
```

Then open **http://127.0.0.1:4310/**.

The homepage and searchable documentation are published through Cloudflare Workers at
`https://octomus-agent.tyk.sh/`, with the hosted demo also at `https://octomus.tyk.sh/`.
Publishing is a manual step. The [launch guide](docs/product-hunt.md) contains deployment
instructions, Product Hunt copy, and gallery assets.

## Getting started

Octomus is built from source; release binaries and the crates.io package are not
published yet. You need a dedicated Ubuntu 24.04 VM, your own Codex or OpenCode
provider access, a GitHub identity reserved for the agent, and a clone of the target
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
audit** or **Run once**. Start with an audit to inspect recommendations without queuing
code changes. A later execution run plans afresh.

VM and provider costs depend on your setup. Session admissions are operating limits,
not dollar caps; validated per-task and daily cost figures are not available yet.

**[Full installation and first-run walkthrough →](docs/getting-started.md)**

## Documentation

| Document | What it covers |
| --- | --- |
| [Documentation overview](docs/index.md) | Find your path through the guides |
| [Getting started](docs/getting-started.md) | Prerequisites, install, service user, first run |
| [Configuration](docs/configuration.md) | Repository, routes, checks, and operating limits |
| [Architecture](docs/architecture.md) | Cycle, task and review contracts; storage and trust |
| [Model routing](docs/model-routing.md) | Per-role Codex and OpenCode selection |
| [Configuration example](docs/configuration.example.json) | Every saved policy field with its shipped default |
| [Deployment](docs/deployment.md) | systemd, controls, limits, retention, backup, HTTP API |
| [Run evidence](docs/run-evidence.md) | What `RunEvidenceV1` reports, and what it never claims |
| [Showcase](docs/showcase.md) | The standalone static run explorer |
| [Product Hunt launch](docs/product-hunt.md) | Public site, launch copy, gallery, and publishing checklist |
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
