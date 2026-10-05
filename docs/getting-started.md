# Getting started

Octomus is a self-hosted preview for one operator and one repository. Install the
published release with the checksum-verifying installer below, deploy the signed
release images, or build from source; [releasing](releasing.md) describes how the
release is built, packaged and verified.

Your first run has four explicit steps:

1. **Enter** your repository, model routes, and verification commands.
2. **Save** the configuration to the service.
3. **Check** the connection using the saved configuration.
4. **Run an audit** to discover and review recommendations without queuing code changes.

An audit is the recommended starting point. **Run once** executes accepted work from a
fresh planning cycle; continuous operation is a separate choice. Nothing in setup starts
model work automatically. The optional clean-baseline check runs saved verification
commands ahead of any model work and is separate from verifying a task's changes.

## Prerequisites you must already have

- **A Linux host with Docker Engine 28 or later** and the Compose plugin, x86_64 or aarch64.
  A VPS you already use is fine: every agent turn and verification command runs in its own
  [sandbox](sandbox.md), and the control plane never runs repository code. Keep the host's
  other services off the published internet as usual; the dashboard binds to loopback only.
- **Owner-supplied accounts**, arranged and approved before you start: a Codex or
  OpenCode provider login that exposes the models you intend to route, and a GitHub
  fine-grained token limited to the one target repository (Contents and Pull requests
  read/write; Checks and Commit statuses read). Octomus never creates accounts or
  performs logins; you run each login yourself.
- Your project's build and test tools, added to the sandbox image when verification needs
  them (see [extending the sandbox image](sandbox.md#extend-the-sandbox-image)).

You supply the host and provider access. Their charges depend on usage and your
subscriptions; Octomus's session-admission limit is not a dollar budget. See [cost](cost.md)
for what is measured and what remains unvalidated.

## 1. Deploy with Docker

```bash
git clone https://github.com/tyk-swe/octomus-agent.git
cd octomus-agent/deploy/docker
./setup.sh
```

The script:
- checks Docker;
- asks for `OWNER/REPOSITORY` and the GitHub token;
- prints a new operator token once, which you should save in your password manager;
- builds the images and starts the stack.

The control plane clones the repository into its own data volume on first start. Then sign
in a runner; the login lands in a volume only runner sandboxes mount, and it reaches out only
through the egress gateway. For OpenCode routes, first add `models.opencode.ai` and your
provider's sign-in host to `OCTOMUS_EGRESS_MODEL_HOSTS` in `.env` and run `docker compose up -d`.

```bash
docker compose run --rm login codex login --device-auth
# or, for OpenCode routes:
docker compose run --rm login opencode auth login
```

From your own computer, forward the dashboard port and open **http://127.0.0.1:4200**:

```bash
ssh -N -L 4200:127.0.0.1:4200 your-host
```

Enter the operator token. The service starts paused. [Deployment](deployment.md) covers
host settings, upgrades and backups; continue with [the first run](#3-first-run-enter-save-check-then-choose).

## Alternative: a dedicated VM without a sandbox

The steps below install Octomus directly on a **dedicated Ubuntu 24.04 VM** that runs
nothing else, with `--sandbox off`. Runner and verification commands then execute with the
service user's permissions and are not sandboxed. Do not use your workstation, and keep
unrelated credentials off the VM. You also need, on the VM:
- the target repository, cloned to a persistent path writable by the service user;
- its build and test tools;
- Git, gh, curl, OpenSSL, Go (per `go.mod`), Node 22.12+ and npm to build Octomus.

### Install the tools and application

As the VM administrator, install Git, gh, curl and OpenSSL. This npm-based Codex
installation uses Node 22 from [NodeSource](https://github.com/nodesource/distributions)
and the pinned Codex npm package, version 0.153.4.
Install the runners you intend to use. The Codex setup below is optional for an
OpenCode-only installation. For OpenCode, install the pinned
[1.18.30 release](https://github.com/anomalyco/opencode/releases/tag/v1.18.30)
for your platform. Octomus's binary itself needs no build toolchain at runtime.

```bash
sudo apt-get update
sudo apt-get install -y git gh curl ca-certificates openssl
curl -fsSL https://deb.nodesource.com/setup_22.x -o /tmp/octomus-node22.sh
sudo bash /tmp/octomus-node22.sh
sudo apt-get install -y nodejs
sudo npm install -g @openai/codex@0.153.4
```

Add the Go toolchain (per `go.mod`; from [go.dev](https://go.dev/dl/) or your
distribution), then build the dashboard before the binary.
Python is only needed for repository tests.

```bash
git clone https://github.com/tyk-swe/octomus-agent.git
cd octomus-agent
npm ci --prefix web
make build
sudo install -m 755 bin/octomus-agent /usr/local/bin/octomus-agent
```

**Release installer:** binary releases are published; the installer supports the
following command:

```bash
curl -fsSL https://raw.githubusercontent.com/tyk-swe/octomus-agent/main/install.sh | sh
```

The installer verifies the downloaded archive against release SHA-256 checksums
and installs to `/usr/local/bin`. To select a version, download the script and run
`sh install.sh v0.2.0`, or set `OCTOMUS_VERSION=v0.2.0` for the piped `sh`; an
alternate writable absolute destination is supported through `INSTALL_DIR`.
Checksums detect corruption; they are not independent signatures against a
compromised release account.

The executable is statically linked (`CGO_ENABLED=0`) and embeds the dashboard,
so installation is a single administrator-owned file with no runtime toolchain.
Use a fresh data directory, or state from v0.1.0 or later: the service upgrades an
older release's database at startup after writing a backup beside it; see
[Backup and upgrade](deployment.md#backup-and-upgrade). Preserve a separate backup of any
older state.

### Connect as the service user

Create a dedicated account without sudo access and a persistent checkout location:

```bash
sudo useradd --create-home --home-dir /var/lib/octomus --shell /bin/bash octomus
sudo install -d -o octomus -g octomus /srv/projects
sudo -iu octomus
codex login
gh auth login
gh auth setup-git
```

If using OpenCode, run `opencode auth login` as this same service user and configure
its providers in the user-level OpenCode configuration. Skip `codex login` when no
Codex routes are selected. Octomus reads those provider settings and credentials;
it does not manage provider logins in the dashboard.

Use the dedicated identity and repository-restricted authentication arrangement,
not an unrelated personal credential. As this same user, replace the sample
repository identity and clone it. Configure Git identity if your project requires it.

```bash
git clone https://github.com/OWNER/REPOSITORY.git /srv/projects/project
export OCTOMUS_TOKEN="$(openssl rand -hex 32)"
printf '%s\n' "$OCTOMUS_TOKEN"
```

Save this token in your password manager; it grants operator access. Confirm paid
overage is disabled for subscription-only operation. Then start the service:

```bash
octomus-agent --data-dir /var/lib/octomus/.octomus --sandbox off
```

From your own computer, forward the dashboard port (replace `your-vm` with the
VM's SSH destination):

```bash
ssh -N -L 4200:127.0.0.1:4200 your-vm
```

Open **http://127.0.0.1:4200**, enter your saved token, and keep the service running
in the VM terminal. It starts paused. Refreshing the page requires the token again.

## 3. First run: enter, save, check, then choose

Open **Configuration**. The **Setup checklist** at the top tracks seven steps (including
the sandbox and an optional clean-baseline check) and labels
each one as entered (typed in this tab), saved (sent to the service), checked (the
saved configuration passed an explicit connection check) or ran (a cycle actually
executed). Its links move focus to the existing controls. Nothing on the checklist
starts work, and populated fields, catalog matches or a passed check never prove
repository push permission or model inference; only a run's recorded evidence does.

1. **Sandbox.** Shows **Proven** once a connection check or the Overview's **Run
   self-test** has checked containment from inside a real sandbox, and **Off** when the
   service runs unsandboxed.
2. **Repository details.** In the Docker deployment, the checkout and `OWNER/REPOSITORY`
   are already set by the deployment and cannot be changed here. Unsandboxed, enter
   `/srv/projects/project` and `OWNER/REPOSITORY`. Then enter the default branch and an
   owned branch prefix. The prefix marks the branches Octomus owns; the default is
   `octomus/`.
3. **Model routes.** Unsandboxed, set the executable paths; in Docker the runners come from
   the sandbox image. Then use **Load Codex models** or
   **Load OpenCode models**. For each role, choose a runner and model. Codex requires
   reasoning effort; OpenCode requires a provider and offers the model's supported
   variants, including **Provider default**. The same selectors apply to all execution
   tiers and repair. Catalog checks use the executable paths currently entered, without
   saving or making model calls. Custom providers in OpenCode's user configuration (the
   service user's, unsandboxed; in Docker, the one
   [installed into the runner volume](sandbox.md#signing-in-a-runner)) appear in its
   catalog; models must support text and tool calling. Unsupported
   routes fail visibly; select available routes explicitly instead of expecting a
   fallback. See [model routing](model-routing.md) for JSON examples.
4. **Verification policy.** Enter meaningful verification commands, one shell command
   per line; all must pass on the reviewed revision before a PR is published. Each
   command runs in a fresh sandbox, so its tools must be in the sandbox image and any
   package registry it needs in `OCTOMUS_EGRESS_BUILD_HOSTS`. For Octomus itself, install Go (per
   `go.mod`), a C compiler for the race detector, Node 22.12+, Python 3 and the
   Playwright prerequisites, then use:

   ```bash
   npm ci --prefix web && make check && make test
   ```

   An audit needs no commands; **Run once** refuses to start without at least one.
5. **Save configuration**, then **Check connection** or **Check audit connection**.
   Both stay disabled while edits are unsaved. In the Docker deployment the check first
   runs the sandbox self-test and fails if any containment check fails. It then validates the saved origin
   remote, the GitHub CLI login and the runner catalogs for the saved routes. It does
   not prove push permission and makes no model call. Correct any CLI version warning
   before live work. Any later saved change invalidates the result, so check again.
   If a saved field ever appears as a hidden or shortened preview, it stays locked and
   unchanged until you use its explicit **Replace** action.
6. Optionally use **Check clean baseline** while paused and idle. Confirm the saved
   commands will run, in sandboxes (or with the service user's permissions when
   unsandboxed), in a disposable clone of the
   remote default branch. This makes no model calls and creates no tasks or PRs. Inspect
   the checked revision, bounded output, cancellation/timeout status and configuration
   freshness. A baseline pass is never verification of a later task's changes.
7. **Choose Audit or Run once** on the Overview while the service is paused and idle.
   **Run an audit** runs one planning pass, records accepted, rejected and deferred
   decisions with reasons, queues nothing and leaves execution paused; no later cycle
   executes an audit's recommendations. With nine discovery agents a completed pass
   normally consumes 13 session admissions, and audit agents run in sandboxes like any other
   agent (unsandboxed with `--sandbox off`).
   Read the **Proposals** view before going further. **Run once** drains any existing
   queue, plans one cycle, finishes its accepted tasks and pauses. Start conservatively:
   one concurrent task, one task per cycle and a 21,600-second interval. This is a
   conservative starting profile, not a measured replacement for shipped defaults.

Unsaved edits, verification commands and loaded model catalogs survive dashboard
navigation in this tab. **Discard changes** restores the last loaded or saved
configuration without writing to the server. Revisiting a clean form refreshes saved
values; dirty drafts and failed saves keep your edits. Disconnecting, session expiry or
reloading the page clears the draft and the checklist's check result. Existing saved
model IDs stay visible when a catalog changes or cannot be loaded.

Use **Start continuous** only when you want ongoing scheduling; it is never the default
first action. Inspect each task's review and verification evidence and its PR; only you
decide to merge. A cycle with no worthwhile work is a valid outcome.

A complete planning pass needs 12–14 admissions (13 with nine discovery agents).
Unaffordable audits and Run once requests are refused before spending admissions.
Continuous operation waits for allowance to return without creating failed cycles;
Run once rechecks after draining queued work and pauses if planning is no longer affordable.

**Open PR capacity** defaults to five owned open PRs. New-PR tasks wait when those PRs
plus admitted deliveries fill the limit, or when remote state cannot be established.
Existing-PR maintenance remains eligible. Lowering the limit never closes PRs or
interrupts already admitted publication.

For unattended attention notices, optionally set `OCTOMUS_NOTIFICATION_WEBHOOK_URL`
in the protected service environment and restart. Only minimal task/run identity and
failure categories are sent, with bounded retries; duplicate delivery is possible.
The URL is not returned by the dashboard or inherited by runner/verification commands.
Inspect delivery health in Configuration. See [deployment](deployment.md).

**Pause** stops new work; active tasks may finish and publish. Use **Cancel task** to
stop an individual task, or stop the service to terminate its workers. Audits cannot
start alongside active work. For durable service setup, follow
[deployment](deployment.md).
