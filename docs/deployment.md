# Dedicated-host deployment

Octomus is a single-operator, single-repository service for a dedicated Linux VM. Codex and repository verification commands run with the service account's full host permissions. Task clones separate mutable work; they are not a sandbox. Keep unrelated production credentials and services off this host.

## Install

Build on the target architecture or another compatible Linux host:

```bash
npm ci --prefix web
make package
```

Install `target/release/octomus-agent` (or the executable from a checksum-verified release archive) as `/usr/local/bin/octomus-agent`. The dashboard is embedded. Public release installation remains pending; see [distribution](distribution.md). Create an `octomus` OS account with a home directory at `/var/lib/octomus`, and make its home and target repository writable by that account. The binary must remain administrator-owned. The supplied unit expects `/srv/projects/octomus-agent` to exist. If using another target path (including the README example `/srv/projects/project`), change `ReadWritePaths` in a systemd override before starting.

Install `git`, `gh` and the runners your routes select for that account: Codex CLI pinned to **0.153.4** and/or OpenCode **1.18.30**, the tested protocol versions. Authenticate the runners and GitHub as that user, configure Git credentials, and verify it can fetch the target checkout's origin without prompting. Install the target project's build/test toolchains as well. Ensure the unit's PATH includes their actual locations (including `/var/lib/octomus/.cargo/bin` when using rustup); a systemd service does not load the interactive shell's profile.

Create `/etc/octomus/agent.env`, readable only by the administrator and service account, with a fresh random token:

```text
OCTOMUS_TOKEN=<output of openssl rand -hex 32>
```

Do not use the literal placeholder. Use `chmod 600` and assign ownership appropriately. The service refuses tokens shorter than 32 characters. The token is an operator capability; it is not passed to child commands.

The unit makes system paths read-only and permits persistent writes only below
`/var/lib/octomus` and `/srv/projects/octomus-agent`. Codex state and tool caches
must live in the service home. Install tools as the administrator before starting;
NoNewPrivileges prevents verification commands from acquiring sudo privileges.
PrivateTmp gives workers temporary storage without sharing the host's `/tmp`.
These restrictions do not isolate agents from the service user's credentials.
See the [threat model](threat-model.md).

Create `/etc/octomus` and the checkout directory before starting. For a different
checkout, use `sudo systemctl edit octomus-agent` with:

```ini
[Service]
ReadWritePaths=
ReadWritePaths=/var/lib/octomus /srv/projects/project
```

Copy [deploy/octomus-agent.service](../deploy/octomus-agent.service) to `/etc/systemd/system/`, then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now octomus-agent
sudo systemctl status octomus-agent
```

The unit uses `KillMode=control-group` so crashes and restarts cannot leave old task processes running alongside recovered work. Do not change that to `process`. The service additionally kills each owned process group on normal cancellation or timeouts. Escaped processes on this intentionally unsandboxed host remain an operator responsibility.

## Optional attention webhook

Add `OCTOMUS_NOTIFICATION_WEBHOOK_URL=<operator-supplied HTTPS webhook URL>` to the
same protected environment file to opt in, then restart the service. Do not use the
placeholder or commit the actual URL. Unset the variable to disable notifications.
URL rotation cancels old pending deliveries rather than forwarding them to a new
receiver. No dashboard URL editor or inbound integration is provided.

The receiver accepts an `application/json` POST with `schema_version`, `event_id`,
`occurred_at`, `repository`, optional `cycle_id`/`run_id`/`task_id`, `category` and
`action`. Deduplicate by event ID if appropriate. Notices cover newly blocked/failed
tasks and error-paused service episodes; historical failures are not backfilled.
HTTPS is required except literal loopback HTTP for local receivers. Redirects and
implicit proxies are disabled. Requests time out after ten seconds and have at most
five attempts with 30/120/600/1,800-second backoffs. Delivery is rate-limited to one
request per second, expires after 24 hours, and bounds pending backlog to 1,000.
Retries after ambiguous acceptance may duplicate an event. Queue overflow, expiry,
invalid setup and delivery failures remain visible in Configuration.

The URL is not persisted in task/config snapshots, returned through observation APIs,
or passed in child-process environments. Same-user unsandboxed processes are still
inside the dedicated-host trust boundary. Never provision unrelated secrets here.

## Private access

The default listener is `127.0.0.1:4200`. Access it over an SSH tunnel:

```bash
ssh -N -L 4200:127.0.0.1:4200 your-host
```

Open `http://127.0.0.1:4200` locally. If using a reverse proxy instead, provide TLS and an operator-controlled access boundary. The bearer token is still required. No CORS access is enabled. `/healthz` exposes only liveness and version; all operational data and controls require authentication.

The dashboard starts paused. Configure the repository, explicit role routes, verification commands and host-appropriate limits, then run **Check connection** before enabling continuous work.

## Controls and recovery

- **Pause:** prevents new work. Running discovery and tasks finish their current workflow, including publication. To stop a running task, use its **Cancel task** control. Publication already in progress is allowed to reconcile.
- **Check clean baseline:** explicitly runs saved verification commands without model calls in a disposable clone of the identified remote default revision. Requires paused idle operation and confirmation of the saved configuration. Config changes, planning/execution starts and publication reconciliation conflict while it runs; Cancel stops the check. Results retain revision/configuration identity, bounded output and cleanup status separately from task verification. Restart records interruption without replay.
- **Run an audit:** requires paused operation and no active work. It spends planning admissions, records all decisions, creates no tasks and leaves any existing queue paused. Resume and cycle requests return a conflict while the audit is active. Restart marks interrupted audits without replaying them.
- **Run once:** requires paused operation with no active work. It captures the queued tasks, drains them, runs one discovery cycle, drains that cycle's accepted tasks, and returns to paused. Later retries are outside the captured batch. Independent tasks finish after failures; dependent tasks block. A failed initial drain prevents discovery. The batch and its phase survive restart; interrupted planning pauses without replay.
- **Start continuous:** enables queued execution and subsequent discovery cycles. **Pause** ends further one-shot dispatch as well as continuous scheduling; active workflows still finish.
- **Retry task:** available for eligible failures after prerequisite checks. It retains the task contract and workspace, but captures current timeouts, repair/no-progress limits and retry ceiling as a new attempt policy. Daily admissions and application storage always use saved live limits, including ordinary queued work and restart recovery. Start continuous operation or run once to execute a paused retry.
- **Stale context:** use **Supersede and rediscover**. The old task and workspace remain available; a durable request seeds the next authorized execution cycle. The new context receives fresh planning and review evidence. Replacements link back to the old task; an obsolete objective records its rejection instead. Dependents need their own rediscovery. **Reconcile publication** reuses the saved output and all publication checks without a model turn.
- **Restart:** initialized in-flight tasks are queued for bounded automatic recovery. Partial workspace initialization or exhausted retry budgets are blocked. The last executor can resume, interrupted review work gets a fresh review, and a recorded publication checkpoint reconciles GitHub without another model call. Completed PR delivery remains recorded even if the PR has since been closed.
- **Model or authentication errors:** correct the host's account setup or explicit routes. There is no hidden fallback. Existing task route snapshots remain unchanged. If a task needs a different route, pause, cancel it, save the new routes, then choose **Supersede and rediscover** on the cancelled task and start an execution cycle. This explicitly requests a fresh decision even when repository files are unchanged; any replacement uses the new routes and links back to the cancelled task. Cancellation alone does not request replacement work.
- **Repair/verification limits:** unresolved work stays blocked and is never treated as clean. Review evidence and the workspace remain available for inspection.

Repository identity and branch policy cannot change while unresolved tasks exist. Configuration edits require the service to be paused with no active tasks or cycle. Operator API changes are the only application path for modifying policy; repository/model output is never parsed as configuration.

## Limits and retention

Defaults are visible in the dashboard and [configuration example](configuration.example.json): nine discovery agents, two simultaneous tasks, a 30-minute cycle interval, five accepted tasks per cycle, five owned open PRs, four repair rounds, two no-progress rounds, and 150 agent turn admissions per UTC day. Reused repair turns count against the daily budget too.

Planning checks the complete pass requirement before starting (13 admissions with
nine discovery agents). Manual audit/Run once requests are refused without spending
admissions when short. Continuous operation waits without repeated failed cycles;
Run once pauses if its initial queue drain leaves insufficient planning allowance.
Per-role atomic limits still apply, and the preflight is not an execution-budget reservation.

The owned-open-PR ceiling is live policy, including for queued work. Complete remote
observations plus durable admitted-delivery reservations govern new-PR dispatch;
unknown state is not zero. Existing-PR maintenance and preserved publication replay
remain eligible. Capacity returns after observed closure/merge. A lowered limit does
not close existing PRs or interrupt already admitted work, which can finish above the
new ceiling. External PR changes can also alter backlog after observation.

The workspace budget is an **admission limit**, checked before launching model work. Active commands can grow beyond it; set host disk and process limits appropriate to the repository. The MVP does not estimate dollar spend or interrupt a provider's in-flight token billing. Use account-level spending limits as appropriate.

Housekeeping runs every 15 minutes, including while paused or configured only for audits. Published task workspaces and completed cycle directories follow the configured retention period (14 days by default). Successful planning-role clones are disposed after structured results, session evidence and unchanged-source checks are persisted. Failed or modified clones and unresolved task workspaces remain retained. **Archive task/cycle** resolves retained work and makes its workspace eligible for retention; **Discard workspace** explicitly removes an archived workspace. Database evidence, identities and lineage remain available. Active workspaces and symlink paths are excluded from cleanup. Activity events are capped at the configured count. Command output is drained and bounded; raw app-server tool arguments and output streams are not stored in the dashboard event log. Runner transcript storage is separate: Codex uses the service account's Codex home, and OpenCode uses its data directory. Configure host retention for the selected runners separately.

Logs: `journalctl -u octomus-agent`. Task errors and session metadata also appear in the dashboard. Known credential patterns and values from token/secret/password/API-key environment variables are redacted from dashboard JSON and summaries. Keep secrets out of project documentation and task prompts; this redaction is not a secret-detection guarantee.

## Backup and upgrade

Pause and stop the service before a file-copy backup:

```bash
sudo systemctl stop octomus-agent
```

Back up `/var/lib/octomus/.octomus` in full, the service account's selected runner session stores (Codex home and/or OpenCode data directory), and the target repository. Protect backups as sensitive operator data. Keep `.octomus/state.db`, its WAL files if present, task workspaces, and runner session state together. OpenCode sessions normally live under the service user's XDG data directory; retain that directory when using OpenCode. Do not copy only the SQLite database while it is being written.

Replace the binary with a tested package and restart. State is persisted in SQLite with WAL and full synchronous writes. A file lock prevents two processes from operating on the same state directory. The initial MVP schema is created automatically; future incompatible schema changes will need explicit migrations.

Rotate the dashboard token by updating the environment file and restarting the service. Existing browser tokens stop working immediately after restart.

## CLI

```text
octomus-agent [--data-dir PATH] [--listen IP:PORT] [--assets PATH]
octomus-agent --print-config
octomus-agent --data-dir PATH --usage-report
octomus-agent --data-dir PATH --doctor
octomus-agent --data-dir PATH --doctor --audit
octomus-agent --data-dir PATH --export-run CYCLE_ID
```

Environment equivalents: `OCTOMUS_DATA_DIR`, `OCTOMUS_LISTEN`, `OCTOMUS_ASSETS`, `OCTOMUS_TOKEN`. `--assets`/`OCTOMUS_ASSETS` explicitly replaces embedded serving with a directory containing `200.html`; the default needs no asset files. `--doctor --audit` (or **Check audit connection**) checks only planning prerequisites. The `--doctor` check takes the same state lock as the service; stop the service first, or use **Check connection** in the running dashboard.

`--doctor` reports installed/tested Codex versions and warns on a mismatch. Correct a mismatch before live commissioning. `--usage-report` opens existing SQLite state read-only, works alongside the service, and needs neither a token nor dashboard assets. It exports admission counts and saved cycle/task evidence, not provider billing. Historical usage without ledger entries is marked unattributed. See the [operator checklist](operations.md) and [cost methodology](cost.md). Admission records are retained with the state database; include their growth in disk monitoring and backups. `--export-run` likewise opens the database read-only, without the service lock, and prints the recorded `RunEvidenceV1` for one saved cycle; see [run evidence](launch/run-evidence.md).

## HTTP API

`POST /api/control/audit` runs one audit; authentication and JSON content type are
required. Conflicting active work returns 409; invalid configuration returns 400.
`POST /api/doctor?mode=audit` checks planning prerequisites; omitted mode retains
full execution checking. `GET /api/state` includes `audit_configured`,
`active_cycle_mode` (audit/execution/null) and cycle `mode`. During an audit,
status is `auditing` even though execution is paused. Older cycles load as
`execution`. Usage-report cycle rows also include `mode`; existing fields remain.

Successful audit role clones are disposable; retained failures follow the archive/discard lifecycle. No automatic audit replay occurs on resume.

`POST /api/control/cycle` means **Run once**; it does not enable continuous
operation. `POST /api/control/resume` selects continuous operation. Control JSON
contains `mode` (`paused`, `run_once`, `continuous`) and the persisted batch phase;
`paused` remains a derived compatibility field. Old boolean control records load
as paused or continuous.

`GET /api/state` contains authoritative task counts, merged PR count, bounded task,
cycle, PR and event summaries, attention examples, live admission limits, and the
latest storage sample. It does not include full planning evidence. Use
`GET /api/tasks/{id}`, `GET /api/cycles/{id}`, and
`GET /api/proposals/{cycle}/{id}` for details. `GET /api/cycles/{id}/evidence` returns
the recorded `RunEvidenceV1` for one cycle. Task detail includes
`blocked_reason`, `allowed_actions`, `effective_attempt_policy`, and current
`operating_policy`. Existing `config` remains the original task snapshot.

`GET /api/tasks`, `/api/cycles`, `/api/proposals`, and `/api/prs` support `before`
cursors, `limit` (default 50, maximum 100), `status`, and `q`. Proposals also
support `cycle`. Responses contain `items`, `next_cursor`, and decision counts
where applicable. Task filters include `active` and `attention`. Pagination uses
insertion order, so task status changes do not reorder history pages.

Task actions add `supersede`, `reconcile`, `archive`, and `discard`; cycle actions
support `archive` and `discard`. Ineligible actions return 409. Discard requires
archiving first. Publication reconciliation waits for active tasks to finish.

PR observations refresh after delivery and every five minutes, including while
paused. Open, merged and closed-unmerged outcomes and external head movement are
separate from delivered task status. Migrated PR caches have no observation time
until a fresh read. Runner transcript storage is reported separately as unavailable
when it is not measured; it is never counted as zero or automatically deleted.

Optional `runner_storage_paths` entries (`codex`, `opencode`) let the operator
supply absolute paths for separate size measurement, also available below the
operating limits in Configuration. Unconfigured or unreadable roots are shown
as unavailable; configured readable roots report their actual size. Only file
metadata is inspected. These roots are never cleaned by Octomus. Application admission still measures
the entire data directory, including any runner storage placed inside it. Use
disjoint roots when interpreting the separate runner total.
