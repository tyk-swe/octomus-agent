# Architecture and operational contract

## Components

```text
SvelteKit static dashboard
          │ same-origin JSON API + bearer token
          ▼
Rust / Axum service
 ├─ SQLite state, event log, daily admission counter and ledger
 ├─ one scheduler, configured task concurrency, branch writer locks
 ├─ Codex app-server subprocesses over newline-delimited JSON RPC
 ├─ owned OpenCode HTTP/SSE servers on loopback
 └─ Git + GitHub CLI publication coordination
```

The dashboard polls authoritative Rust state and never schedules work itself. The production server serves the dashboard embedded in the executable; `--assets` explicitly overrides it with a filesystem build. No database service, message broker, Docker runtime, or sandbox backend is required.

## Planning

Each cycle records the remote default-branch revision, owned open PRs, their heads and accumulated scope, maintenance targets, task history, and separate read-only external PR summaries. The complete open inventory is paginated and fails explicitly if machine capture is incomplete. External context is ordered by PR number and limited to 100 entries, 200 title characters, 2,000 body characters, and 512 KiB serialized total; source/head references, omitted counts and truncation flags are recorded. External/fork PRs never become execution or maintenance targets. Old grounding records load with unknown external coverage. The orchestrator inspects the repository and PR diffs. Each discovery and proposal-review role has a separate clone and runner session. Planning sessions are instructed to inspect rather than mutate, and their worktree/HEAD must remain unchanged.

The standard cycle runs nine discovery agents (configurable from eight to ten), followed by two adversarial reviewers and orchestrator consolidation. The final result must account for every original proposal ID, with a decision and reason. Accepted work needs project evidence, benefit, scope, a self-contained prompt, a supported tier, and an eligible target. Unknown dependencies, dependency cycles, unordered/forked same-PR plans, duplicate accepted titles, and unowned targets are rejected by the core. Same-PR proposals require a unique dependency order; unrelated branches remain parallelizable.

Semantic value, overlapping ideas, and conflicting assessments are judged by the proposal reviewers and orchestrator; their results are recorded. The core cannot independently prove an idea's product value. Returning an empty task set is a successful idle cycle.

Maintenance is prioritized at the configured cadence for the main project and PRs above the size/age thresholds. It follows the same proposal and delivery gates as feature work.

Execution cycles commit the complete accepted queue and successful cycle in one
SQLite transaction. Incomplete discovery cannot become executable work after a restart.
Audits reuse planning and validation but persist decisions without dispatching tasks.
They require paused operation without active work and leave existing queued tasks
untouched. Resume/cycle requests conflict while an audit is active. Audit readiness
requires only the three planning-role routes, repository and authentication; normal
execution still requires all routes and meaningful verification. Cycles carry an
`audit` or `execution` mode, defaulting older records to `execution`. Both modes use
the same cycle numbering and maintenance cadence. Interrupted audits are recorded
and never automatically replayed. Audit clones remain subject to admission limits,
unsandboxed agent behavior and the normal cycle retention policy.

Before creating a cycle, the scheduler compares live remaining daily admissions with
`discovery_agents + REVIEWER_SLOTS.len() + 2`. Unaffordable manual starts change no
cycle or batch state; continuous operation waits without failed-cycle churn. Run once
rechecks after its queue drain and pauses instead of silently deferring planning.
This preflight does not replace or pre-reserve the individual atomic role admissions.

## Tasks and dependencies

A task snapshots its configuration, route, source revision, default-branch context, refined prompt and dependencies. Global admission limits come from current saved policy inside the reservation transaction. Separate attempt policy controls all timeouts, repair/no-progress limits and retry ceiling; explicit retry adopts current attempt limits and starts a fresh repair-round budget, while automatic recovery retains both. Task configuration, routes and verification commands remain immutable. Dependency-authorized source advancement retains the original grounding in the cycle. New workspaces use `.octomus/tasks/<task-id>/workspace` and are cloned before a runner session is created. Existing saved workspace paths, including legacy Codex thread-based paths, are retained. Native runner session IDs are recorded separately and never used to name new directories. Each task uses an independent Git clone. Review and repair threads work in that same task clone.

Independent tasks can run concurrently. Existing PR branch writers are serialized. Dependent tasks on the same existing PR wait for their prerequisites to publish; the source revision is advanced only to a recorded prerequisite output and its ancestry is checked. External branch movement blocks stale work.

Code-dependent default-branch proposals must be consolidated into a cohesive task or deferred until the prerequisite PR has merged. Merely publishing a separate PR does not make its code available on the default branch. Octomus does not auto-merge or implicitly create stacked PRs.

New-PR dispatch also observes live `max_open_prs` (default five, including legacy
configurations). Capacity counts complete observed owned-open PR identities plus
unrepresented durable task reservations, not dashboard/history windows. Fresh
asynchronous inventories authorize one admission batch; failures, obsolete responses
and unknown state do not authorize new work. Existing-PR and reserved queued work
remain selectable behind a large new-PR queue. Checkpoint publication replay retains
its strict Git/publication checks without requiring another new-PR slot. Observed
closure/merge releases capacity; a terminal task's admitted branch is also settled by
remote re-observation, so a cancelled task whose branch carries no matching PR
releases its reservation instead of stranding the slot. Lowering the ceiling neither
closes PRs nor cancels in-flight work. External actors can still change remote
backlog after observation.

## Review, verification and delivery

The executor's changes are committed locally. Review uses a fixed comparison base: the original default revision for new work, or the merge base with the default branch for existing PR work. Every review round examines the entire accumulated diff against that base.

A reviewer is always a fresh session on its configured runner. A repair thread starts separately with the task’s saved configurable repair route (shipped with `medium` effort and no model, so it must be configured before a run is ready) and is resumed for subsequent repair turns. Interrupted, failed, missing, malformed, or explicitly incomplete results never count as clean reviews. Rounds and their revisions are recorded.

Configured shell verification runs after a completed clean review. Every command must pass on exactly the reviewed revision. Worktree cleanliness and HEAD are checked before the first command and after each command; a command that changes tracked state is recorded as failed evidence, stops the remaining commands and blocks the task, so no success is attributed to a revision the command did not actually run against. Successful commands keep bounded stdout and stderr as evidence. Commands run from the assigned workspace with Bash `pipefail`. Net-empty changes are not publishable, even if an executor made commits.

Publication requires recorded clean review and successful verification evidence for the output commit. Before writing, the core verifies repository identity, PR ownership/open state, source/default revisions, worktree cleanliness and commit ancestry. It pushes only the assigned prefixed branch, using an exact Git lease to guard the check/push race. The push destination comes from the validated configured checkout, not an agent-editable task remote. No default-branch push is generated.

GitHub is the MVP provider. Existing PRs require the configured prefix, matching head repository, and an Octomus task marker in their body. Prefixed PRs without that marker contribute planning context but are not writable targets. New branch suffixes use unique task IDs. PR bodies record the objective, scope, benefit, verification, implementation summary and publication marker. Follow-ups post their record as a comment on the existing PR rather than rewriting its description, so earlier delivery notes and maintainer edits are never replaced; the task marker attached there makes the append idempotent.

Creation resolves the exact returned PR number, then creation and updates share a final repository/ownership/branch/base/head/task-marker validator. A mismatched result remains blocked with its publication checkpoint. Before retrying a publication, the service searches all matching PR states, including closed and merged PRs; ambiguous branch associations fail closed. A matching task marker — in the description or a follow-up comment — and output head confirm previously completed delivery without another push or duplicate PR. Changed remote state is surfaced for reconciliation.

## Explicit baseline verification

The authenticated baseline action runs only by operator request while paused and idle.
It compares the expected saved configuration, records its fingerprint and the remote
default SHA, then runs saved commands in a disposable clone with the ordinary process,
timeout, output and worktree-integrity primitives. It creates no task, cycle, PR or model
admission. Results are separate baseline records, never task publication evidence.
Output is capped at 16 KiB per command and 1 MiB per check with explicit truncation;
configuration and observed revision freshness remain distinct from a recorded pass.
Cancellation, deadlines and restart interruption are terminal, never automatically
replayed. Owned clones are cleaned up safely; cleanup failures are recorded and retried.

## Runtime and trust

[Codex app-server documentation](https://developers.openai.com/codex/app-server) and the bindings generated by installed CLI 0.153.4 informed the integration. The client uses `initialize`, `model/list`, `account/read`, `thread/start`, `thread/resume`, `turn/start` and `turn/interrupt`, correlating completion events with thread and turn IDs. Structured schemas constrain proposal/review responses. A role uses `approvalPolicy: never` and `danger-full-access`; returned model, effort and sandbox settings must match.

OpenCode 1.18.30 is the HTTP/SSE protocol baseline. Each task owns lazily started
servers for its selected backends; planning invocations own separate clients.
OpenCode binds to loopback with per-process Basic authentication and uses existing
service-user provider configuration. The adapter supplies temporary worker policy,
disables project configuration overrides, automatic sharing, updates, helper
agents, automatic compaction, formatters and LSP processes. It checks session
workspace/model/variant/permissions on creation and resumption, correlates message
identities, and validates native structured results before accepting evidence.
See [model routing](model-routing.md) for setup and API details.

Unexpected interactive requests fail visibly. RPCs, turns, whole tasks, command output and admission counts are bounded. No model turns are started by the automated test fixtures or dashboard's model-catalog check.

Repository content and agent outputs are never deserialized into operating configuration. API access requires the operator token, which is excluded from child-process environment variables. JSON is redacted before being returned to the dashboard, and rendered as text rather than trusted HTML.

Because execution is deliberately unsandboxed, prompts and application policy are **not a host security boundary**. A process with the service user's permissions can exercise those permissions. Use the dedicated-host deployment model described in [deployment](deployment.md).

SQLite uses full synchronous writes and WAL. Only one service may hold the state-directory lock. Restart recovery preserves workspace/session identities and retries initialized interrupted tasks within the retry limit. A deployment supervisor must terminate old processes before recovery; the supplied systemd unit uses control-group termination.

## Usage records and upgrades

Every budget reservation commits its UTC day counter and admission metadata in one transaction. Failed starts still consume reservations; reused repair threads consume another admission for each turn. The additive admissions table is created on startup. Older daily counts remain intact and are reported as unattributed, without invented historical ledger entries. Configuration and task snapshots without `repair_route` retain the shipped medium-effort Codex route through deserialization defaults. Routes without a backend load as Codex, including historical session/admission records. Missing `opencode_binary` defaults to `opencode`; no database rewrite or workspace migration is needed. Newly saved routes carry their backend and any OpenCode provider/variant.

`--usage-report` opens an existing database read-only and reads one transaction snapshot without taking the service lock or initializing/migrating state. It exports metadata rather than raw prompts/transcripts or credentials. Admissions are not provider charges; see [cost methodology](cost.md). Keep a full state backup before upgrading; older binaries do not understand newly saved configuration fields.

`GET /api/cycles/{id}/evidence` and `--export-run` assemble `RunEvidenceV1`, a read-only account of what one cycle and its tasks saved: positional reviewer verdicts, the latest review round and the latest result per required command, joined by cycle and proposal identity, with explicit gaps and limitations and without prompts, transcripts or command output. See [run evidence](run-evidence.md).

For deployment trust boundaries, authentication backoff and redaction limitations,
see the [threat model](threat-model.md).

## Operating modes and recovery

`Paused`, `RunOnce` and `Continuous` are durable modes. One-shot membership is
recorded separately from mutable task retry state: a later retry cannot erase a
batch failure or silently join the batch. The successful cycle, complete queue,
decision memory, lineage and one-shot phase change commit together. Interrupted
planning is never replayed automatically. Typed blocked reasons determine valid
operator actions; stale context requires supersession and fresh discovery.

## Bounded observation and storage

SQLite JSON records remain canonical. Transactional projection triggers maintain
indexed task/cycle/PR summaries and proposal listings. Dashboard queries limit rows
before Rust deserialization, read counts and summaries in one transaction, and
fetch full evidence only on demand. Attention queries use a dedicated partial
index, including when no unresolved rows match. Scheduler/recovery queries select
operational states and resolve dependencies by identity.

Housekeeping runs independently of operation mode. Completed planning-role clones
are disposable after evidence and source checks; unresolved work remains retained
until explicit resolution. Archive/discard changes workspace lifecycle without
deleting database evidence. Application storage is measured separately from runner
transcripts. Storage admission remains a pre-turn check, not a filesystem quota.

Diagnostic subprocess output retains a bounded 256 KiB preview and truncation
flags. Machine stdout is complete up to 16 MiB or returns `OutputTooLarge`; invalid
UTF-8 also fails explicitly. Git/GitHub machine consumers never parse a diagnostic
truncation marker. All captures retain timeout, draining and process-group ownership.

State-snapshot cost was measured on 2026-09-10 at `5314f2d` with
`cargo test --locked --test history_scale -- --ignored --nocapture`. The fixture stores
4 KiB prompts in 1,000, 10,000 and 100,000 historical task records; each measurement
includes 20 dashboard snapshots using the debug build.

| Historical tasks | State JSON bytes | Median query time | p95 query time | Peak additional Rust allocations |
| --- | ---: | ---: | ---: | ---: |
| 1,000 | 95,227 | 14.27 ms | 18.04 ms | 1,344,963 bytes |
| 10,000 | 95,528 | 13.99 ms | 23.44 ms | 1,345,563 bytes |
| 100,000 | 95,829 | 16.50 ms | 23.57 ms | 1,346,163 bytes |

These timings are environment-dependent. Allocation measurements cover the Rust
allocator, not SQLite's C allocations or total process RSS.

## Decision memory and outcomes

A bounded selection of repository-scoped decisions records problem identity,
relevant paths, rationale, source context and reconsideration time. Matching
problem identities are reused across wording changes. Relevant blob changes or
30 elapsed days permit reconsideration; unresolved tasks continue to suppress
duplication. Audit acceptance remains a recommendation and does not suppress
subsequent execution. Explicit supersession requests receive their own fresh
accepted/rejected/deferred decision.

After the first idle cycle, successive idle outcomes double the interval, capped
at 24 hours without shortening a longer configured interval. Lightweight repository
and PR-head observation every five minutes shortens extended idle backoff on changed context while preserving the ordinary configured cadence.
Manual one-shot runs bypass backoff. PR outcomes are observations distinct from
task delivery; known follow-up outputs are recognized when checking external head
movement. The service never infers provider charges or merges a PR.
