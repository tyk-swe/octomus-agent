# Octomus Agent

**When the project builds itself.**

Octomus continuously discovers useful improvements in a repository, challenges the ideas, implements accepted work through Codex, and delivers reviewed pull requests. It can also keep improving existing Octomus PRs. When there is nothing worthwhile to do, it idles.

A Rust service owns the workflow and durable state. A SvelteKit dashboard provides configuration, progress, and operator controls. One process serves the production dashboard and API; there is no Node server to operate.

![Octomus dashboard](docs/dashboard.png)

*Dashboard shown with synthetic browser-test data.*

## What the MVP does

- Grounds planning in the default branch, recorded work, and open `octomus/*` PRs.
- Runs 8–10 complementary discovery sessions, two independent proposal reviews, and final orchestrator consolidation.
- Routes XS/S/M/L/XL tasks to the exact configured model and reasoning effort. Unsupported routes stop visibly.
- Gives every task its own Codex thread and `.octomus/tasks/<thread-id>/workspace` checkout.
- Uses a fresh reviewer for every review round, with one persistent Astra-medium repair thread per task.
- Runs configured verification on the reviewed revision before creating or updating a GitHub PR.
- Preserves interrupted work, reconciles publication, serializes branch writers, and enforces operating limits.
- Provides private dashboard access, task cancellation/retry, pause/resume, maintenance settings, and inspection of review and verification evidence.

Delivery ends at a PR. The service does not merge, deploy, or perform production migrations. The execution model is deliberately **unsandboxed**, intended for a dedicated Linux VM and a single operator.

## Quick start

Requirements: Linux, Rust 1.88+ with Cargo, Node.js 22.12+ for building the dashboard, Git, GitHub CLI (`gh`), and an authenticated Codex CLI with app-server support. Python 3 is needed only for the integration tests. The protocol was checked against Codex CLI **0.153.4**.

```bash
cargo build --release --locked
npm ci --prefix web
npm run build --prefix web

export OCTOMUS_TOKEN="$(openssl rand -hex 32)"
./target/release/octomus-agent
```

Open **http://127.0.0.1:4200** and enter the value of `OCTOMUS_TOKEN`. The token remains in the browser tab's memory; refreshing the page requires signing in again. The service starts paused with an empty configuration.

In **Configuration**:

1. Enter the absolute path of a Git checkout and its `owner/repository` GitHub name. Its `origin` must point to that repository using SSH or credential-free HTTPS.
2. Choose models and efforts for the orchestrator, discovery agents, proposal reviewers, and code reviewer. These roles intentionally have no invented defaults. **Load available models** reads the installed runtime's catalog.
3. Enter meaningful verification commands, one per line. All commands must pass before publication. Keep credentials in the host's authentication/environment, not these fields.
4. Save, then **Check connection** to validate repository settings, GitHub/Codex authentication, and every model route.
5. Select **Run a cycle**. This enables continuous operation; **Pause** prevents new tasks and cycles while in-flight work finishes.

Authenticate `codex login` and `gh auth login` as the same OS account that runs Octomus. SSH Git access must also work without an interactive prompt. Configure GitHub's Git credential helper if using HTTPS (`gh auth setup-git`).

The default execution routes are:

| Tier | Model | Reasoning |
| --- | --- | --- |
| XS | `gpt-5.6-luna` | `xhigh` |
| S | `gpt-5.6-luna` | `max` |
| M | `gpt-6-astra` | `low` |
| L | `gpt-6-astra` | `medium` |
| XL | `gpt-6-astra` | `high` |

Repair always uses `gpt-6-astra` / `medium`. Availability depends on the installed Codex runtime and account. Octomus never substitutes routes silently.

## Development and verification

```bash
# Production build and Rust tests
cargo test --locked
cargo clippy --all-targets --locked -- -D warnings
cargo fmt --check
npm ci --prefix web
npm run check --prefix web
npm run build --prefix web

# Real Git + SQLite + service integration; deterministic external peers
cargo build --locked
python3 tests/e2e.py

# Browser tests against the Rust server, including mobile and accessibility
npx --prefix web playwright install chromium
npm test --prefix web
```

The integration suite never invokes a real model or writes to GitHub. It exercises the complete workflow with protocol-compatible Codex/GitHub fixtures and actual local Git repositories. A real authenticated repository smoke run is still required when commissioning a host; fixture tests cannot establish model quality, account entitlements, or network reliability.

For UI development, run the Rust service with built assets, then `npm run dev --prefix web`. Vite proxies `/api` to port 4200. The production build is static and is served by Rust.

## Deployment and operations

Use the [dedicated-host deployment guide](docs/deployment.md) for systemd, private remote access, backups, recovery, and upgrade instructions. `make package` builds a Linux release directory and archive under `dist/`.

- [Your launch checklist](todo.md)
- [Architecture and operational contract](docs/architecture.md)
- [PRD acceptance coverage](docs/acceptance.md)
- [Default configuration](docs/configuration.example.json)
- [Product requirements](PRD.md)

Apache-2.0 licensed. See [LICENSE](LICENSE).
