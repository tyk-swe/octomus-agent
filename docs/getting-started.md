# Getting started

Octomus is a self-hosted preview for one operator and one repository. Installation
currently means building from source. Public release binaries and the crates.io package
are not published yet; [releasing](releasing.md) describes what is prepared for them.

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

- A **dedicated Ubuntu 24.04 VM** (x86_64 or aarch64) that runs nothing else. Runner
  and verification commands execute with the service user's permissions and are not
  sandboxed. Do not use your workstation, and keep unrelated credentials off the VM.
- **Owner-supplied accounts**, arranged and approved before you start: a Codex or
  OpenCode provider login that exposes the models you intend to route, and a dedicated
  GitHub identity whose access is restricted to the one target repository. Octomus never
  creates accounts or performs logins; you run each login yourself as the service user.
- The target repository, cloned to a persistent path writable by the service user, with
  its own build and test tools installed on the VM.
- Build tools, needed only to build Octomus itself: Git, gh, curl, OpenSSL, a C
  compiler, Rust 1.88+, Node 22.12+ and npm. Python 3 is only needed for repository tests.

You supply VM and provider access. Their charges depend on usage and your subscriptions;
Octomus's session-admission limit is not a dollar budget. See [cost](cost.md) for what is
measured and what remains unvalidated.

## 1. Install the tools and application

As the VM administrator, install Git, gh, curl and OpenSSL. This npm-based Codex
installation uses Node 22 from [NodeSource](https://github.com/nodesource/distributions)
and the pinned [Codex release](https://github.com/openai/codex/releases/tag/rust-v0.153.4).
Install the runners you intend to use. The Codex setup below is optional for an
OpenCode-only installation. For OpenCode, install the pinned
[1.18.30 release](https://github.com/anomalyco/opencode/releases/tag/v1.18.30)
for your platform. Octomus's binary itself does not require Node or Rust at runtime.

```bash
sudo apt-get update
sudo apt-get install -y git gh curl ca-certificates openssl
curl -fsSL https://deb.nodesource.com/setup_22.x -o /tmp/octomus-node22.sh
sudo bash /tmp/octomus-node22.sh
sudo apt-get install -y nodejs
sudo npm install -g @openai/codex@0.153.4
```

Add Rust 1.88+ and a C compiler, then build the dashboard before Rust.
Python is only needed for repository tests.

```bash
sudo apt-get install -y build-essential
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs -o /tmp/octomus-rustup.sh
sh /tmp/octomus-rustup.sh -y --profile minimal
. "$HOME/.cargo/env"
git clone https://github.com/tyk-swe/octomus-agent.git
cd octomus-agent
npm ci --prefix web
make build
sudo install -m 755 target/release/octomus-agent /usr/local/bin/octomus-agent
```

**Pending release options:** after binary releases are published, the installer
will support the following command:

```bash
curl -fsSL https://raw.githubusercontent.com/tyk-swe/octomus-agent/main/install.sh | sh
```

The installer verifies the downloaded archive against release SHA-256 checksums
and installs to `/usr/local/bin`. To select a version, download the script and run
`sh install.sh v0.1.0`; an alternate writable absolute destination is supported
through `INSTALL_DIR`. Checksums detect corruption; they are not independent
signatures against a compromised release account.

`cargo install octomus-agent --locked` is also pending crates.io publication. The
crate includes the built dashboard, so installing that package will not require npm.

## 2. Connect as the service user

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
octomus-agent --data-dir /var/lib/octomus/.octomus
```

From your own computer, forward the dashboard port (replace `your-vm` with the
VM's SSH destination):

```bash
ssh -N -L 4200:127.0.0.1:4200 your-vm
```

Open **http://127.0.0.1:4200**, enter your saved token, and keep the service running
in the VM terminal. It starts paused. Refreshing the page requires the token again.

## 3. First run: enter, save, check, then choose

Open **Configuration**. The **Setup checklist** at the top tracks six steps (including
an optional clean-baseline check) and labels
each one as entered (typed in this tab), saved (sent to the service), checked (the
saved configuration passed an explicit connection check) or ran (a cycle actually
executed). Its links move focus to the existing controls. Nothing on the checklist
starts work, and populated fields, catalog matches or a passed check never prove
repository push permission or model inference; only a run's recorded evidence does.

1. **Repository details.** Enter `/srv/projects/project`, `OWNER/REPOSITORY`, the
   default branch and an owned branch prefix. The prefix marks the branches Octomus
   owns; the default is `octomus/`.
2. **Model routes.** Set the executable paths, then use **Load Codex models** or
   **Load OpenCode models**. For each role, choose a runner and model. Codex requires
   reasoning effort; OpenCode requires a provider and offers the model's supported
   variants, including **Provider default**. The same selectors apply to all execution
   tiers and repair. Catalog checks use the executable paths currently entered, without
   saving or making model calls. Custom providers configured for the OpenCode service
   user appear in its catalog; models must support text and tool calling. Unsupported
   routes fail visibly; select available routes explicitly instead of expecting a
   fallback. See [model routing](model-routing.md) for JSON examples.
3. **Verification policy.** Enter meaningful verification commands, one shell command
   per line; all must pass on the reviewed revision before a PR is published. Install
   your project's build and test tools first. For Octomus itself, install Rust with
   rustfmt and clippy, Node 22.12+, Python 3 and the Playwright prerequisites, then use:

   ```bash
   npm ci --prefix web && make check && make test
   ```

   An audit needs no commands; **Run once** refuses to start without at least one.
4. **Save configuration**, then **Check connection** or **Check audit connection**.
   Both stay disabled while edits are unsaved. The check validates the saved origin
   remote, the GitHub CLI login and the runner catalogs for the saved routes. It does
   not prove push permission and makes no model call. Correct any CLI version warning
   before live work. Any later saved change invalidates the result, so check again.
5. Optionally use **Check clean baseline** while paused and idle. Confirm the saved
   commands will run with the service user's permissions in a disposable clone of the
   remote default branch. This makes no model calls and creates no tasks or PRs. Inspect
   the checked revision, bounded output, cancellation/timeout status and configuration
   freshness. A baseline pass is never verification of a later task's changes.
6. **Choose Audit or Run once** on the Overview while the service is paused and idle.
   **Run an audit** runs one planning pass, records accepted, rejected and deferred
   decisions with reasons, queues nothing and leaves execution paused; no later cycle
   executes an audit's recommendations. With nine discovery agents a completed pass
   normally consumes 13 session admissions, and audit agents still run unsandboxed.
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
