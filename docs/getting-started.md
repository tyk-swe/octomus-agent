# Getting started

Octomus is a self-hosted preview for one operator and one repository.
[Deployment](deployment.md) covers the Docker stack, the checksum-verifying release
installer and building from source.

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
subscriptions; Octomus's session-admission limit is not a dollar budget. Confirm paid
overage is disabled for subscription-only operation. See [cost](cost.md) for what is
measured and what remains unvalidated.

## 1. Deploy

Follow [Docker deployment](deployment.md#docker-deployment): `setup.sh` builds and starts
the stack and prints a new operator token once, which you should save in your password
manager. Sign in a runner, open the dashboard through an [SSH tunnel](deployment.md#private-access)
and enter the token. The service starts paused.

Without Docker, a [dedicated VM without a sandbox](deployment.md#dedicated-vm-without-a-sandbox)
runs runner and verification commands with the service user's permissions instead.

## 2. First run: enter, save, check, then choose

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
