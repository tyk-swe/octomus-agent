# Deployment

Octomus is a single-operator, single-repository service. The recommended deployment is
Docker Compose on a Linux host you already run: every agent turn and verification command
runs in its own [sandbox](sandbox.md), and the control plane holding your GitHub token never
runs repository code. An unsandboxed deployment on a
[dedicated VM](#dedicated-vm-without-a-sandbox) is also available.

## Docker deployment

You need:
- Docker Engine 28 or later with the Compose plugin, on x86_64 or aarch64;
- a GitHub fine-grained token for the one repository, with Contents and Pull requests
  read/write, and Checks and Commit statuses read;
- Codex or OpenCode provider access.

Nothing else is installed on the host.

```bash
git clone https://github.com/tyk-swe/octomus-agent.git
cd octomus-agent/deploy/docker
./setup.sh
```

`setup.sh` does the following:
- checks Docker;
- writes `.env` from [env.example](../deploy/docker/env.example), including the Docker
  socket's group for the broker;
- asks for `OWNER/REPOSITORY` and the GitHub token;
- generates the operator token and prints it once, even when a later step fails (a later
  run says where it is: `sudo cat deploy/docker/secrets/operator_token`);
- stores both secrets as files that only the control plane's user can read;
- builds the control-plane and sandbox images;
- runs `docker compose up -d`.

Repository selection follows Docker Compose's effective environment, including an
exported `OCTOMUS_GITHUB_REPO`, quoted `.env` values and interpolation. The setup
prompt names the repository that the containers will use. When setup asks for a new
value, it saves that value and exports it for the rest of the invocation, including
when the calling shell had exported an empty value.

On first start the control plane clones the repository into its data volume. That clone
is the trusted checkout, and the dashboard cannot repoint it.

Then sign in a runner. The login is stored in the `octomus-runner` volume, which only
runner sandboxes mount:

```bash
docker compose run --rm login codex login --device-auth
# or
docker compose run --rm login opencode auth login
```

A login runs like a runner sandbox: it reaches out only through the egress gateway, under
the runner allowlist. Codex's sign-in hosts are allowed by default; for OpenCode, first add
`models.opencode.ai` and your provider's sign-in host to `OCTOMUS_EGRESS_MODEL_HOSTS` in
`.env` and run `docker compose up -d`. See [signing in a runner](sandbox.md#signing-in-a-runner).

Open the dashboard through an [SSH tunnel](#private-access) and run **Check connection**. It
proves the sandbox from inside one before any work starts; see [the self-test](sandbox.md#prove-it-the-self-test).

### Host settings

Everything security-relevant is set in `.env`, not in the dashboard: the pinned repository,
sandbox limits, the sandbox image and runtime, and the egress allowlists. The dashboard
shows them read-only. [env.example](../deploy/docker/env.example) documents each setting,
and [sandbox](sandbox.md) explains them. After changing `.env`, run `docker compose up -d`.

Add your project's build and test tools to the sandbox image, not to the host: see
[extending the sandbox image](sandbox.md#extend-the-sandbox-image). Verification commands run
in that image.

For a provider that uses a private certificate authority, see the
[host-owned runner CA bundle](sandbox.md#private-provider-certificates). This changes
runner certificate trust while keeping the egress host and address checks in force.
Automatic maintenance merging requires separate trusted credentials and server-side
rules; follow [maintenance merge setup](maintenance-merging.md) before enabling that mode.

### Operating the stack

| Task | Command |
| --- | --- |
| Status | `docker compose ps` |
| Logs | `docker compose logs -f octomus` (egress decisions: `docker compose logs egress`); Docker keeps at most five 10 MB files per service |
| Stop, keeping state | `docker compose stop` |
| Restart after an `.env` change | `docker compose up -d` |
| Upgrade | `git pull`, `docker compose build octomus sandbox-image`, rebuild any [derived sandbox image](sandbox.md#extend-the-sandbox-image), then `docker compose up -d` |

The control plane stops gracefully within its 75-second grace period, which covers its
wait for the broker to confirm each running sandbox's removal. Stopping it closes
every sandbox's stream, so the broker removes every running sandbox. The broker also
removes any sandbox left from a previous run when it starts. It only ever touches
containers carrying its own `octomus.sandbox.instance` label.

State lives in named volumes:
- `octomus-data`: the database, workspaces and the trusted checkout;
- `octomus-runner`: runner logins and session transcripts;
- `octomus-tools`: the broker's helper binary and optional public CA bundle, recreated on start.

Keep the secret files under `secrets/` with the deployment. `setup.sh` gives them to the
container user (uid 10001) at mode 0600. The control plane reads them only when it starts,
and `docker compose up -d` does not notice a changed secret file. To rotate one, install
the new value with the same owner and mode, then recreate the control plane:

```bash
sudo install -o 10001 -g 10001 -m 0600 /path/to/new-token secrets/github_token
docker compose up -d --force-recreate octomus
```

After rotating `secrets/operator_token`, sign in to the dashboard with the new token.

## Dedicated VM without a sandbox

This deployment runs runner sessions and verification commands with the service account's
full host permissions (`--sandbox off`). Task clones separate mutable work but are not a
sandbox, so use a dedicated Ubuntu 24.04 VM that runs nothing else, never your workstation,
and keep unrelated production credentials and services off it. The dashboard shows a
permanent **Unsandboxed** warning.

### Install

As the administrator, install Git, gh and the runners your routes select, pinned to the
tested protocol versions: Codex CLI **0.153.4** (through npm, with Node 22 from
[NodeSource](https://github.com/nodesource/distributions)) and/or the OpenCode
[1.18.30 release](https://github.com/anomalyco/opencode/releases/tag/v1.18.30) for your
platform. Skip the Node and Codex lines for an OpenCode-only installation.

```bash
sudo apt-get update
sudo apt-get install -y git gh curl ca-certificates openssl
curl -fsSL https://deb.nodesource.com/setup_22.x -o /tmp/octomus-node22.sh
sudo bash /tmp/octomus-node22.sh
sudo apt-get install -y nodejs
sudo npm install -g @openai/codex@0.153.4
```

Install the release with the checksum-verifying installer:

```bash
curl -fsSL https://raw.githubusercontent.com/tyk-swe/octomus-agent/main/install.sh | sh
```

It verifies the archive against the release's SHA-256 checksums and installs to
`/usr/local/bin`. To select a version, download the script and run `sh install.sh v0.2.0`,
or set `OCTOMUS_VERSION=v0.2.0` for the piped `sh`; `INSTALL_DIR` selects another writable
absolute destination. Checksums detect corruption; they are not independent signatures
against a compromised release account. Release archives for x86_64 and aarch64 are on the
[releases page](https://github.com/tyk-swe/octomus-agent/releases).

To build from source instead, on the target architecture or another compatible Linux host,
you need Go (per `go.mod`), Node 22.12+ and npm for the embedded dashboard:

```bash
git clone https://github.com/tyk-swe/octomus-agent.git
cd octomus-agent
npm ci --prefix web
make build
sudo install -m 755 bin/octomus-agent /usr/local/bin/octomus-agent
```

The executable is statically linked and embeds the dashboard, so it needs no toolchain at
runtime, and it must remain administrator-owned. Use a fresh data directory or state from
v0.1.0 or later; the service upgrades older state at startup after writing a backup (see
[Backup and upgrade](#backup-and-upgrade)).

Create the service account without sudo access, sign in the runners and GitHub as that
user, and clone the target repository where the supplied unit expects it:

```bash
sudo useradd --create-home --home-dir /var/lib/octomus --shell /bin/bash octomus
sudo install -d -o octomus -g octomus /srv/projects
sudo -iu octomus
codex login            # or, for OpenCode routes: opencode auth login
gh auth login
gh auth setup-git
git clone https://github.com/OWNER/REPOSITORY.git /srv/projects/octomus-agent
```

Use a dedicated identity with repository-restricted authentication, not an unrelated
personal credential, and verify the account can fetch the checkout's origin without
prompting. Configure OpenCode's providers in that user's OpenCode configuration; Octomus
reads provider settings and credentials but never manages logins. For another checkout
path, change `ReadWritePaths` in a systemd override before starting (below).

Install the target project's build and test tools as well: verification commands use them.
Add their locations to the unit's PATH with a systemd override; a service does not load the
interactive shell's profile.

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

Copy [deploy/octomus-agent.service](../deploy/octomus-agent.service) to `/etc/systemd/system/`.
It starts the service with `--sandbox off`, the explicit choice this deployment makes. Then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now octomus-agent
sudo systemctl status octomus-agent
```

The unit uses `KillMode=control-group` so crashes and restarts cannot leave old task processes running alongside recovered work. Do not change that to `process`. The service additionally kills each owned process group on normal cancellation or timeouts. Escaped processes on this intentionally unsandboxed host remain an operator responsibility.

## Optional attention webhook

Add `OCTOMUS_NOTIFICATION_WEBHOOK_URL=<operator-supplied HTTPS webhook URL>` to the
same protected environment file to opt in, then restart the service. In the Docker
deployment, write the URL to a file under `secrets/` and point
`OCTOMUS_NOTIFICATION_WEBHOOK_URL_FILE` at it in a compose override for the `octomus`
service. Do not use the
placeholder or commit the actual URL. Unset the variable to disable notifications.
URL rotation cancels old pending deliveries rather than forwarding them to a new
receiver. No dashboard URL editor or inbound integration is provided.

Notices cover newly blocked/failed or published tasks, failed planning cycles,
successful completed audits (including idle audits), and error-paused service
episodes, including those discovered during restart recovery. Historical events
are not backfilled during setup or upgrades; resaving or archiving a terminal
record does not create another notice.
The receiver accepts an `application/json` POST whose
body always carries every key below:

| Key | Value |
| --- | --- |
| `schema_version` | `1` |
| `event_id` | 32 lowercase hexadecimal characters, unique per notice and repeated on its retries; deduplicate on it |
| `occurred_at` | UTC RFC 3339 time with milliseconds, such as `2026-09-26T12:00:00.123Z` |
| `repository` | The recorded GitHub `OWNER/REPOSITORY`, or an empty string when none is recorded |
| `cycle_id`, `run_id`, `task_id` | A string or `null`, never absent |
| `category` | A task's blocked reason, `unknown`, `task_published`, `cycle_failed`, `audit_completed`, or `service_error_paused` |
| `action` | `inspect_task`, `inspect_cycle`, or `inspect_service` |

A task notice (`inspect_task`) names the task, its cycle and its **Run once** batch
(`run_id` is `null` outside one). Its category is the task's blocked reason
(`budget_exhausted`, `storage_limit`, `stale_base`, `remote_conflict`,
`publication_uncertain`, `runner_unavailable`, `invalid_review`, `verification_failed`,
`dependency_blocked`, `invalid_plan`, `workspace_invalid`, `retry_limit` or `timeout`),
or `unknown` when the task has no known reason; a published task uses `task_published`
with the same identifiers and action. Cycle notices (`cycle_failed` or
`audit_completed`, `inspect_cycle`) name the recorded cycle and its optional Run
once batch, with `task_id: null`. A failed audit is a `cycle_failed` notice, not
an audit-completed notice. An interrupted/cancelled cycle is not a failure notice.
Cycle repository attribution comes from its saved snapshot, not the configuration
at delivery time. A service notice
(`service_error_paused`, `inspect_service`) has a `null` `task_id`. Its `cycle_id` and
`run_id` would name a Run once batch recorded with the pause, but pausing clears the
batch, so expect `null` while still accepting strings there. For example, a task
notice and a service notice:

```json
{"schema_version":1,"event_id":"9f1c2a7be04d4c3a8e6f5b2d1c0a9e8f","occurred_at":"2026-09-26T12:00:00.123Z","repository":"OWNER/REPOSITORY","cycle_id":"3f2b8c1e-5d4a-4f6b-9c7e-1a2b3c4d5e6f","run_id":null,"task_id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","category":"verification_failed","action":"inspect_task"}
```

```json
{"schema_version":1,"event_id":"0a1b2c3d4e5f60718293a4b5c6d7e8f9","occurred_at":"2026-09-26T12:05:00.456Z","repository":"OWNER/REPOSITORY","cycle_id":null,"run_id":null,"task_id":null,"category":"service_error_paused","action":"inspect_service"}
```

HTTPS is required except literal loopback HTTP for local receivers. Redirects and
implicit proxies are disabled. Requests time out after ten seconds and have at most
five attempts with 30/120/600/1,800-second backoffs. Delivery waits one second after each
attempt completes before checking for another notification, so slow database or
HTTP work cannot cause a catch-up burst. Events expire after 24 hours, and the
pending backlog is bounded to 1,000.
Retries after ambiguous acceptance may duplicate an event. Queue overflow, expiry,
invalid setup and delivery failures remain visible in Configuration.

The URL is not persisted in task/config snapshots, returned through observation APIs,
or passed in child-process environments. Sandboxes never see the control plane's
environment; in unsandboxed mode, same-user processes are still inside the
dedicated-host trust boundary. Never provision unrelated secrets here.

## Private access

The default listener is `127.0.0.1:4200`; the compose file publishes the control plane on
`127.0.0.1:${OCTOMUS_PORT}` only. Access it over an SSH tunnel:

```bash
ssh -N -L 4200:127.0.0.1:4200 your-host
```

Open `http://127.0.0.1:4200` locally. If using a reverse proxy instead, provide TLS and an operator-controlled access boundary. The bearer token is still required. No CORS access is enabled. `/healthz` exposes only liveness and version; all operational data and controls require authentication.

The dashboard starts paused. Configure the repository, explicit role routes, verification commands and host-appropriate limits, then run **Check connection** before enabling continuous work.

## Controls and recovery

- **Pause:** prevents new work. Running discovery and tasks finish their current workflow, including publication. To stop a running task, use its **Cancel task** control. Publication already in progress is allowed to reconcile.
- **Check clean baseline:** explicitly runs saved verification commands without model calls in a disposable clone of the identified remote default revision. Requires paused idle operation and confirmation that pins the saved configuration's canonical revision; a superseded revision conflicts. Config changes, planning/execution starts and publication reconciliation conflict while it runs; Cancel stops the check. Results retain revision/configuration identity, bounded output and cleanup status separately from task verification. Restart records interruption without replay.
- **Run an audit:** requires paused operation and no active work. It spends planning admissions, records all decisions, creates no tasks and leaves any existing queue paused. Resume and cycle requests return a conflict while the audit is active. Restart marks interrupted audits without replaying them.
- **Run once:** requires paused operation with no active work. It captures the queued tasks, drains them, runs one discovery cycle, drains that cycle's accepted tasks, and returns to paused. Later retries are outside the captured batch. Independent tasks finish after failures; dependent tasks block. A failed initial drain prevents discovery. The batch and its phase survive restart; interrupted planning pauses without replay.
- **Start continuous:** enables queued execution and subsequent discovery cycles. **Pause** ends further one-shot dispatch as well as continuous scheduling; active workflows still finish.
- **Retry task:** available for eligible failures after prerequisite checks. It retains the task contract and workspace, but captures current timeouts, repair/no-progress limits and retry ceiling as a new attempt policy. Daily admissions and application storage always use saved live limits, including ordinary queued work and restart recovery. Start continuous operation or run once to execute a paused retry.
- **Stale context:** use **Supersede and rediscover**. The old task and workspace remain available; a durable request seeds the next authorized execution cycle. The new context receives fresh planning and review evidence. Replacements link back to the old task; an obsolete objective records its rejection instead. Archiving the old task before then withdraws its request. Dependents need their own rediscovery. **Reconcile publication** reuses the saved output and all publication checks without a model turn.
- **Restart:** after a crash or a graceful stop (SIGINT, SIGTERM, or a terminal hangup unless it is ignored, as under `nohup`), initialized in-flight tasks are queued for bounded automatic recovery. A graceful stop blocks work interrupted before its workspace was initialized; after a crash, restart blocks it. Restart also blocks tasks whose retry budget is exhausted. The last executor can resume, interrupted review work gets a fresh review, and a recorded publication checkpoint reconciles GitHub without another model call; once the retry budget is exhausted, such a task is blocked as `publication_uncertain` for **Reconcile publication** instead. Completed PR delivery remains recorded even if the PR has since been closed.
- **Model or authentication errors:** correct the host's account setup or explicit routes. There is no hidden fallback. Existing task route snapshots remain unchanged. If a task needs a different route, pause, cancel it, save the new routes, then choose **Supersede and rediscover** on the cancelled task and start an execution cycle. This explicitly requests a fresh decision even when repository files are unchanged; any replacement uses the new routes and links back to the cancelled task. Cancellation alone does not request replacement work.
- **Repair/verification limits:** unresolved work stays blocked and is never treated as clean. Review evidence and the workspace remain available for inspection.

Repository identity and branch policy cannot change while unresolved tasks exist. Configuration edits require the service to be paused with no active tasks, cycle, baseline check or automatic merge check. The dashboard disables editing, Run once and Run an audit while an automatic merge check is active. Each write pins the canonical configuration revision it was made from and replaces only the supplied top-level fields; writes against a superseded revision conflict. Values served only as redacted or shortened display previews are never written back, so hidden settings survive unrelated edits unchanged. Operator API changes are the only application path for modifying policy; repository/model output is never parsed as configuration.

## Limits and retention

Defaults are visible in the dashboard and [configuration example](configuration.example.json): nine discovery agents, two simultaneous tasks, a 30-minute cycle interval, five accepted tasks per cycle, five owned open PRs, four repair rounds, two no-progress rounds, and 150 agent turn admissions per UTC day. Reused repair turns count against the daily budget too. [Configuration](configuration.md#field-reference) lists every field's accepted range.

Planning checks the complete pass requirement before starting (13 admissions with
nine discovery agents). Manual audit/Run once requests are refused without spending
admissions when short. Continuous operation waits without repeated failed cycles;
Run once pauses if its initial queue drain leaves insufficient planning allowance.
Per-role atomic limits still apply, and the preflight is not an execution-budget reservation.

The owned-open-PR ceiling is live policy, including for queued work. Complete remote
observations plus durable admitted-delivery reservations govern new-PR dispatch;
unknown state is not zero. Existing-PR maintenance and preserved publication replay
remain eligible. Capacity returns after observed closure/merge, or once remote
inspection settles a cancelled task's admitted branch. A lowered limit does not close
existing PRs or interrupt already admitted work, which can finish above the new
ceiling. External PR changes can also alter backlog after observation.

The workspace budget is an **admission limit**, checked before launching model work. Active commands can grow beyond it; set host disk and process limits appropriate to the repository. The MVP does not estimate dollar spend or interrupt a provider's in-flight token billing. Use account-level spending limits as appropriate.

Retention housekeeping runs every 15 minutes, including while paused or configured only for audits. Published task workspaces and completed cycle directories follow the configured retention period (14 days by default). Successful planning-role clones are disposed after structured results, session evidence and unchanged-source checks are persisted. Failed or modified clones and unresolved task workspaces remain retained. **Archive task/cycle** resolves retained work and makes its workspace eligible for retention; **Discard workspace** explicitly removes an archived workspace. Each happens once; repeating it is a conflict. Database evidence, identities and lineage remain available. A failed cleanup is retried on later passes; a new or changed failure is recorded as an activity event at once, an unchanged one at most once a day per service process. Active workspaces and symlink paths are excluded from cleanup. Activity events are capped at the configured count. Command output is drained and bounded; raw runner tool arguments and output streams are not stored in the dashboard event log. Runner transcript storage is separate: in the Docker deployment both runners keep sessions in the `octomus-runner` volume; unsandboxed, Codex uses the service account's Codex home and OpenCode its data directory. Configure retention for the selected runners separately.

Logs: `docker compose logs octomus` in the Docker deployment, `journalctl -u octomus-agent` under systemd. Task errors and session metadata also appear in the dashboard. Known credential patterns, the webhook URL and values from token/secret/password/API-key environment variables are redacted from dashboard JSON and summaries. Keep secrets out of project documentation and task prompts; this redaction is not a secret-detection guarantee.

## Backup and upgrade

Pause and stop the service before a file-copy backup. In the Docker deployment, stop the
stack and archive its volumes and secrets together:

```bash
docker compose stop
docker run --rm --network none -v octomus-data:/backup/data:ro -v octomus-runner:/backup/runner:ro \
  -v "$PWD":/out debian:trixie-slim sh -c 'umask 077 && tar czf /out/octomus-backup.tgz -C /backup data runner'
sudo sh -c 'umask 077 && tar czf octomus-secrets.tgz secrets .env'
docker compose start
```

Both archives hold credentials: the runner logins, the GitHub token and the operator token.
Only root can read them; move them off the host, or into a directory only you can read, and
protect them as sensitive operator data.

Under systemd:

```bash
sudo systemctl stop octomus-agent
```

Back up `/var/lib/octomus/.octomus` in full, the service account's selected runner session stores (Codex home and/or OpenCode data directory), and the target repository. Protect backups as sensitive operator data. Keep `.octomus/state.db`, its WAL files if present, task workspaces, and runner session state together. OpenCode sessions normally live under the service user's XDG data directory; retain that directory when using OpenCode. Do not copy only the SQLite database while it is being written.

### Upgrading

Take the backup above first. Then install the tested release — the new binary under systemd, or `OCTOMUS_IMAGE` and `OCTOMUS_SANDBOX_IMAGE` in `deploy/docker/.env` for the Docker deployment — and start the service.

When the release's schema is newer, the first start — the service or `--doctor` — checks the saved version before changing anything. It then writes a consistent copy `state.db.v<old>-backup-<UTC yyyymmddThhmmssZ>` beside `state.db` through the SQLite backup API, integrity-checks it and stores it mode 0600, never overwriting an earlier backup. Only then does it apply each migration in its own transaction. The start logs `Upgraded state database from schema version …` (`docker compose logs octomus`, `journalctl -u octomus-agent`) and records an `upgrade` activity event.

A failed migration rolls back alone: the database stays at the version that step started from, and the error names the backup it keeps. Starting again retries with a fresh backup. If the backup cannot be written (space, permissions), nothing changes. A database newer than the binary is refused untouched, as are pre-release databases older than version 7 — those need a fresh data directory. `--usage-report` and `--export-run` from a newer binary refuse an older database until the service has upgraded it. Restarting the same release changes nothing.

The backup holds the same sensitive data as `state.db` and counts toward application storage. It stays until you remove it, so remove it once the upgraded release has run satisfactorily.

State uses SQLite with WAL and full synchronous writes; a file lock prevents two processes from operating on the same directory.

### Rolling back

There are no down-migrations, and an older release refuses a newer schema, so a rollback restores the pre-upgrade backup. Stop the service. Move `state.db`, `state.db-wal` and `state.db-shm` aside together — leaving a newer WAL beside the restored file would corrupt it. Copy the backup to `state.db` with the service user's ownership and mode 0600, reinstall the older release and start.

In the Docker deployment, run the file moves in a throwaway container on the `octomus-data` volume, mounted at `/var/lib/octomus/data` inside the service container; the service runs as uid 10001:

```bash
docker compose stop
docker run --rm --network none -v octomus-data:/data debian:trixie-slim sh -c '
  cd /data && install -d -o 10001 -g 10001 -m 700 .rollback && \
  for f in state.db state.db-wal state.db-shm; do [ ! -e "$f" ] || mv "$f" .rollback/; done && \
  install -o 10001 -g 10001 -m 600 state.db.v7-backup-<timestamp> state.db'
```

Then point `OCTOMUS_IMAGE` and `OCTOMUS_SANDBOX_IMAGE` in `deploy/docker/.env` back at the older release, pull and `docker compose up -d`.

Under systemd, state lives in `/var/lib/octomus/.octomus`, owned by the `octomus` user:

```bash
sudo systemctl stop octomus-agent
sudo -u octomus sh -c '
  cd /var/lib/octomus/.octomus && mkdir -m 700 -p .rollback && \
  for f in state.db state.db-wal state.db-shm; do [ ! -e "$f" ] || mv "$f" .rollback/; done && \
  install -m 600 state.db.v7-backup-<timestamp> state.db'
sh install.sh vX.Y.Z
sudo systemctl start octomus-agent
```

Anything recorded after the upgrade is lost from state. PRs it opened stay on GitHub, and task workspaces it created may remain on disk.

Rotate the dashboard token by updating the environment file and restarting the service. Existing browser tokens stop working immediately after restart.

## HTTP API

The dashboard uses this API; scripts can call it with the same token. The route
table in `internal/httpapi/routes.go` is authoritative; `Router` in
`internal/httpapi/httpapi.go` only assembles the transport and serves health/assets.

- Every route below is under `/api` and needs `Authorization: Bearer <token>`.
  `/healthz` needs no token and reports only liveness and version.
- Every `POST` and `PUT` must send `Content-Type: application/json`, even without a
  body (for example `POST /api/control/pause`); otherwise the answer is
  `415 {"error":"Use application/json"}`.
  Send exactly one Content-Type field. Its media type is case-insensitive and
  parsed with Go's `mime.ParseMediaType`, including valid parameters and a trailing
  semicolon (an empty parameter allowed by HTTP). Multiple fields, other media
  types and parameter parsing errors are rejected before the handler runs.
- Checks run in this order: an unknown path answers `404 {"error":"Unknown API route"}`
  before authentication; a missing or wrong token answers `401` after the
  failed-authentication backoff; then the content type is checked; a known path with
  the wrong method answers `405` with an `Allow` header and an empty body.
- Handler errors are JSON `{"error":"…"}`: `404` for an unknown task, cycle, proposal,
  baseline check (when read) or action name, `409` for a conflict with current state
  (including a superseded configuration revision, and cancelling a baseline check that
  does not exist, already finished or is no longer running), `500` for storage or
  encoding failures, and `400` otherwise, such as a configuration value out of range.
  Request rejections are `text/plain`: `400` for malformed JSON or a malformed query
  value, `422` when the body or a `config` patch fails strict decoding (a wrong type, an
  unknown field or an unknown name), and `413` for a body over 256 KiB.
- Every `/api` JSON response is redacted. Request headers must arrive within 10
  seconds, and idle keep-alive connections close after two minutes.

| Method | Path | Purpose | Success |
| --- | --- | --- | --- |
| `GET` | `/api/state` | Dashboard snapshot: mode, work, capacity, storage, notifications, baseline, sandbox posture | 200 |
| `GET` | `/api/tasks` | Task history page | 200 |
| `GET` | `/api/tasks/{id}` | One task with `allowed_actions` and its attempt and live limits | 200 |
| `POST` | `/api/tasks/{id}/{action}` | `cancel`, `retry`, `supersede`, `archive`, `discard` or `reconcile` | 200 |
| `GET` | `/api/cycles` | Cycle history page | 200 |
| `GET` | `/api/cycles/{id}` | One saved cycle | 200 |
| `GET` | `/api/cycles/{id}/evidence` | [`RunEvidenceV1`](run-evidence.md) | 200 |
| `POST` | `/api/cycles/{id}/{action}` | `archive` or `discard` | 200 |
| `GET` | `/api/proposals` | Proposal page without prompts or evidence, with per-decision counts | 200 |
| `GET` | `/api/proposals/{cycle}/{id}` | One proposal | 200 |
| `GET` | `/api/prs` | Observed pull-request page | 200 |
| `GET` | `/api/config` | Display-safe configuration, its `revision` and `transformed_fields` | 200 |
| `PUT` | `/api/config` | `{"expected_revision":"…","config":{…}}` replaces the supplied top-level fields | 200 |
| `POST` | `/api/baseline-checks` | `{"expected_revision":"…"}` starts a clean-baseline check | 202 |
| `GET` | `/api/baseline-checks/latest` | Latest baseline check | 200 |
| `GET` | `/api/baseline-checks/{id}` | One baseline check | 200 |
| `POST` | `/api/baseline-checks/{id}/cancel` | Cancel a running check | 200 |
| `POST` | `/api/control/{action}` | `pause`, `resume` (Start continuous), `cycle` (Run once) or `audit` | 200 |
| `POST` | `/api/doctor` | Connection check, including the sandbox self-test; `?mode=audit` checks planning only | 200 |
| `POST` | `/api/sandbox/self-test` | Run the [containment self-test](sandbox.md#prove-it-the-self-test) in a probe sandbox; `409` when the sandbox is off | 200 |
| `POST` | `/api/model-catalog` | [Runner model catalog](model-routing.md#dashboard-and-api) | 200 |
| `GET` | `/api/events` | Newest 200 activity events; `?entity=<id>` filters them | 200 |

Task and cycle actions answer `{"ok":true}`; control actions answer the saved control
record. The history pages (`/api/tasks`, `/api/cycles`, `/api/prs`, `/api/proposals`)
answer `{"items":[…],"counts":{…},"next_cursor":…}` newest first and accept
`before=<next_cursor>`, `limit` (default 50, clamped to 1–100), `q` (case-insensitive
text) and `status`: a status name or `all`, and for tasks also `active` or `attention`.
For proposals, `status` is a decision and `cycle=<cycle id>` selects one cycle.
Doctor and model-catalog checks have one 90-second execution deadline across all
their steps. The HTTP request owns cancellation; disconnecting stops the check.
Runner cleanup is joined before removing scratch roots, including after a deadline
or service shutdown.
These diagnostics do not become durable jobs.

`GET /api/cycles?cycle=ID` returns only that cycle's summary. The dashboard refreshes
the newest history page and the selected older cycle independently of live status.
Loaded older pages remain available; **Load older cycles** fills any gap after new
cycles arrive before continuing through the remaining history.

`POST /api/doctor` takes `mode=execution` (the default) or `mode=audit`. Its answer,
passing or failing, carries `checked_config` and `checked_revision`; a failing check
answers with its error status and message instead of the result. The service never
writes the check's version warnings to its log.

## CLI

```text
octomus-agent [--data-dir PATH] [--listen IP:PORT] [--assets PATH] [--sandbox docker|off]
octomus-agent --sandboxd
octomus-agent --sandboxd-check
octomus-agent --egress
octomus-agent --healthcheck [--listen IP:PORT]
octomus-agent --print-config
octomus-agent --data-dir PATH --usage-report
octomus-agent --data-dir PATH --doctor
octomus-agent --data-dir PATH --doctor --audit
octomus-agent --data-dir PATH --export-run CYCLE_ID
octomus-agent --help
octomus-agent --version
```

`--data-dir` defaults to `.octomus` in the working directory and `--listen` to
`127.0.0.1:4200`. Service startup and read-only exports resolve `--data-dir`
symlinks before parent (`..`) segments, so the same path selects the same state.
Read-only exports require that directory to exist and never create it. Environment
equivalents: `OCTOMUS_DATA_DIR`, `OCTOMUS_LISTEN`, `OCTOMUS_ASSETS`; the service also requires `OCTOMUS_TOKEN` (or `OCTOMUS_TOKEN_FILE`).
`--sandbox` (`OCTOMUS_SANDBOX`) defaults to `docker`, which runs every runner and
verification command through the broker at `OCTOMUS_SANDBOXD_SOCKET`; `off` runs them on
this host. An explicit `--sandbox` overrides the environment default, including a
mistyped default. Without an override, an invalid default refuses the command;
`--help` and `--version` remain available without starting the service or writing state.
`--sandboxd` serves the broker and `--egress` the egress gateway, each configured
from the environment in [compose.yaml](../deploy/docker/compose.yaml). `--sandboxd-check` asks
the broker over its socket whether it serves sandboxes; the sandboxd container's HEALTHCHECK runs
it, and the control plane waits for that health before starting. `--healthcheck` asks
the service at `--listen` (or `OCTOMUS_LISTEN`) for `/healthz`; wildcard bind addresses
use loopback in the same IP family. The container image also uses
`--sandbox-init` (the in-sandbox helper) and `--git-credential` (the control plane's Git
credential helper); neither is meant to be run by hand. `--assets`/`OCTOMUS_ASSETS`
explicitly replaces embedded serving with a directory containing `200.html`; the default
needs no asset files. `--doctor --audit` (or **Check audit connection**) checks only
planning prerequisites. The `--doctor` check takes the same state lock as the service;
stop the service first, or use **Check connection** in the running dashboard.

`--doctor` prints the result as JSON, including each required runner's installed
version and protocol baseline (Codex, OpenCode or both), and writes each version
mismatch to stderr as a `WARN` line, also when the check fails. Correct a mismatch
before live commissioning. SIGINT, SIGTERM or a hangup stops a running `--doctor`
along with the runner processes it started; it then exits with status 1 and
`Error: Doctor interrupted`.

`--usage-report` opens SQLite state read-only at this release's schema version — it refuses an older database until the service has upgraded it — works alongside the service, and needs neither a token nor dashboard assets. It exports admission counts and saved cycle/task evidence, not provider billing. See the [cost methodology](cost.md). Admission records are retained with the state database; include their growth in disk monitoring and backups. `--export-run` likewise opens the database read-only, without the service lock, and prints the recorded `RunEvidenceV1` for one saved cycle; see [run evidence](run-evidence.md).
