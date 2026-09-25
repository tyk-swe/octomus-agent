# Configuration

Your saved configuration sets the repository, model routes, verification policy, and
operating limits. Configure Octomus from the private dashboard while it is paused and
has no active tasks or cycle. Start with the [installation guide](getting-started.md)
if you have not connected a host yet.

## Enter, save, check, then run

1. **Enter** the repository path, GitHub repository, default branch, model routes, and checks.
2. **Save configuration** to persist the values to the service.
3. **Check connection** against the saved configuration. Use **Check audit connection**
   when you only need planning prerequisites.
4. **Run an audit** from Overview to inspect recommendations before choosing execution.

Loading a model catalog does not save your choices or start a model call. A connection
check validates prerequisites; it does not prove push permission or model inference.
Editing saved configuration invalidates the previous connection check.

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
The service user needs access to that checkout and the repository's build tools.
Choose an owned branch prefix, such as `tyk/`, to identify branches the agent may publish.
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
check stays visible and can block delivery. Commands run with the service user's permissions
on the dedicated host.

The optional **Check clean baseline** runs the saved commands against the remote default
revision before model work. It does not verify any later task's changes.

## Operating limits

The [configuration example](configuration.example.json) contains every saved policy field
and its shipped default. These are operating limits, not a recommended starting size for
every repository.

| Setting | Shipped default | What it controls |
| --- | --- | --- |
| `discovery_agents` | 9 | Agents exploring complementary repository areas. |
| `execution_concurrency` | 2 | Simultaneous execution tasks. |
| `cycle_interval_seconds` | 1,800 | Interval between discovery cycles. |
| `max_tasks_per_cycle` | 5 | Accepted tasks per execution cycle. |
| `max_open_prs` | 5 | Owned open PRs plus admitted deliveries before new-PR work waits. |
| `max_sessions_per_day` | 150 | Agent turn admissions per UTC day, including repair turns. |
| `max_repair_rounds` | 4 | Bounded repair attempts for a task. |
| `max_no_progress_rounds` | 2 | Rounds allowed without progress. |
| `retain_completed_days` | 14 | Retention for eligible completed workspaces. |

For a conservative first execution, use one concurrent task, one task per cycle, and a
21,600-second interval. This is a starting profile, not a measured performance claim.
With nine discovery agents, a complete planning pass normally needs 13 session admissions.

Session admissions do not cap provider dollar spend. Workspace limits are checked before
launching model work; active commands can grow beyond them. See [usage and costs](cost.md)
and [deployment](deployment.md) for budgeting, host limits, and retention behavior.

## Configuration reference

[Download the default configuration JSON](configuration.example.json) to inspect the full
shape. Model IDs and some role settings are deliberately empty until you choose routes
available to your provider account. The file is a reference, not a ready-to-run setup.

For command-line options, environment variables, and the optional attention webhook, see
[deployment and operations](deployment.md#cli). Keep account credentials in the service
user's protected environment and runner settings, never in the repository.
