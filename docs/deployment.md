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
exported `OCTOMUS_GITHUB_REPO`, quoted `.env` values and interpolation, and the setup
prompt names the repository the containers will use.

On first start the control plane clones the repository into its data volume. That clone
is the trusted checkout, and the dashboard cannot repoint it.

Then sign in a runner. The login is stored in the `octomus-runner` volume, which only
runner sandboxes mount:

```bash
docker compose run --rm login codex login --device-auth
# or
docker compose run --rm login opencode auth login
```

A login reaches out only through the egress gateway under the runner allowlist. Codex's
sign-in hosts are allowed by default; for OpenCode, first add `models.opencode.ai` and your
provider's sign-in host to `OCTOMUS_EGRESS_MODEL_HOSTS` in `.env` and run
`docker compose up -d` ([signing in a runner](sandbox.md#signing-in-a-runner)).

Open the dashboard through an [SSH tunnel](#private-access) and run **Check
connection**, which proves the sandbox from inside one before any work starts ([the
self-test](sandbox.md#prove-it-the-self-test)).

### Host settings

Everything security-relevant is set in `.env`, not in the dashboard, which shows it
read-only: the pinned repository, sandbox limits, image and runtime, and the egress
allowlists. [env.example](../deploy/docker/env.example) documents each setting and
[sandbox](sandbox.md) explains them; after changing `.env`, run `docker compose up -d`.

Add your project's build and test tools to the sandbox image, not the host, since
verification commands run in it: see
[extending the sandbox image](sandbox.md#extend-the-sandbox-image). A provider behind a
private certificate authority needs the
[host-owned runner CA bundle](sandbox.md#private-provider-certificates), and automatic
maintenance merging needs the separate credentials and server-side rules in
[maintenance merge setup](maintenance-merging.md).

### Operating the stack

| Task | Command |
| --- | --- |
| Status | `docker compose ps` |
| Logs | `docker compose logs -f octomus` (egress decisions: `docker compose logs egress`); Docker keeps at most five 10 MB files per service |
| Stop, keeping state | `docker compose stop` |
| Restart after an `.env` change | `docker compose up -d` |
| Upgrade | `git pull`, `docker compose build octomus sandbox-image`, rebuild any [derived sandbox image](sandbox.md#extend-the-sandbox-image), then `docker compose up -d` |

The control plane stops within a 75-second grace period that covers the broker's
confirmation of each sandbox's removal. Stopping it closes every sandbox's stream, so the
broker removes them all, and it sweeps any left from a previous run at startup, touching
only containers carrying its own `octomus.sandbox.instance` label.

State lives in named volumes:
- `octomus-data`: the database, workspaces and the trusted checkout;
- `octomus-runner`: runner logins and session transcripts;
- `octomus-tools`: the broker's helper binary and optional public CA bundle, recreated on start.

`setup.sh` gives the secret files under `secrets/` to the container user (uid 10001) at
mode 0600; each secret file (`*_FILE`) must hold one non-empty line of at most 64 KiB.
The control plane reads them only at start, and `docker compose up -d` does not notice a
changed one. To rotate one, install the new value with the same owner and mode, then
recreate the control plane:

```bash
sudo install -o 10001 -g 10001 -m 0600 /path/to/new-token secrets/github_token
docker compose up -d --force-recreate octomus
```

After rotating `secrets/operator_token`, sign in to the dashboard with the new token;
browser sessions using the old one stop working.

## Dedicated VM without a sandbox

This deployment runs runner sessions and verification commands with the service account's
full host permissions (`--sandbox off`). Task clones separate mutable work but are not a
sandbox, so use a dedicated Ubuntu 24.04 VM that runs nothing else, never your workstation,
and keep unrelated production credentials and services off it. The dashboard shows a
permanent **Unsandboxed** warning.

### Install

As the administrator, install Git, gh and the runners your routes select, pinned to the
tested protocol versions: Codex CLI **0.163.0** (through npm, with Node 24 LTS from
[NodeSource](https://github.com/nodesource/distributions)) and/or the OpenCode
[1.19.4 release](https://github.com/anomalyco/opencode/releases/tag/v1.19.4) for your
platform. Skip the Node and Codex lines for an OpenCode-only installation.

```bash
sudo apt-get update
sudo apt-get install -y git gh curl ca-certificates openssl
curl -fsSL https://deb.nodesource.com/setup_24.x -o /tmp/octomus-node24.sh
sudo bash /tmp/octomus-node24.sh
sudo apt-get install -y nodejs
sudo npm install -g @openai/codex@0.163.0
```

Install the release with the checksum-verifying installer:

```bash
curl -fsSL https://raw.githubusercontent.com/tyk-swe/octomus-agent/main/install.sh | sh
```

It verifies the archive against the release's SHA-256 checksums and installs to
`/usr/local/bin`. To select a version, download the script and run `sh install.sh vX.Y.Z`,
or set `OCTOMUS_VERSION=vX.Y.Z` for the piped `sh`; `INSTALL_DIR` selects another writable
absolute destination. Checksums detect corruption; they are not independent signatures
against a compromised release account. Archives for x86_64 and aarch64 are on the
[releases page](https://github.com/tyk-swe/octomus-agent/releases).

To build from source instead, on the target architecture or another compatible Linux host,
you need Go (per `go.mod`), Node 24 LTS (see `.node-version`) and npm for the embedded dashboard:

```bash
git clone https://github.com/tyk-swe/octomus-agent.git
cd octomus-agent
npm ci --prefix web
make build
sudo install -m 755 bin/octomus-agent /usr/local/bin/octomus-agent
```

The executable is statically linked, embeds the dashboard and must remain
administrator-owned. Use a fresh data directory or state from v0.1.0 or later; older
state is upgraded at startup after a backup ([Backup and upgrade](#backup-and-upgrade)).

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

Use a dedicated identity with repository-restricted authentication, not a personal
credential, and verify the account can fetch the checkout's origin without prompting.
Configure OpenCode's providers in that user's OpenCode configuration; Octomus never
manages logins.

Install the target project's build and test tools too, and add their locations to the
unit's PATH with a systemd override, since a service does not load the interactive
shell's profile.

Create `/etc/octomus/agent.env`, readable only by the administrator and service account,
with a fresh random token:

```text
OCTOMUS_TOKEN=<output of openssl rand -hex 32>
```

Do not use the literal placeholder. Use `chmod 600` and assign ownership appropriately.
The service refuses tokens shorter than 32 characters. The token is an operator
capability; it is not passed to child commands. To rotate it, edit the file and restart
the service; existing browser tokens stop working immediately.

The unit makes system paths read-only and permits persistent writes only below
`/var/lib/octomus` and `/srv/projects/octomus-agent`, so Codex state and tool caches
must live in the service home and tools must be installed as the administrator before
starting. NoNewPrivileges prevents verification commands from acquiring sudo
privileges, and PrivateTmp gives workers temporary storage without sharing the host's
`/tmp`. These restrictions do not isolate agents from the service user's credentials;
see the [threat model](threat-model.md).

Create `/etc/octomus` and the checkout directory before starting. For a checkout path
other than `/srv/projects/octomus-agent`, use `sudo systemctl edit octomus-agent` with:

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

The unit uses `KillMode=control-group` so crashes and restarts cannot leave old task
processes running alongside recovered work; do not change it to `process`. The service
also kills each owned process group on cancellation or timeout. Escaped processes on
this intentionally unsandboxed host remain an operator responsibility.

## Optional attention webhook

Set `OCTOMUS_NOTIFICATION_WEBHOOK_URL=<HTTPS webhook URL>` in the protected environment
file and restart (in Docker, write the URL under `secrets/` and set
`OCTOMUS_NOTIFICATION_WEBHOOK_URL_FILE` in a compose override for `octomus`). Unset it to
disable; rotating it cancels pending deliveries to the old URL. The URL is never saved in
task/config snapshots, returned via APIs, or passed to child processes.

Notices fire on newly blocked/failed or published tasks, failed planning cycles, completed
audits (including idle), and error-paused service episodes (including at restart recovery).
Historical events are not backfilled, and resaving/archiving terminal records creates no
new notice. Each `application/json` POST contains:

| Key | Value |
| --- | --- |
| `schema_version` | `1` |
| `event_id` | 32 lowercase hex chars, unique per notice and reused on retries |
| `occurred_at` | UTC RFC 3339 timestamp with milliseconds (`2026-09-26T12:00:00.123Z`) |
| `repository` | Recorded GitHub `OWNER/REPOSITORY` (from the cycle/task snapshot), or `""` |
| `cycle_id`, `run_id`, `task_id` | String or `null`, never absent (`run_id` is set only inside a Run once batch; cycle/service notices have `task_id: null`) |
| `category` | Task blocked reason, `unknown`, `task_published`, `cycle_failed`, `audit_completed`, or `service_error_paused` |
| `action` | `inspect_task`, `inspect_cycle`, or `inspect_service` |

HTTPS is required except literal loopback HTTP. Redirects and proxies are disabled;
requests time out at 10s with up to 5 attempts (30/120/600/1,800s backoff, 1s spacing,
24h expiry, 1,000 pending cap). Queue overflow, expiry, setup errors and delivery failures
appear in Configuration.

## Private access

The default listener is `127.0.0.1:4200` (`127.0.0.1:${OCTOMUS_PORT}` in Docker). Access
it via SSH tunnel:

```bash
ssh -N -L 4200:127.0.0.1:4200 your-host
```

Open `http://127.0.0.1:4200` locally. Behind a reverse proxy, provide TLS and an operator
access boundary; the bearer token is still required and CORS is disabled. `/healthz`
exposes only liveness and version. Configure the repository, routes, verification commands
and limits while paused, then run **Check connection**.

## Controls and recovery

- **Pause:** stops new dispatch; active discovery and tasks finish their workflow (use
  **Cancel task** to stop a running task).
- **Check clean baseline:** runs saved verification commands in a disposable clone of the
  remote default revision while paused and idle (**Cancel** stops it; results are separate
  from task verification).
- **Run an audit:** plans and records decisions while paused and idle without creating
  tasks ([architecture](architecture.md#planning)).
- **Run once:** drains queued tasks, runs one discovery cycle, drains its accepted tasks,
  and pauses. Survives restart; a failed initial drain or interrupted planning pauses
  without replay.
- **Start continuous:** enables queued execution and scheduled discovery cycles.
- **Retry task:** retries eligible failures with the original task contract/workspace and
  current attempt limits (timeouts, repair/no-progress rounds, retry ceiling).
- **Stale context:** **Supersede and rediscover** seeds the next execution cycle with a
  replacement request (withdrawn if archived first). **Reconcile publication** re-runs
  publication checks on a saved output commit without a model turn.
- **Request work:** asks planning for specific work from the dashboard's Requests view;
  pending requests enter the next cycle as ordinary candidates and are settled with the
  plan (accepted links the task, rejected is final, deferred stays pending). Withdraw is
  refused while planning runs.
- **Restart:** initialized in-flight tasks queue for automatic recovery within the retry
  budget (uninitialized or retry-exhausted tasks block; exhausted output-checkpoint tasks
  block as `publication_uncertain` for **Reconcile publication**).
- **Model/auth or repair limits:** there is no silent route fallback; to change a task's
  route, pause, cancel, save new routes, and **Supersede and rediscover**. Unresolved
  repair/verification failures stay blocked with their evidence and workspace intact.

Configuration edits require paused operation with no active task, cycle, baseline check or
auto-merge check, and repository/branch/delivery policy cannot change while unresolved
tasks exist ([configuration](configuration.md#display-safe-values-and-saved-revisions)).

## Limits and retention

See [configuration.example.json](configuration.example.json) and the
[field reference](configuration.md#field-reference) for defaults and ranges. Manual audit
and Run once starts require enough remaining daily admissions for a full planning pass
(`discovery_agents + 4`; see [architecture](architecture.md#planning) and
[PR capacity](architecture.md#tasks-and-dependencies)). Workspace limits are pre-turn
admission checks, and admissions do not cap provider dollar spend.

Housekeeping runs every 15 minutes in all modes. Published task workspaces and completed
cycle directories expire after `retain_completed_days` (default 14); clean planning-role
clones are removed immediately, while failed/modified clones and unresolved tasks stay.
**Archive task/cycle** marks work eligible for retention and **Discard workspace** removes
an archived workspace (each runs once; database evidence and lineage remain). Activity
events are capped at `retain_events` without raw tool streams. Manage runner transcript
retention (`octomus-runner` volume or host runner directories) separately. Logs (`docker
compose logs octomus` or `journalctl -u octomus-agent`) and API responses scrub known
secret patterns, but keep secrets out of repository docs and prompts.

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

Both archives hold credentials (runner logins, the GitHub token and the operator token),
so move them off the host or into a directory only you can read.

Under systemd:

```bash
sudo systemctl stop octomus-agent
```

Back up `/var/lib/octomus/.octomus` in full (the database and its WAL files, task
workspaces), the service account's selected runner session stores (Codex home and/or
OpenCode's XDG data directory) and the target repository, together, and protect them as
sensitive operator data. Do not copy only the SQLite database while it is being written.

### Upgrading

Take the backup above first, then install the tested release (the new binary under
systemd, or `OCTOMUS_IMAGE` and `OCTOMUS_SANDBOX_IMAGE` in `deploy/docker/.env` for
Docker) and start the service.

When the release's schema is newer, the first start (the service or `--doctor`) checks
the saved version before any schema or journal change. It then writes an
integrity-checked, mode 0600 copy `state.db.v<old>-backup-<UTC yyyymmddThhmmssZ>` beside
`state.db`, never overwriting an earlier backup, and only then configures WAL and applies
each migration in its own transaction. The start logs `Upgraded state database from
schema version …` (`docker compose logs octomus`, `journalctl -u octomus-agent`) and
records an `upgrade` activity event.

A failed migration rolls back alone: the database stays at the version that step started
from, the error names the backup, and starting again retries with a fresh backup. If the
backup cannot be written, nothing changes. A database newer than the binary is refused
untouched, as are pre-release databases older than version 7, which need a fresh data
directory. `--usage-report` and `--export-run` from a newer binary refuse an older
database until the service has upgraded it. Restarting the same release changes nothing.

The backup holds the same sensitive data as `state.db` and counts toward application
storage; remove it once the upgraded release has run satisfactorily.

### Rolling back

There are no down-migrations, and an older release refuses a newer schema, so a rollback
restores the pre-upgrade backup (`N` is the schema version the backup was taken from).
Stop the service. Move `state.db`, `state.db-wal` and `state.db-shm` aside together —
leaving a newer WAL beside the restored file would corrupt it. Copy the backup to
`state.db` with the service user's ownership and mode 0600, reinstall the older release
and start.

In the Docker deployment, run the file moves in a throwaway container on the
`octomus-data` volume, mounted at `/var/lib/octomus/data` inside the service container;
the service runs as uid 10001:

```bash
docker compose stop
docker run --rm --network none -v octomus-data:/data debian:trixie-slim sh -c '
  cd /data && install -d -o 10001 -g 10001 -m 700 .rollback && \
  for f in state.db state.db-wal state.db-shm; do [ ! -e "$f" ] || mv "$f" .rollback/; done && \
  install -o 10001 -g 10001 -m 600 state.db.v<N>-backup-<timestamp> state.db'
```

Then point `OCTOMUS_IMAGE` and `OCTOMUS_SANDBOX_IMAGE` in `deploy/docker/.env` back at
the older release, pull and `docker compose up -d`.

Under systemd, state lives in `/var/lib/octomus/.octomus`, owned by the `octomus` user:

```bash
sudo systemctl stop octomus-agent
sudo -u octomus sh -c '
  cd /var/lib/octomus/.octomus && mkdir -m 700 -p .rollback && \
  for f in state.db state.db-wal state.db-shm; do [ ! -e "$f" ] || mv "$f" .rollback/; done && \
  install -m 600 state.db.v<N>-backup-<timestamp> state.db'
sh install.sh vX.Y.Z
sudo systemctl start octomus-agent
```

Anything recorded after the upgrade is lost from state. PRs it opened stay on GitHub,
and task workspaces it created may remain on disk.

## HTTP API

The dashboard uses this API; scripts can call it with the same token. The route
table in `internal/httpapi/routes.go` is authoritative.

- Every route below is under `/api` and needs `Authorization: Bearer <token>`.
  `/healthz` needs no token and reports only liveness and version.
- Every `POST` and `PUT` must send exactly one `Content-Type: application/json` header
  (media type case-insensitive, parameters allowed), even without a body such as
  `POST /api/control/pause`; otherwise the answer is `415 {"error":"Use application/json"}`.
- Checks run in order: an unknown path answers `404 {"error":"Unknown API route"}` before
  authentication; a missing, duplicate or wrong `Authorization` header answers `401` after
  the failed-authentication backoff; then the content type is checked; a known path with
  the wrong method answers `405` with an `Allow` header and an empty body.
- Handler errors are JSON `{"error":"…"}`: `404` for an unknown task, cycle, proposal,
  baseline check (when read) or action name, `409` for a conflict with current state
  (including a superseded configuration revision, and cancelling a baseline check that
  does not exist, already finished or is no longer running), `500` for storage or
  encoding failures, and `400` otherwise, such as a configuration value out of range.
  A `409` may also carry a stable `code` beside the message, which clients should branch
  on instead of the text: `config_revision_conflict` (reload the configuration),
  `active_work` (wait for operation to settle), `unresolved_work` (resolve tasks before
  changing repository, branch or delivery policy), `recovery_pending` (wait for
  saved-state recovery) and the generic `action_conflict`.
  Request rejections are `text/plain`: `400` for malformed JSON or a malformed query
  value, `422` when the body or a `config` patch fails strict decoding (a wrong type, an
  unknown field or an unknown name), and `413` for a body over 256 KiB.
- Every `/api` JSON response is redacted. Request headers must arrive within 10
  seconds, and idle keep-alive connections close after two minutes.
- Hashed dashboard assets under `/_app/immutable/` are cacheable for a year
  (`immutable`) and served gzip-compressed when the client accepts it; every
  other response is `no-store`.

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
| `GET` | `/api/requests` | Pending and decided operator requests plus eligible targets | 200 |
| `POST` | `/api/requests` | `{"title":"…","description":"…","target":"…"}` files a request | 201 |
| `POST` | `/api/requests/{id}/{action}` | `withdraw` | 200 |
| `GET` | `/api/prs` | Observed pull-request page | 200 |
| `GET` | `/api/insights` | Read-only usage and outcome summary for the last 7, 30 or 90 UTC days (`days`, default 30) | 200 |
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
text) and `status` (a status name or `all`; for tasks also `active` or `attention`; for
proposals a decision, with `cycle=<cycle id>` selecting one cycle).
`GET /api/cycles?cycle=ID` returns only that cycle's summary.

`POST /api/doctor` takes `mode=execution` (the default) or `mode=audit`. Its answer,
passing or failing, carries `checked_config` and `checked_revision`; a failing check
answers with its error status and message instead. Doctor and model-catalog checks share
one 90-second deadline, stop when the client disconnects, and never become durable jobs.

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
octomus-agent -h | --help
octomus-agent -V | --version
```

`--data-dir` defaults to `.octomus` in the working directory and `--listen` to
`127.0.0.1:4200`; read-only exports require the data directory to exist and never create
it. Environment equivalents: `OCTOMUS_DATA_DIR`, `OCTOMUS_LISTEN`, `OCTOMUS_ASSETS`, and
for the service `OCTOMUS_TOKEN` (or `OCTOMUS_TOKEN_FILE`), which it requires.

| Flag | Purpose |
| --- | --- |
| `--sandbox docker\|off` (`OCTOMUS_SANDBOX`) | `docker` (default) runs every runner and verification command through the broker at `OCTOMUS_SANDBOXD_SOCKET`; `off` runs them on this host. An explicit flag overrides the environment; an invalid default without one refuses the command |
| `-h`, `--help`, `-V`, `--version` | Always work, without starting the service or writing state |
| `--assets`, `OCTOMUS_ASSETS` | Serve a directory containing `200.html` instead of the embedded dashboard; missing static asset files return `404` while SPA routes fall back to `200.html` |
| `--sandboxd`, `--egress` | Serve the broker and the egress gateway, configured from the environment in [compose.yaml](../deploy/docker/compose.yaml) |
| `--sandboxd-check` | Ask the broker over its socket whether it serves sandboxes; its HEALTHCHECK runs it and the control plane waits for it |
| `--healthcheck` | Ask the service at `--listen` (or `OCTOMUS_LISTEN`) for `/healthz`, using loopback for wildcard addresses |
| `--doctor [--audit]` | Check prerequisites (`--audit`, or **Check audit connection**, planning only); takes the service's state lock, so stop the service first or use **Check connection** in the dashboard |
| `--sandbox-init`, `--login-lease`, `--git-credential` | Used by the container image: the in-sandbox helper, compose's login-lease service (before each runner login) and the control plane's Git credential helper. Not for manual use |

`--doctor` prints the result as JSON, including each required runner's installed
version and protocol baseline, and writes each version mismatch to stderr as a `WARN`
line, also when the check fails. Correct a mismatch before live commissioning. A signal
stops a running `--doctor` and its runner processes; it exits with status 1 and
`Error: Doctor interrupted`.

`--usage-report` opens state read-only at this release's schema version (an older
database is refused until the service has upgraded it), works alongside the service and
needs neither a token nor dashboard assets. It exports admission counts and saved
cycle/task evidence, not provider billing ([cost methodology](cost.md)); admission records
stay with the state database, so include their growth in disk monitoring and backups.
`--export-run` likewise opens the database read-only, without the service lock, and prints
the recorded `RunEvidenceV1` for one saved cycle ([run evidence](run-evidence.md)).
