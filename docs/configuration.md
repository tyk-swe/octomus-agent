# Configuration

Your saved configuration sets the repository, model routes, verification policy, and
operating limits. Configure it from the private dashboard while the service is paused and
has no active tasks or cycle. For the first-run sequence and host setup, see [Getting
started](getting-started.md) and [Deployment](deployment.md).

Loading a model catalog does not save choices or start a model call. A connection check
validates prerequisites; it does not prove push permission or model inference. Editing
saved configuration invalidates the previous connection check.

## Display-safe values and saved revisions

The dashboard loads a display-safe view of the saved configuration. Values matching
known credential patterns or token/secret/password/API-key environment variables are
redacted, and very long values are shortened; each affected field is marked. A marked
field is locked as a preview: saving other fields preserves its stored value, and the
preview is never written back. To change it, use the field's **Replace** action and
re-enter the complete value — hidden originals are never merged back.

Every save pins the revision of the saved configuration it was made from and replaces
only the fields you changed. A save based on a superseded revision conflicts instead
of overwriting newer values. Connection checks and baseline checks report the exact
saved revision they covered.

## Repository and routes

Set an absolute, persistent repository path and a GitHub identity such as `OWNER/REPOSITORY`.
In the Docker deployment both are fixed by `OCTOMUS_GITHUB_REPO` and shown read-only: the
control plane owns its checkout in the data volume, and the operator token cannot point
its git anywhere else. Unsandboxed, the service user needs access to that checkout and the
repository's build tools.
Choose an owned branch prefix, such as the default `octomus/`, to identify branches the agent may publish.
Repository identity and branch policy cannot change while unresolved tasks exist.

Select routes for discovery, proposal review, orchestration, code review, execution tiers,
and repair. Codex routes name a model and reasoning effort. OpenCode routes name a
provider and model, with an optional supported variant. See [model routing](model-routing.md)
for complete examples. Unsupported routes fail visibly instead of silently substituting
another model.

## Verification commands

Use meaningful checks for the target project, one shell command per line. At least one
command is required for execution. An audit can run without verification commands.

Every configured command must pass on the reviewed revision before publication. A failed
check stays visible and can block delivery. Each verification run gets a fresh clone of
exactly the reviewed commit: dependencies, caches and build output an agent left in the
task work tree are not there, so commands must install what they need (for example
`npm ci && npm test`), as the clean-baseline check always required. In the Docker deployment each command runs in
a fresh sandbox with the sandbox image's tools, the task's work tree and a home shared only
by the commands of one verification run; it reaches only the hosts in
`OCTOMUS_EGRESS_BUILD_HOSTS`. Unsandboxed, commands run with the service user's permissions
on the dedicated host.

A command must leave the workspace as it found it. One that changes tracked files, moves
HEAD (by committing, for example), or leaves a new file that the repository does not
git-ignore (a coverage report or a cache, for example) fails verification and blocks the
task, so ignore such artifacts in the repository's `.gitignore`. Such a command fails a
clean-baseline check the same way.

The optional **Check clean baseline** runs the saved commands against the remote default
revision before model work. It does not verify any later task's changes.

## Host settings

Sandbox mode, sandbox limits, the sandbox image and runtime, the egress allowlists and the
pinned repository are deployment settings in `.env`, not saved policy: the dashboard shows
them read-only and no API call changes them. [env.example](../deploy/docker/env.example)
documents each one; the [sandbox guide](sandbox.md) explains them.

## Operating limits

The [configuration example](configuration.example.json) contains every saved policy field
and its shipped default. These are operating limits, not a recommended starting size for
every repository. The [field reference](#field-reference) lists what each field accepts.

For a conservative first execution, use one concurrent task, one task per cycle, and a
21,600-second interval. This is a starting profile, not a measured performance claim.
With nine discovery agents, a complete planning pass normally needs 13 session admissions.

Session admissions do not cap provider dollar spend. Workspace limits are checked before
launching model work; active commands can grow beyond them. See [usage and costs](cost.md)
and [deployment](deployment.md) for budgeting, host limits, and retention behavior.

## Field reference

Saving checks every field and refuses the whole save when one is out of range, naming
that setting and the range it accepts. Counts are whole numbers. `repository`,
`github_repo`, complete routes and at least one verification command are required only
when a check or run needs them, so an incomplete draft can still be saved.

### Repository and branches

| Field | Shipped default | Accepted values | What it controls |
| --- | --- | --- | --- |
| `repository` | empty | Absolute path of a Git checkout | The persistent target checkout. |
| `github_repo` | empty | `OWNER/NAME` of letters, digits, `-`, `_` and `.` | The GitHub repository that receives PRs. |
| `default_branch` | `main` | A valid branch name outside the owned prefix | The branch new work starts from and targets. |
| `branch_prefix` | `octomus/` | A valid branch path ending in `/`; must neither include the default branch nor be nested beneath it | Branches Octomus owns and may publish. |

### Runners and routes

| Field | Shipped default | Accepted values | What it controls |
| --- | --- | --- | --- |
| `codex_binary` | `codex` | Non-blank executable path, at most 4,096 bytes, no control characters | The Codex CLI to run. |
| `opencode_binary` | `opencode` | As for `codex_binary` | The OpenCode CLI to run. |
| `roles` | Codex routes without model or effort | Exactly `orchestrator`, `discovery`, `proposal_reviewer` and `code_reviewer` | Planning and code review routes. |
| `tiers` | Codex routes with an effort and no model | Exactly `XS`, `S`, `M`, `L` and `XL` | Execution routes by task size. |
| `repair_route` | Codex, `medium` effort, no model | One route | The persistent repair thread's route. |

[Model routing](model-routing.md) describes the route fields. An audit needs the
orchestrator, discovery and proposal reviewer routes; execution needs every route.

### Planning

| Field | Shipped default | Accepted values | What it controls |
| --- | --- | --- | --- |
| `categories` | All nine | A non-empty subset of `features`, `correctness`, `performance`, `ux-dx`, `refactoring`, `simplification`, `tests`, `dependencies` and `documentation` | Categories accepted work may use. |
| `discovery_agents` | 9 | 8–10 | Agents exploring complementary repository areas. |
| `max_tasks_per_cycle` | 5 | 1–20 | Accepted tasks per execution cycle. |
| `cycle_interval_seconds` | 1,800 | 30–604,800 | Interval between discovery cycles. |
| `maintenance_every_cycles` | 3 | 1–10,000 | Maintenance is due on cycle numbers divisible by this. |
| `large_pr_lines` | 1,000 | Any count | Owned open PRs with at least this many changed lines become maintenance targets; 0 marks every one. |
| `long_lived_pr_days` | 7 | Any count | Owned open PRs at least this many days old become maintenance targets; 0 marks every one. |

### Delivery

| Field | Shipped default | Accepted values | What it controls |
| --- | --- | --- | --- |
| `delivery_mode` | `standard` | `standard` or `maintenance` | Standard publishes reviewed pull requests for manual merge. Maintenance excludes the `features` category and allows small reviewed deliveries to squash-merge themselves when fresh remote checks and protections allow it. |
| `auto_merge_max_lines` | 500 | 1–10,000 | Full-PR changed lines a maintenance delivery may merge automatically. |
| `auto_merge_max_files` | 10 | 1–100 | Full-PR changed files a maintenance delivery may merge automatically. |
| `auto_merge_excluded_paths` | None | Up to 100 repository-relative names or subtree prefixes, each at most 4,096 bytes | Additional paths whose changes keep a maintenance delivery manual. CI/workflow rules, security policies, deployment settings and migrations are always manual. |

Maintenance mode is opt-in and delivery mode changes are refused while unresolved
unarchived tasks remain. Every publication still requires a clean full-diff review and
passing verification; the maintenance review additionally classifies the whole
accumulated change. The stricter of the task's saved limits and the live limits applies.

### Verification and attempts

| Field | Shipped default | Accepted values | What it controls |
| --- | --- | --- | --- |
| `verification_commands` | None | Each non-blank and at most 4,096 bytes; execution needs at least one | Shell checks that must pass on the reviewed revision. |
| `max_repair_rounds` | 4 | 1–20 | Repair rounds per attempt before unresolved work blocks. |
| `max_no_progress_rounds` | 2 | At least 1 | Consecutive repair rounds that leave the reviewed revision unchanged before the task blocks. |
| `max_retries` | 2 | 0–10 | Further attempts per task, by operator retry or restart recovery. |
| `session_timeout_seconds` | 1,800 | 10–604,800 | Longest agent turn. |
| `task_timeout_seconds` | 14,400 | From the session timeout to 604,800 | Whole task: execution, review, repair, verification and delivery; also each clean-baseline check. |
| `command_timeout_seconds` | 600 | 1–604,800 | Each Git, GitHub CLI and verification command. |

A task snapshots these settings when accepted. An explicit retry adopts the current
repair, no-progress, retry and timeout limits; a task's verification commands never change.

### Capacity

| Field | Shipped default | Accepted values | What it controls |
| --- | --- | --- | --- |
| `execution_concurrency` | 2 | 1–8 | Simultaneous execution tasks. |
| `max_sessions_per_day` | 150 | 1–1,000,000 | Agent turn admissions per UTC day, including repair turns. |
| `max_open_prs` | 5 | 1–1,000 | Owned open PRs plus admitted deliveries before new-PR work waits. |
| `max_workspace_bytes` | 20,000,000,000 | 1,000,000–10^15 | Data-directory size, measured before each model turn and baseline check, at which they are refused. |

### Storage and retention

| Field | Shipped default | Accepted values | What it controls |
| --- | --- | --- | --- |
| `runner_storage_paths` | None | Only `codex` and `opencode` keys, each an absolute path | Runner transcript directories measured for display. |
| `retain_completed_days` | 14 | 1–36,500 | Days after publication, completion or archiving before a workspace is removed. |
| `retain_events` | 10,000 | 100–100,000 | Newest activity events kept. |

Housekeeping measures `runner_storage_paths` every 15 minutes and reports them
separately from `max_workspace_bytes`, which covers only the data directory. Octomus
never deletes runner storage; configure its retention on the host.

[Download the default configuration JSON](configuration.example.json) to inspect the full
shape. Model IDs and some role settings are deliberately empty until you choose routes
available to your provider account. The file is a reference, not a ready-to-run setup.

For command-line options, environment variables, the optional attention webhook and
planning context, see [deployment and operations](deployment.md#cli) and
[architecture](architecture.md#planning). Keep account credentials in the service user's
protected environment and runner settings, never in the repository.
