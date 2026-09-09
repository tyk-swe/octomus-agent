# Dedicated-host deployment

Octomus is a single-operator, single-repository service for a dedicated Linux VM. Codex and repository verification commands run with the service account's full host permissions. Task clones separate mutable work; they are not a sandbox. Keep unrelated production credentials and services off this host.

## Install

Build on the target architecture or another compatible Linux host:

```bash
npm ci --prefix web
make package
```

Install `target/release/octomus-agent` (or the executable from a checksum-verified release archive) as `/usr/local/bin/octomus-agent`. The dashboard is embedded. Public release installation remains pending; see [distribution](distribution.md). Create an `octomus` OS account with a home directory at `/var/lib/octomus`, and make its home and target repository writable by that account. The binary must remain administrator-owned. The supplied unit expects `/srv/projects/octomus-agent` to exist. If using another target path (including the README example `/srv/projects/project`), change `ReadWritePaths` in a systemd override before starting.

Install `git`, `gh`, `curl` (used only for optional outbound notifications), and Codex for that account. Pin Codex CLI **0.153.4**, the tested protocol version. Authenticate Codex and GitHub as that user, configure Git credentials, and verify it can fetch the target checkout's origin without prompting. Install the target project's build/test toolchains as well. Ensure the unit's PATH includes their actual locations (including `/var/lib/octomus/.cargo/bin` when using rustup); a systemd service does not load the interactive shell's profile.

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

## Private access

The default listener is `127.0.0.1:4200`. Access it over an SSH tunnel:

```bash
ssh -N -L 4200:127.0.0.1:4200 your-host
```

Open `http://127.0.0.1:4200` locally. If using a reverse proxy instead, provide TLS and an operator-controlled access boundary. The bearer token is still required. No CORS access is enabled. `/healthz` exposes only liveness and version; all operational data and controls require authentication.

The dashboard starts paused. Configure the repository, explicit role routes, verification commands and host-appropriate limits, then run **Check connection** before enabling continuous work. Optional operator guidance (up to 4000 characters) steers planning, for example excluded modules or a current priority; it is injected into planning prompts as authoritative policy.

## Pull-request feedback

Grounding fetches reviews, check runs and comments for every owned open PR through
`gh api` (four additional calls per owned PR per cycle). PRs with requested
changes, failing checks or merge conflicts appear as feedback targets in the
dashboard's pull-request view and in planning prompts, so the next cycle can
propose work that addresses them. Octomus still never merges; it can only update
its own PR branches through the normal review and verification lifecycle.

## Notifications

An optional **Notification URL** in configuration receives one JSON POST per
operator-relevant event so an unattended host can reach you without an open
dashboard. Events: `task_published`, `task_blocked`, `cycle_failed`,
`audit_completed`, `audit_failed` and `service_paused` (the scheduler paused
itself after an error). Each body has the shape:

```json
{"event":"task_published","at":"2026-09-09T12:00:00Z","repository":"owner/project",
 "detail":{"task_id":"…","title":"…","branch":"octomus/…","pr_url":"https://github.com/…","pr_number":12,"error":null}}
```

Delivery uses the host's `curl` with a 10-second timeout, no redirects and one
retry after five seconds. It is best effort: a final failure is recorded as a
`notification` event in the activity log and never pauses or blocks work. Bodies
pass through the same redaction as dashboard JSON and never include the
operator token; the destination must be an `http(s)` URL without embedded
credentials. Point it at a receiver you control (ntfy, a chat relay, a small
webhook service) and allow that egress in the VM's network policy. Leave it empty
to disable.

## Controls and recovery

- **Pause:** prevents new work. Running discovery and tasks finish their current workflow, including publication. To stop a running task, use its **Cancel task** control. Publication already in progress is allowed to reconcile.
- **Run an audit:** requires paused operation and no active work. It spends planning admissions, records all decisions, creates no tasks and leaves any existing queue paused. Resume and cycle requests return a conflict while the audit is active. Restart marks interrupted audits without replaying them.
- **Run a cycle:** enables operation and makes the next planning cycle due. The current queue finishes first.
- **Retry task:** retries blocked or failed work within the configured retry budget. It retains the original target, model routes, verification contract and workspace. Updated time, storage, daily-session and repair limits can be applied to the retry. Resume operation if the service is paused.
- **Default branch moved:** unpublished new-branch work is reconciled automatically, up to the configured **Default-branch reconciliations** limit (three by default). Before execution the task simply adopts the new revision and its prompt says so; afterwards the workspace is rebased onto the moved revision and always receives a fresh review and verification before publication. Existing-PR work is never rebased: only its recorded default revision is refreshed, because its review compares against the merge base. Nothing is rebased once a branch has been pushed. A rebase conflict, or exceeding the limit, blocks the task with the workspace preserved at its previous HEAD; inspect it, then retry or cancel. Setting the limit to 0 restores blocking on any movement.
- **Source/branch conflict:** for changes to an existing owned branch outside the declared dependency chain, inspect the preserved workspace and changed remote state. Cancel the stale task and rediscover against the current source. Octomus never overwrites external changes.
- **Restart:** initialized in-flight tasks are queued for bounded automatic recovery. Partial workspace initialization or exhausted retry budgets are blocked. The last executor can resume, interrupted review work gets a fresh review, and a recorded publication checkpoint reconciles GitHub without another model call. Completed PR delivery remains recorded even if the PR has since been closed.
- **Model or authentication errors:** correct the host's account setup or explicit routes. There is no hidden fallback. Existing task route snapshots remain unchanged; cancel and rediscover if a task needs a different route.
- **Repair/verification limits:** unresolved work stays blocked and is never treated as clean. Review evidence and the workspace remain available for inspection.

Repository identity and branch policy cannot change while unresolved tasks exist. Configuration edits require the service to be paused with no active tasks or cycle. Operator API changes are the only application path for modifying policy; repository/model output is never parsed as configuration.

## Limits and retention

Defaults are visible in the dashboard and [configuration example](configuration.example.json): nine discovery agents, two simultaneous tasks, a 30-minute cycle interval, five accepted tasks per cycle, four repair rounds, two no-progress rounds, and 150 Codex turn admissions per UTC day. Reused repair turns count against the daily budget too.

The workspace budget is an **admission limit**, checked before launching model work. Active commands can grow beyond it; set host disk and process limits appropriate to the repository. The MVP does not estimate dollar spend or interrupt a provider's in-flight token billing. Use account-level spending limits as appropriate.

Published task workspaces and successful/idle discovery workspaces are removed after the configured retention period (14 days by default). Task identity, decisions, review records and publication associations remain in SQLite. Failed, interrupted or cancelled workspaces are preserved for inspection and may require deliberate operator cleanup after resolution. Activity events are capped at the configured count. Command output is drained and bounded; raw app-server tool arguments and output streams are not stored in the dashboard event log. Codex's own transcript storage is separate, under the service account's Codex home; configure its host retention separately.

Logs: `journalctl -u octomus-agent`. Task errors and session metadata also appear in the dashboard. Known credential patterns and values from token/secret/password/API-key environment variables are redacted from dashboard JSON and summaries. Keep secrets out of project documentation and task prompts; this redaction is not a secret-detection guarantee.

## Backup and upgrade

Pause and stop the service before a file-copy backup:

```bash
sudo systemctl stop octomus-agent
```

Back up `/var/lib/octomus/.octomus` in full, the service account's Codex home (for resumable threads), and the target repository. Protect backups as sensitive operator data. Keep `.octomus/state.db`, its WAL files if present, task workspaces, and Codex thread state together. Do not copy only the SQLite database while it is being written.

Replace the binary with a tested package and restart. State is persisted in SQLite with WAL and full synchronous writes. A file lock prevents two processes from operating on the same state directory. The initial MVP schema is created automatically; future incompatible schema changes will need explicit migrations.

Rotate the dashboard token by updating the environment file and restarting the service. Existing browser tokens stop working immediately after restart.

## CLI

```text
octomus-agent [--data-dir PATH] [--listen IP:PORT] [--assets PATH]
octomus-agent --print-config
octomus-agent --data-dir PATH --usage-report
octomus-agent --data-dir PATH --doctor
octomus-agent --data-dir PATH --doctor --audit
```

Environment equivalents: `OCTOMUS_DATA_DIR`, `OCTOMUS_LISTEN`, `OCTOMUS_ASSETS`, `OCTOMUS_TOKEN`. `--assets`/`OCTOMUS_ASSETS` explicitly replaces embedded serving with a directory containing `200.html`; the default needs no asset files. `--doctor --audit` (or **Check audit connection**) checks only planning prerequisites. The `--doctor` check takes the same state lock as the service; stop the service first, or use **Check connection** in the running dashboard.

`--doctor` reports installed/tested Codex versions and warns on a mismatch. Correct a mismatch before live commissioning. `--usage-report` opens existing SQLite state read-only, works alongside the service, and needs neither a token nor dashboard assets. It exports admission counts and saved cycle/task evidence, not provider billing. Historical usage without ledger entries is marked unattributed. See the [operator checklist](operations.md) and [cost methodology](cost.md). Admission records are retained with the state database; include their growth in disk monitoring and backups.

## HTTP interface additions

`POST /api/control/audit` runs one audit; authentication and JSON content type are
required. Conflicting active work returns 409; invalid configuration returns 400.
`POST /api/doctor?mode=audit` checks planning prerequisites; omitted mode retains
full execution checking. `GET /api/state` includes `audit_configured`,
`active_cycle_mode` (audit/execution/null) and cycle `mode`. During an audit,
status is `auditing` even though execution is paused. Older cycles load as
`execution`. Usage-report cycle rows also include `mode`; existing fields remain.

Successful/idle audit clones follow normal cycle retention. Cleanup runs during
enabled, idle scheduling; a service used only for paused audits still requires
operator monitoring of retained disk usage. No automatic replay occurs on resume.
