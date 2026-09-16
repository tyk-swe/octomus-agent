# Operator checklist

Use this checklist to commission and operate a dedicated host. Repository tests establish behavior with fixtures; they do not validate live accounts, model quality or installation time. Record live validation results with the tested revision, host, date and private evidence references. See [cost methodology](cost.md) for usage measurements.

## 1. Prepare the host

- [ ] Use a dedicated Linux VM for Octomus. Agents execute without a sandbox and have the service account's host permissions.
- [ ] Create the `octomus` service account with home directory `/var/lib/octomus` if using the supplied systemd unit.
- [ ] Install Git, GitHub CLI, the runners your routes select (Codex CLI 0.153.4 and/or OpenCode 1.18.30), and your target project's build tools. Building Octomus requires Rust 1.88+, Node.js 22.12+, npm, and a C compiler. Its integration tests also require Python 3.
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
- [ ] Confirm your provider account exposes the models and reasoning efforts you plan to use. Repair defaults to `gpt-6-astra` with `medium` and can be configured explicitly; unsupported routes are blocked rather than silently replaced.

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
- [ ] Use **Load Codex models** or **Load OpenCode models**, then choose a runner, model, and supported effort/variant for all four roles: orchestrator, discovery, proposal reviewers, and code reviewer. OpenCode also requires a provider configured as the service user; see [model routing](model-routing.md).
- [ ] Verify the five execution tier routes. If your runtime does not support a route, configure an available replacement explicitly before starting work.
- [ ] Enter meaningful verification commands for the target project. For Octomus itself, useful commands include:

```bash
npm ci --prefix web && make check && make test
```

Install Rust/rustfmt/clippy, Python 3, Node 22.12+ and Playwright Chromium/system
prerequisites first. The dashboard is built before Rust so cold clones can embed
it. This is one verification command; all configured commands must pass on the
reviewed revision before publication. Measure cold timings before changing limits.

- [ ] Choose enabled improvement categories, maintenance cadence, and resource/time limits appropriate to your host and account. Start with one concurrent task, one accepted task per cycle and a six-hour cycle interval for the initial live run. Discovery still requires 8–10 agents.
- [ ] Confirm operation uses existing subscription allowance only and paid overage is disabled. Set the daily session budget, recognizing that admissions are not an allowance or dollar-spend cap.
- [ ] Set **Open PR capacity** to match review bandwidth (default five owned open PRs). The ceiling also accounts for admitted deliveries; existing-PR maintenance can continue at capacity.
- [ ] Save configuration and run **Check connection**. The **Setup checklist** at the top of Configuration labels each step as entered, saved, checked or ran and links to these controls; it starts nothing. Both connection checks validate saved values and are disabled while edits remain unsaved. Catalog loading uses the executable paths entered in the form. Correct any reported configuration, authentication, or route errors.

Configuration drafts, verification commands and loaded catalogs stay in this tab
when switching dashboard views. **Unsaved changes** identifies a draft;
**Discard changes** restores the last loaded or saved values without a server write.
Revisiting a clean form refreshes saved configuration. Dirty edits and failed saves
retain the draft. Disconnect, session expiry and page reload clear it. Retry a
failed configuration load with **Retry**.

Optionally use **Check clean baseline** before spending model admissions. Save or
discard drafts, pause and wait for active work, then explicitly confirm the saved
commands. They execute with the service user's permissions on an identified remote
default-branch clone. Inspect bounded command results, checked SHA, configuration
match and observation freshness. Cancel the check if needed; interrupted checks are
not replayed. A pass is point-in-time environment evidence, not later task verification.

## 5. Validate the first real cycle

- [ ] While paused with no active work, run **Check audit connection**, then **Run an audit**. Audits need the three planning-role routes but no verification commands. Inspect every decision and both assessments. Confirm the queue is unchanged and operation remains paused. An audit does not sandbox agents or guarantee absence of malicious external effects.
- [ ] For execution, configure the code reviewer, execution tiers, repair route and verification commands, then use **Check connection**. Audit results are recommendations; an executing cycle plans afresh.

- [ ] Select **Run once**. Confirm one planning cycle and its accepted task batch finish, then the service returns to paused. Select **Start continuous** separately when ready for ongoing scheduling.
- [ ] Confirm discovery reads the repository, owned PRs and recorded read-only contributor/fork PR summaries. Inspect coverage/omissions and source links in run inspection; external branches must never become execution targets.
- [ ] Confirm a planning pass has its full 12–14 admissions available (13 with nine discovery agents). Unaffordable manual starts are refused; continuous scheduling waits. A daily limit below the requirement needs a policy change, not just midnight.
- [ ] Inspect the first task's workspace, executor/reviewer/repair sessions, verification output, and any blocked state.
- [ ] Use **Inspect run** on the Overview, or `--export-run`, to review the recorded review and check evidence for that cycle; see [run evidence](launch/run-evidence.md).
- [ ] Confirm a successfully reviewed task creates or updates the expected GitHub PR, with verification evidence, while leaving `main` untouched. An idle cycle with no worthwhile proposals is also a valid outcome.
- [ ] Review the actual PR and its GitHub checks before merging it. Automated fixture tests do not replace this first authenticated, real-repository validation.
- [ ] Exercise **Pause** and **Start continuous**. Pause prevents new work; in-flight tasks may finish and publish. Use a task's **Cancel task** control when you want to stop that task.

## 6. Enable ongoing operation

- [ ] Leave the system running once you are satisfied with the first live results; increase throughput only as needed.
- [ ] Monitor blocked/failed tasks, model usage, host disk capacity, and the value of generated PRs.
- [ ] Optionally set `OCTOMUS_NOTIFICATION_WEBHOOK_URL` in the protected service environment and restart. Use an HTTPS destination you control; inspect **Attention notifications** in Configuration. Notices contain minimal repository/run/task identifiers and failure categories, not diagnostics or transcripts. Confirm delivery on the dedicated host; local receiver tests do not prove live delivery. Expect bounded retries and possible duplicate event IDs, not exactly-once delivery.
- [ ] Set up protected backups of the state directory, task workspaces, and the service account's selected runner session state (Codex home and/or OpenCode data directory) using the [backup procedure](deployment.md#backup-and-upgrade).
- [ ] Review retention settings, including the selected runner's separate transcript storage. Unresolved workspaces are intentionally preserved and can require deliberate cleanup.
- [ ] Continue reviewing and merging useful PRs yourself. Application upgrades and production deployments remain your responsibility; Octomus delivers PRs.

## Week-long hardening acceptance

This remains an owner-operated check on the dedicated VM and bot. Fixture and
local-provider contract tests do not establish live acceptance. Use the
conservative profile above, record UTC observations and private evidence references,
and keep billing/account evidence out of Git.

- Confirm lowered and raised admission limits govern already queued tasks across restart.
- Exercise one-shot completion, interrupted planning, active-task pause and stale-context supersession.
- Track blocked reasons and interventions per day; require no repeated stale retry loops.
- Record delivered tasks separately from open, merged and closed PR outcomes.
- Monitor application storage and separately managed runner transcripts; check eligible cleanup while paused.
- Require zero admission overruns, unverified publications, duplicate deliveries or orphaned owned workers.
- Record state response size/latency and daily admissions, alongside the owner’s PR acceptance decisions.
