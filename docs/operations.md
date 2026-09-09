# Operator checklist

Use this checklist to commission and operate a dedicated host. Repository tests establish behavior with fixtures; they do not validate live accounts, model quality or installation time. Record live validation results with the tested revision, host, date and private evidence references. See [cost methodology](cost.md) for usage measurements.

## 1. Prepare the host

- [ ] Use a dedicated Linux VM for Octomus. Agents execute without a sandbox and have the service account's host permissions.
- [ ] Create the `octomus` service account with home directory `/var/lib/octomus` if using the supplied systemd unit.
- [ ] Install Git, GitHub CLI, curl (for optional outbound notifications), Codex CLI 0.153.4, and your target project's build tools. Building Octomus requires Rust 1.88+, Node.js 22.12+, npm, and a C compiler. Its integration tests also require Python 3.
- [ ] Clone the repository you want Octomus to improve into a persistent path writable by the service account, such as `/srv/projects/octomus-agent`. Keep this checkout separate from the installed binary at `/usr/local/bin/octomus-agent`.

## 2. Connect your accounts

Run these as the same OS account that will run the service:

```bash
codex login
gh auth login
gh auth setup-git
```

- [ ] Confirm GitHub access can fetch and push branches and create/update PRs in the target repository. If using SSH, configure working non-interactive SSH credentials; the HTTPS credential helper is then optional.
- [ ] Confirm the repository's `origin` uses SSH or credential-free HTTPS and points to the intended GitHub repository.
- [ ] Confirm your Codex account exposes the models and reasoning efforts you plan to use. Repair defaults to `gpt-6-astra` with `medium` and can be configured explicitly; unsupported routes are blocked rather than silently replaced.

## 3. Build and install

```bash
git clone https://github.com/tyk-swe/octomus-agent.git
cd octomus-agent
npm ci --prefix web
make package
```

- [ ] Install `target/release/octomus-agent` into `/usr/local/bin/octomus-agent`, or use a checksum-verified published release. The dashboard is embedded; no separate assets are needed. Release/crates.io publication remains pending.
- [ ] Generate a random operator token with `openssl rand -hex 32`. Store it as `OCTOMUS_TOKEN` in `/etc/octomus/agent.env`, with permissions restricted to the appropriate administrator/service account. Keep a copy in your password manager for dashboard login.
- [ ] Install [the systemd unit](../deploy/octomus-agent.service), following [the deployment guide](deployment.md). Keep `KillMode=control-group` so restarts terminate old task processes.
- [ ] Start the service and confirm `systemctl status octomus-agent` reports it running. Check `journalctl -u octomus-agent` if startup fails.
- [ ] Access the private dashboard through an SSH tunnel, then sign in using your operator token:

```bash
ssh -N -L 4200:127.0.0.1:4200 your-host
```

Open **http://127.0.0.1:4200**. The first launch is paused.

## 4. Configure your project

- [ ] Set the absolute repository path, GitHub `owner/repository`, and default branch. To improve this project, the GitHub value is `tyk-swe/octomus-agent` and the default branch is `main`.
- [ ] Set the owned branch prefix. For this repository, use **`tyk/`** to match your branch naming instruction; the product's initial default is `octomus/`.
- [ ] Use **Load available models**, then explicitly choose a model and effort for all four roles: orchestrator, discovery, proposal reviewers, and code reviewer.
- [ ] Verify the five execution tier routes. If your runtime does not support a route, configure an available replacement explicitly before starting work.
- [ ] Enter meaningful verification commands for the target project. For Octomus itself, useful commands include:

```bash
npm ci --prefix web && make check && make test
```

Install Rust/rustfmt/clippy, Python 3, Node 22.12+ and Playwright Chromium/system
prerequisites first. The dashboard is built before Rust so cold clones can embed
it. This is one verification command; all configured commands must pass on the
reviewed revision before publication. Measure cold timings before changing limits.

- [ ] Optionally set a notification URL that you control. Published and blocked tasks, failed cycles, completed audits and error pauses are posted there as JSON so you need not keep the dashboard open.
- [ ] Optionally enter operator guidance: short standing instructions for planning, such as areas to leave alone or a current priority. Keep it concise; it is appended to every planning prompt.
- [ ] Choose enabled improvement categories, maintenance cadence, and resource/time limits appropriate to your host and account. Start with one concurrent task, one accepted task per cycle and a six-hour cycle interval for the initial live run. Discovery still requires 8–10 agents.
- [ ] Confirm operation uses existing subscription allowance only and paid overage is disabled. Set the daily session budget, recognizing that admissions are not an allowance or dollar-spend cap.
- [ ] Save configuration and run **Check connection**. Correct any reported configuration, authentication, or route errors.

## 5. Validate the first real cycle

- [ ] While paused with no active work, run **Check audit connection**, then **Run an audit**. Audits need the three planning-role routes but no verification commands. Inspect every decision and both assessments. Confirm the queue is unchanged and operation remains paused. An audit does not sandbox agents or guarantee absence of malicious external effects.
- [ ] For execution, configure the code reviewer, execution tiers, repair route and verification commands, then use **Check connection**. Audit results are recommendations; an executing cycle plans afresh.

- [ ] Select **Run a cycle**. This also enables subsequent continuous cycles.
- [ ] Confirm discovery reads the repository and existing owned PRs, and proposal decisions include reasons.
- [ ] Inspect the first task's workspace, executor/reviewer/repair sessions, verification output, and any blocked state.
- [ ] Confirm a successfully reviewed task creates or updates the expected GitHub PR, with verification evidence, while leaving `main` untouched. An idle cycle with no worthwhile proposals is also a valid outcome.
- [ ] Review the actual PR and its GitHub checks before merging it. Automated fixture tests do not replace this first authenticated, real-repository validation.
- [ ] Exercise **Pause** and **Resume**. Pause prevents new work; in-flight tasks may finish and publish. Use a task's **Cancel task** control when you want to stop that task.

## 6. Enable ongoing operation

- [ ] Leave the system running once you are satisfied with the first live results; increase throughput only as needed.
- [ ] Monitor blocked/failed tasks, model usage, host disk capacity, and the value of generated PRs.
- [ ] Set up protected backups of the state directory, task workspaces, and the service account's Codex thread state using the [backup procedure](deployment.md#backup-and-upgrade).
- [ ] Review retention settings, including Codex's separate transcript storage. Unresolved workspaces are intentionally preserved and can require deliberate cleanup.
- [ ] Continue reviewing and merging useful PRs yourself. Application upgrades and production deployments remain your responsibility; Octomus delivers PRs.
