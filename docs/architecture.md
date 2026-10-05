# Architecture and operational contract

## Components

```text
SvelteKit static dashboard
          │ same-origin JSON API + bearer token
          ▼
Go control plane (net/http)
 ├─ SQLite state, event log, daily admission counter and ledger
 ├─ one scheduler, configured task concurrency, branch writer locks
 ├─ Git + GitHub CLI publication coordination on trusted git metadata
 └─ sandbox backend ── unix socket ──▶ sandbox broker ── Docker Engine API
                                          │
                  one container per agent turn or verification command
                   ├─ Codex app-server over newline-delimited JSON RPC on stdio
                   ├─ OpenCode HTTP/SSE, relayed as HTTP/2 over stdio
                   └─ bash -o pipefail verification commands
                  internal networks ──▶ egress gateway (allowlisted HTTPS only)
```

The dashboard polls authoritative service state and never schedules work itself. The production server serves the dashboard embedded in the executable; `--assets` explicitly overrides it with a filesystem build. No database service or message broker is required. The default Docker deployment runs the control plane, the [sandbox](sandbox.md) broker and the egress gateway from one image; `--sandbox off` runs runners and verification commands as host processes instead.

## Planning

Each cycle records the remote default-branch revision, owned open PRs, their heads and accumulated scope, maintenance targets, task history, and separate read-only external PR summaries. The complete open inventory is paginated and fails explicitly if machine capture is incomplete. External context is ordered by PR number and limited to 100 entries, 200 title characters, 2,000 body characters, and 512 KiB serialized total; source/head references, omitted counts and truncation flags are recorded. External/fork PRs never become execution or maintenance targets. The orchestrator inspects the repository and PR diffs. Each discovery and proposal-review role has a separate clone and runner session. Planning sessions are instructed to inspect rather than mutate, and their worktree/HEAD must remain unchanged.

After an orchestrator grounding turn, the standard cycle runs nine discovery agents (configurable from eight to ten), followed by two adversarial reviewers and orchestrator consolidation. The final result must account for every original proposal ID, with a decision and reason. Accepted work needs project evidence, benefit, scope, a self-contained prompt, a supported tier, an enabled category, and an eligible target. Unknown dependencies, dependency cycles, unordered/forked same-PR plans, accepted proposals that repeat each other's work on one target (the same title or problem identity), accepted work that repeats a recorded task on the same repository and target that is neither cancelled nor archived, and unowned targets are rejected by the core; a rejected plan fails the cycle and dispatches nothing. Same-PR proposals require a unique dependency order; unrelated branches remain parallelizable.

Semantic value, overlapping ideas, and conflicting assessments are judged by the proposal reviewers and orchestrator; their results are recorded. The core cannot independently prove an idea's product value. Returning an empty task set is a successful idle cycle.

Maintenance is prioritized at the configured cadence for the main project and PRs above the size/age thresholds. It follows the same proposal and delivery gates as feature work.

Execution cycles commit the complete accepted queue and successful cycle in one
SQLite transaction. Incomplete discovery cannot become executable work after a restart.
Audits reuse planning and validation but persist decisions without dispatching tasks.
They require paused operation without active work and leave existing queued tasks
untouched. Resume/cycle requests conflict while an audit is active. Audit readiness
requires only the three planning-role routes, repository and authentication; normal
execution still requires all routes and meaningful verification. Cycles record an
`audit` or `execution` mode. Both modes use the same cycle numbering and maintenance
cadence. A graceful shutdown or a crash during planning records the cycle as
interrupted; interrupted audits are never automatically replayed. Audit clones remain
subject to admission limits, the deployment's sandboxing and the normal cycle retention
policy.

Before creating a cycle, the scheduler compares live remaining daily admissions with
`discovery_agents + 4` (one grounding turn, two proposal reviewers and one
consolidation turn besides discovery). Unaffordable manual starts change no
cycle or batch state; continuous operation waits without failed-cycle churn. Run once
rechecks after its queue drain and pauses instead of silently deferring planning.
This preflight does not replace or pre-reserve the individual atomic role admissions.

## Tasks and dependencies

A task snapshots its configuration, route, source revision, default-branch context, refined prompt and dependencies. Global admission limits come from current saved policy inside the reservation transaction. Separate attempt policy controls all timeouts, repair/no-progress limits and retry ceiling; explicit retry adopts current attempt limits and starts a fresh repair-round budget, while automatic recovery retains both. Task configuration, routes and verification commands remain immutable. Dependency-authorized source advancement retains the original grounding in the cycle. Workspaces use `.octomus/tasks/<task-id>/workspace`, with the clone's git metadata beside them in `repo.git`, where sandboxes can read but not write it; they are cloned before a runner session is created. Native runner session IDs are recorded separately and never used to name directories. Each task uses an independent Git clone. Review and repair threads work in that same task clone.

Completed repairs keep a durable progress checkpoint. Recovery cannot reset the consecutive no-progress limit or charge the same completed repair twice. An explicit retry clears this checkpoint along with its repair-round budget. Existing version-7 records without a checkpoint start tracking progress at their next completed repair.

The repair-round budget counts completed repair turns separately from review rounds.
The completed repair session, its counter, and its progress are saved together.
If that save fails, the supervisor retains this accounting in its subsequent
failure or interruption checkpoint when storage accepts the write.
Recovery resumes unfinished review or verification with a fresh full-diff review
and, if clean, verification, including after the final allowed repair; interrupted
verification does not spend a repair round.
Older version-7 tasks without the repair counter initialize it from their
conservative review-based budget estimate, then count completed repairs directly.
An explicit retry starts the counter at zero for the fresh attempt.

Independent tasks can run concurrently. Existing PR branch writers are serialized. Dependent tasks on the same existing PR wait for their prerequisites to publish; the source revision is advanced only to a recorded prerequisite output and its ancestry is checked. External branch movement blocks stale work.

Code-dependent default-branch proposals must be consolidated into a cohesive task or deferred until the prerequisite PR has merged. Merely publishing a separate PR does not make its code available on the default branch. Octomus does not auto-merge or implicitly create stacked PRs.

New-PR dispatch also observes live `max_open_prs` (default five). Capacity counts complete observed owned-open PR identities plus
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

Configured shell verification runs after a completed clean review, in a fresh clone of exactly the reviewed revision (removed afterwards), each command in a fresh sandbox. Ignored files, caches and build output a session left in the task work tree never reach verification, so commands install their own dependencies. Every command must pass on exactly the reviewed revision. Worktree cleanliness and HEAD are checked before the first command and after each command; a command that leaves the worktree unclean (modified or staged files, or new untracked files that are not git-ignored) or moves HEAD is recorded as failed evidence, stops the remaining commands and blocks the task, so no success is attributed to a revision the command did not actually run against. Verification artifacts such as coverage reports and caches must therefore be git-ignored by the repository. Each command's evidence holds its stdout, then any stderr in a `[stderr]` section, then the exit status on failure, secret-scrubbed and bounded to 16 KiB: each stream keeps its end, even past the diagnostic capture limit, stderr may use half of the bound however long stdout is, and `[output truncated]` marks any cut. A failed workspace state check adds its note, itself capped at 4 KiB, after the output; the bound leaves room for it. Repair turns receive the same text for failed commands. Commands run from the assigned workspace with Bash `pipefail`. Net-empty changes are not publishable, even if an executor made commits.

Publication requires recorded clean review and successful verification evidence for the output commit. Before writing, the core verifies repository identity, PR ownership/open state, source/default revisions, worktree cleanliness and commit ancestry. It pushes only the assigned prefixed branch, using an exact Git lease to guard the check/push race. The push destination comes from the validated configured checkout, not an agent-editable task remote. No default-branch push is generated.

GitHub is the MVP provider. Existing PRs require the configured prefix, matching head repository, and an Octomus task marker in their body. Prefixed PRs without that marker contribute planning context but are not writable targets. New branch suffixes use unique task IDs. PR bodies record the objective, scope, benefit, verification, implementation summary and publication marker. Follow-ups post their record as a comment on the existing PR rather than rewriting its description, so earlier delivery notes and maintainer edits are never replaced; the task marker attached there makes the append idempotent.

Titles, descriptions and follow-up comments are prepared for public delivery before the first new outbound write: proposal text, command descriptions and session summaries pass through the non-truncating secret scrubber, and the prepared text is validated — exact task marker and reviewed commit intact, within the remote's title and body limits. Metadata that cannot satisfy both is refused without writing; nothing is silently truncated, and canonical task records keep their private values. The commit message Octomus generates for the executor's changes passes through the same scrubber, because it is published with the branch.

Creation resolves the exact returned PR number, then creation and updates share a final repository/ownership/branch/base/head/task-marker validator. A mismatched result remains blocked with its publication checkpoint. Before retrying a publication, the service searches all matching PR states, including closed and merged PRs; ambiguous branch associations fail closed. A matching task marker — in the description or a follow-up comment — and output head confirm previously completed delivery without another push or duplicate PR. Changed remote state is surfaced for reconciliation.

## Explicit baseline verification

The authenticated baseline action runs only by operator request while paused and idle.
Admission pins the canonical configuration revision the operator loaded; a stale or
display-transformed value conflicts before any clone is made. The record keeps the
canonical snapshot, its fingerprint and the remote default SHA, then runs saved
commands in a disposable clone with the ordinary process,
timeout, output and worktree-integrity primitives. It creates no task, cycle, PR or model
admission. Results are separate baseline records, never task publication evidence.
Output is capped at 16 KiB per command and 1 MiB per check with explicit truncation;
configuration and observed revision freshness remain distinct from a recorded pass.
A successful remote read that finds the default branch missing clears revision
freshness; an older read cannot restore it, and the recorded pass is preserved.
Cancellation, deadlines and restart interruption are terminal, never automatically
replayed. Owned clones are cleaned up safely. A failed cleanup is retried on later
housekeeping passes; the service records a new or changed failure at once and an
unchanged one at most once a day per service process.

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

Codex's own sandbox stays off because the container is the boundary: in the Docker deployment each runner starts in a fresh [sandbox](sandbox.md) bound to one owned root, stopped after each turn, with no GitHub credential and egress only through the allowlisting gateway. Prompts and application policy are **not a security boundary** on their own. With `--sandbox off` a process with the service user's permissions can exercise those permissions; use the dedicated-host model described in [deployment](deployment.md#dedicated-vm-without-a-sandbox).

SQLite uses full synchronous writes and WAL. Only one service may hold the state-directory lock. Restart recovery preserves workspace/session identities and retries initialized interrupted tasks within the retry limit. A graceful stop cancels running work but leaves initialized in-flight tasks to the same recovery as a crash. A deployment supervisor must terminate old processes before recovery: in the Docker deployment the broker kills and removes every sandbox whose stream closes and sweeps leftovers when it starts; the supplied systemd unit uses control-group termination.

## Usage records and state

Every budget reservation commits its UTC day counter and admission metadata in one transaction. Failed starts still consume reservations; reused repair threads consume another admission for each turn. State starts at schema version 7 (v0.1.0). Later releases upgrade it at startup through ordered forward-only migrations after a verified backup. The version check runs before any schema or journal change, and pre-release or newer databases are refused untouched. Route snapshots record the selected backend, model, effort or variant.

`--usage-report` opens the database read-only at this release's schema version and reads one transaction snapshot without taking the service lock or initializing state. It exports metadata rather than raw prompts/transcripts or credentials. Admissions are not provider charges; see [cost methodology](cost.md). Keep a full state backup before replacing a binary.

`GET /api/cycles/{id}/evidence` and `--export-run` assemble `RunEvidenceV1`, a read-only account of what one cycle and its tasks saved: positional reviewer verdicts, the latest review round and the latest result per required command, joined by cycle and proposal identity, with explicit gaps and limitations and without prompts, transcripts or command output. See [run evidence](run-evidence.md).

For deployment trust boundaries, authentication backoff and redaction limitations,
see the [threat model](threat-model.md).

## Operating modes and recovery

`paused`, `run_once` and `continuous` are durable modes. One-shot membership is
recorded separately from mutable task retry state: a later retry cannot erase a
batch failure or silently join the batch. The successful cycle, complete queue,
decision memory, lineage and one-shot phase change commit together. Interrupted
planning is never replayed automatically. Typed blocked reasons determine valid
operator actions; stale context requires supersession and fresh discovery.
The scheduler marks a running cycle interrupted once its planning worker has
exited, including after storage recovers from refused terminal writes, so its
retained evidence can be inspected and archived without restarting the service.
Continuous operation keeps its saved schedule and starts a fresh cycle when due,
with new grounding and sessions rather than resuming the interrupted plan.
While a background recovery write is refused, the scheduler records a recovery
error and starts no work in that pass. It keeps the saved mode and schedule for
the next recovery attempt. During an unresolved episode, each of the first eight
successfully recorded redacted causes appears once, followed by at most one
notice that further causes are suppressed. This bounds recovery activity even
when causes alternate or keep changing. Refused activity writes remain retryable;
the current redacted cause remains visible until all maintenance recovery succeeds,
even if its activity entry cannot be saved or further entries are suppressed.
Likewise, a saved publication checkpoint whose worker has exited becomes blocked
as `publication_uncertain` once storage accepts the update. This preserves its
evidence and PR reservation for explicit reconciliation without replaying delivery.

## Bounded observation and storage

SQLite JSON records remain canonical. Transactional projection triggers maintain
indexed task/cycle/PR summaries and proposal listings. Dashboard queries limit rows
before JSON decoding, read counts and summaries in one transaction, and
fetch full evidence only on demand. Attention queries use a dedicated partial
index, including when no unresolved rows match. Scheduler/recovery queries select
operational states and resolve dependencies by identity.

Housekeeping runs independently of operation mode. Completed planning-role clones
are disposable after evidence and source checks; unresolved work remains retained
until explicit resolution. Archive/discard changes workspace lifecycle without
deleting database evidence; each happens once per task or cycle, and repeating it
conflicts. Application storage is measured separately from runner
transcripts. Storage admission remains a pre-turn check, not a filesystem quota.
Measurement walks relative to each directory's descriptor, so nesting depth
never lengthens a path it resolves, and it never follows a symlink. A directory
below the measured root that denies access (or is swapped for a symlink or file
during the walk), or that sits 2,112 or more levels below the data directory
(beyond git's default `core.maxTreeDepth` of 2,048, so only a chain a sandbox built
reaches it),
holds unknown bytes: admission refuses its owning task, planning cycle or
baseline with the storage limit until that work is resolved, while other owners'
admissions count only what was measured. Such a directory outside those owned
roots refuses every admission until it is made readable or moved out of the data
directory; a data directory at a filesystem's root has a `lost+found` the service
usually cannot read, so use a subdirectory. Any other filesystem error except a
vanished path, and failing to read the measured root itself, still fails the
measurement, and measurement never changes permissions.

Diagnostic subprocess output retains the first 256 KiB of each stream and
truncation flags, and for a longer stream its last 64 KiB, kept in a rolling
window allocated only once the stream passes 256 KiB. `internal/redact` owns
cut-fragment normalization and `internal/process` exposes captured streams only
as safe diagnostic text. Before secrets are scrubbed, a truncated capture's
head is cut back to its last newline (or,
without one, its last whitespace or nothing), dropping the partial line the
limit cut, together with the first words or lines of a redacted environment
value the limit fell inside. Its kept end likewise drops the partial first line
the window began inside (or, without a newline, the partial first word), then
any token redaction recognises only after context that was dropped (a bearer
token after its prefix, an API key after a terminal escape sequence), and the
last words or lines of a redacted environment value the window began inside,
with the rest of the word they end in. Failure texts and verification evidence
render from that text, with a truncation marker where output was dropped.
Machine stdout is complete up to 16 MiB or fails as `outputTooLarge` (a failed
command's error still keeps the real end past that, from the same 64 KiB
window); invalid UTF-8 also fails explicitly. Git/GitHub machine consumers
never parse a diagnostic truncation marker. All captures retain timeout,
draining and process-group ownership.

State-snapshot cost was measured on 2026-09-22 at `529b63f` with
`OCTOMUS_SCALE_TEST=1 go test ./internal/store -run TestBoundedHistoryScale -v`. The
fixture stores 4 KiB prompts in 1,000, 10,000 and 100,000 historical task records;
each measurement includes 20 dashboard snapshots.

| Historical tasks | State JSON bytes | Median query time | p95 query time | Peak Go heap allocated per snapshot |
| --- | ---: | ---: | ---: | ---: |
| 1,000 | 95,227 | 4.23 ms | 7.81 ms | 790,520 bytes |
| 10,000 | 95,528 | 4.50 ms | 5.75 ms | 785,752 bytes |
| 100,000 | 95,829 | 4.11 ms | 7.57 ms | 784,880 bytes |

These timings are environment-dependent. Allocation measurements count Go heap
bytes allocated during each snapshot, not SQLite's page cache or total process RSS.

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
