# Getting started

Octomus is a self-hosted preview for one operator and one repository. [Deployment](deployment.md)
is the source for Docker, the checksum-verifying release installer, the dedicated VM and
ongoing operations.

Your first run is deliberately explicit:

1. **Enter** the repository, model routes and verification commands.
2. **Save** the configuration.
3. **Check** the saved configuration.
4. **Run an audit** or **Run once**.

The service starts paused. An audit records recommendations without queuing code changes;
Run once plans afresh and executes accepted work. Use [Configuration](configuration.md)
for saved policy and [Deployment](deployment.md#controls-and-recovery) for later controls.

## Prerequisites

- A Linux host with Docker Engine 28 or later and the Compose plugin. The Docker deployment
  runs every runner turn and verification command in a [sandbox](sandbox.md); the dashboard
  binds to loopback by default.
- A Codex or OpenCode provider login with the models you intend to route.
- A GitHub fine-grained token limited to the target repository, with the permissions needed
  to read checks and publish pull requests.
- The target project's build and test tools. Add missing tools to the sandbox image; see
  [Extend the sandbox image](sandbox.md#extend-the-sandbox-image).

Octomus does not create accounts or perform logins. Provider charges depend on your account;
session admissions are operating limits rather than a dollar budget. See [Cost and usage](cost.md).

## Deploy

Follow [Docker deployment](deployment.md#docker-deployment), then sign in the selected
runner and open the dashboard through the [SSH tunnel](deployment.md#private-access). The
setup script prints the operator token once; store it in a password manager.

For a dedicated host without Docker, follow [Dedicated VM without a sandbox](deployment.md#dedicated-vm-without-a-sandbox).

## First run

Open **Configuration** and provide:

- the repository and GitHub identity;
- explicit Codex or OpenCode routes for planning, review, execution and repair; and
- at least one meaningful verification command for execution.

Save, then run **Check connection**. In Docker this includes the sandbox self-test and
checks the saved origin, GitHub CLI login and runner catalogs; it does not make a model call
or prove push permission. Correct any reported route or account issue before continuing.

Choose **Run an audit** first if you want to inspect proposals without creating tasks. Choose
**Run once** to drain the queue, plan one cycle, execute accepted tasks and return to paused.
Use **Start continuous** only after reviewing the first results. Review each task's full
review and verification evidence and decide whether its pull request should be merged.

The [architecture](architecture.md) describes planning, recovery and publication contracts;
the [run evidence reference](run-evidence.md) explains the durable evidence attached to a
cycle.
