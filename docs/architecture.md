# Architecture and operational contract

## Components

```text
SvelteKit static dashboard
          │ same-origin JSON API + bearer token
          ▼
Go control plane (net/http)
 ├─ SQLite state, event log, daily admission counter and ledger
 ├─ one scheduler fleet: a per-repository engine each, ticked in rotating order
 ├─ Git + GitHub CLI publication coordination on trusted git metadata
 └─ sandbox backend ── unix socket ──▶ sandbox broker ── Docker Engine API
                                          │
                  one container per agent turn or verification command
                   ├─ Codex app-server over newline-delimited JSON RPC on stdio
                   ├─ OpenCode HTTP/SSE, relayed as HTTP/2 over stdio
                   └─ bash -o pipefail verification commands
                  internal networks ──▶ egress gateway (allowlisted HTTPS only)
```

One instance serves every registered repository for its single operator: each repository has its own saved configuration, control, budget counters, trusted checkout and workspace root in the shared state database and data directory, and the scheduler rotates its passes across repositories so none systematically precedes another. The instance-wide daily session ceiling (when set) is enforced with each repository's own cap inside the same admission transaction. The primary repository keeps the original unsuffixed settings records and data-directory root, so pre-multi-repository state upgrades in place (schema v11 attributes existing usage counters to it). The dashboard polls authoritative service state for the selected repository and never schedules work itself. The binary embeds the dashboard (`--assets` overrides it from disk) and needs no external database or message broker. The default Docker deployment runs the control plane, [sandbox](sandbox.md) broker and egress gateway from one image; `--sandbox off` runs runners and verification commands as host processes.

## Planning

Each cycle records the remote default-branch revision, owned open PRs (heads and accumulated scope), maintenance targets, task history and read-only external PR summaries. The open inventory is paginated and fails explicitly if incomplete. External context is ordered by PR number and capped at 100 entries, 200 title chars, 2,000 body chars and 512 KiB serialized, secret-scrubbed first, with omitted counts and truncation flags recorded. External/fork PRs never become execution or maintenance targets. Each discovery and proposal-review role runs in its own clone and session; sessions must not modify their worktree or HEAD.

After resolving each advertised `refs/pull/<number>/head` ref and fetching it into `refs/octomus/pr/<number>`, the orchestrator records `local_head_available` when the exact commit exists in the trusted checkout. A false value (including on historical records) keeps the summary but marks its diff unavailable, never as evidence of no overlap.

Owned PRs carry GitHub's `review_decision`, `check_status` and `mergeability` (`status_source`, `status_observed_at`) from one fixed-size GraphQL query per PR that never copies comments, review bodies or check logs into prompts. The GraphQL identity and commit must match the REST head; partial, unknown or racing responses fail the refresh. Empty means not observed, `none` means no decision or checks, and `unknown` mergeability is not clean. Status changes reset idle backoff; timestamp-only refreshes do not. External PRs get no status query, and these observations never replace review/verification or authorize merging or rebasing.

After orchestrator grounding, a standard cycle runs 8–10 discovery agents (default 9), two adversarial reviewers and orchestrator consolidation, accounting for every original proposal ID with a decision and reason. Optional operator `planning_guidance` reaches only these planning roles as a quoted preference that can never authorize skipping any review, verification, eligibility or limit gate; execution, code review and repair prompts never see it. Accepted work requires project evidence, benefit, scope, a self-contained prompt, a supported tier, an enabled category and an eligible target. The core rejects unknown or cyclic dependencies, unordered or forked same-PR plans, duplicate accepted proposals (by title or problem identity), active/unarchived duplicates on the same repository and target, and unowned targets; a rejected plan fails the cycle and dispatches nothing. Same-PR proposals require a linear dependency order; unrelated branches run in parallel. An empty task set is a successful idle cycle. Maintenance follows the same gates on its cadence, targeting the default branch and PRs above the size/age thresholds.

The operator may also file work requests (at most 20 pending per repository). Each cycle seeds up to 10 of them, oldest first within the 100-candidate bound, as `request-<id>` candidates in both execution and audit cycles. They face both adversarial reviewers and consolidation under unchanged rules, must keep their target, and are exempt only from the recorded-decision repeat rule. Their settlement commits in the same plan transaction: accepted links the new task, rejected is final, deferred stays pending, audits only record an assessment, and a request whose target is no longer eligible is rejected by the orchestrator instead of seeded. Failed or interrupted planning changes no request, and withdraw is refused while planning runs. When planning fails on a request, the cycle error names the request ID — withdraw it to stop reseeding.

Execution cycles commit the accepted queue and completed cycle in one SQLite transaction. Audits reuse planning and validation without dispatching tasks: they require paused idle operation, leave queued tasks untouched, conflict with resume/cycle while active, and need only the three planning routes, repository and authentication. Cycles record `audit` or `execution` mode and share numbering and cadence; interrupted planning is marked interrupted and never replayed.

Before creating a cycle, the scheduler checks that remaining daily admissions cover `discovery_agents + 4`. Unaffordable manual starts change no state, continuous mode waits without failed-cycle churn, and run once pauses after its initial queue drain.

## Tasks and dependencies

A task snapshots its configuration, route, source revision, default-branch context, prompt and dependencies immutably. Global admission limits use live policy inside the reservation transaction. Separate attempt policy holds timeouts, repair/no-progress limits and the retry ceiling: explicit retry adopts current limits and a fresh repair budget, while automatic recovery keeps both. Dependency-authorized source advancement keeps the original grounding.

Each task uses an independent clone at `.octomus/tasks/<task-id>/workspace` with git metadata in sibling `repo.git` (read-only in sandboxes), created before starting any runner session. Review and repair threads reuse the same clone.

Completed repair turns are counted apart from review rounds: the repair session, counter and progress checkpoint commit together (or fall back to the supervisor's next failure/interruption checkpoint), so recovery neither resets the consecutive no-progress limit nor double-charges a repair. Recovery resumes unfinished review/verification with a fresh full-diff review and verification, including after the final allowed repair; interrupted verification spends no repair round. Explicit retry clears the checkpoint and counter.

Task mutations run in a store transaction checking the exact saved predecessor, durable cancellation marker and pure lifecycle policy. Named operations restrict which fields admission, workers, retry, recovery, publication, lineage and cleanup may change. Worker checkpoints preserve prior review, verification and session evidence. Retry commits task, cancellation clearing and adopted policy together; archive commits task and merge-authority revocation together.

Workers hold an engine handle with the last committed checkpoint and pending evidence. The admission gate serializes checkpoint commits with operator actions, and released/replaced handles cannot write; runner, Git, filesystem and readiness work stays outside the gate. A cancellation marker committed before output blocks further execution or publication; tasks with an output checkpoint use `publication_uncertain` reconciliation instead.

Task and branch claims survive cancellation until child work joins. If settlement storage fails, the worker raises the recovery barrier before releasing its claim and retains pending evidence in memory for recovery ahead of orphan/retention scans (invalid evidence is dropped in favor of the last durable checkpoint).

Independent tasks run concurrently, while writers to one PR branch and fetches/fork-head replacements on the shared trusted checkout are serialized (clones still run concurrently, and cancelled waiters abort). Same-PR dependents wait for prerequisites to publish; dispatch checks every prerequisite up front, advances `source_revision` only to a recorded prerequisite output with verified ancestry, and blocks on external branch movement. Octomus creates no stacked PRs and merges only in opt-in maintenance mode.

New-PR dispatch enforces live `max_open_prs` (default 5) over observed owned-open PRs plus unrepresented durable task reservations, authorized by a fresh async inventory batch. Existing-PR work and checkpoint publication replay need no new slot. Observed closure/merge or remote re-observation of a cancelled task without a PR releases capacity.

## Review, verification and delivery

Executor changes are committed locally; snapshots and review diffs that add or retarget a gitlink are refused. Review compares the accumulated diff against a fixed base (the original default revision for new work, or the default-branch merge base for existing PRs) in a fresh reviewer session. Repair uses a persistent thread on `repair_route`. Interrupted, failed, malformed or incomplete reviews never count as clean.

After a clean review, configured shell verification commands run with `bash -o pipefail` in a fresh clone of the reviewed revision (each command in a fresh sandbox). Worktree cleanliness and HEAD are checked before and after each command; any tracked change, untracked non-ignored file or HEAD movement fails verification and blocks the task. Evidence records stdout, `[stderr]` and failure exit status, secret-scrubbed and capped at 16 KiB (`[output truncated]` on cuts, plus up to 4 KiB for workspace violations). Net-empty changes cannot publish.

Publication requires a clean review and passing verification at `output_commit`. Before pushing (including replay and reconciliation), the core verifies repository identity, PR ownership/open state, source/default revisions, comparison base, worktree cleanliness, ancestry and no added/retargeted gitlinks, then pushes only the prefixed branch from the trusted checkout with an exact Git lease (`GH_HOST=github.com`).

Existing PRs require the configured prefix, matching head repository and an Octomus task marker in the body; follow-ups post idempotent marker comments without rewriting the description. Titles, bodies, comments and commit messages are secret-scrubbed and length-validated without truncation before any outbound write. Creation and updates validate the returned PR identity and marker; before retrying publication, all matching PR states (including closed/merged) are searched so a matching marker and head confirm delivery without duplicate pushes.

## Explicit baseline verification

Operator-requested baseline checks run only while paused, idle and clear of recovery, pinning the loaded configuration revision and remote default SHA in a disposable clone. Output is capped at 16 KiB per command and 1 MiB total. Baseline checks consume no model admissions and never count as task verification. Cancellation, timeouts and restarts are terminal; failed clone cleanups are retried by housekeeping and recorded on the check and in activity events.

## Runtime and trust

The Codex integration follows the [app-server protocol](https://developers.openai.com/codex/app-server) and bindings of the installed CLI 0.163.0, using `initialize`, `model/list`, `account/read`, `thread/start`, `thread/resume`, `turn/start` and `turn/interrupt` and correlating completion events by thread and turn ID. Structured schemas constrain proposal and review responses, and a role uses `approvalPolicy: never` and `danger-full-access`; returned model, effort and sandbox settings must match.

OpenCode 1.19.4 is the HTTP/SSE baseline. Each task lazily starts its own servers for its selected backends, and planning invocations use separate clients. Servers bind to loopback with per-process Basic authentication and use the service user's provider configuration. The adapter supplies temporary worker policy that disables project configuration overrides, sharing, updates, helper agents, automatic compaction, formatters and LSP, checks session workspace/model/variant/permissions on creation and resumption, correlates message identities and validates native structured results before accepting evidence. See [model routing](model-routing.md).

Unexpected interactive requests fail visibly. RPCs, turns, whole tasks, command output and admission counts are bounded. A runner child that has not ended when its cleanup budget does is reported as the sandbox's own failure, never as a clean turn: its owner remembers that failure across later requests and releases, starts no replacement child, blocks the task as a runner failure, and keeps the broker's record of a sandbox that ran even when no session record exists yet to carry it. The automated test fixtures and the dashboard's model-catalog check start no model turns.

Repository content and agent outputs are never deserialized into operating configuration. API access requires the operator token, which child processes never receive. JSON is redacted before reaching the dashboard and rendered as text, not trusted HTML. Indexed summaries keep complete source titles and errors and are scrubbed with the current environment on every read, so a saved prefix cannot defeat redaction after credentials change; already truncated external PR context drops its cut trailing line or word before redaction.

Codex's own sandbox stays off because the container is the boundary: in the Docker deployment each runner starts in a fresh [sandbox](sandbox.md) bound to one owned root, stopped after each turn, with no GitHub credential and egress only through the allowlisting gateway. Prompts and application policy are **not a security boundary** on their own. With `--sandbox off` a process with the service user's permissions can exercise those permissions; use the dedicated-host model described in [deployment](deployment.md#dedicated-vm-without-a-sandbox). See the [threat model](threat-model.md) for trust boundaries, authentication backoff and redaction limitations.

SQLite uses full synchronous writes and WAL, and only one service may hold the state-directory lock. Restart recovery preserves workspace and session identities and retries initialized interrupted tasks within the retry limit; a graceful stop cancels running work but leaves initialized in-flight tasks to the same recovery as a crash. A supervisor must terminate old processes before recovery: the Docker broker kills every sandbox whose stream closes and sweeps leftovers at startup, and the supplied systemd unit uses control-group termination.

## Usage records and state

Every budget reservation commits its UTC day counter and admission metadata in one transaction. Failed starts still consume reservations; reused repair threads consume another admission for each turn. A definitively missing Codex thread is replaced only when no accepted first turn is recorded for a tracked session; its failed evidence is retained and the replacement consumes a new admission. Sessions with unknown lifecycle and established threads remain intact on resume failures.

State is a Go-owned SQLite schema that releases upgrade at startup through ordered forward-only migrations after a verified backup. The version check runs before any schema or journal change, and pre-release or newer databases are refused untouched. Route snapshots record the selected backend, model, effort or variant. See [backup and upgrade](deployment.md#backup-and-upgrade) and [releasing](https://github.com/tyk-swe/octomus-agent/blob/main/docs/releasing.md).

Opt-in webhook events are inserted by SQLite triggers in the same transaction as the record transition, so a delivered event always corresponds to saved state and never replays historical records. The durable outbox fans out to every registered destination (the generic webhook and any Slack/Discord alert channels), supplying per-destination rotation, bounded pending events, retries and expiry; payloads carry only identifiers, repository, category and action. See [the attention webhook](deployment.md#optional-attention-webhook) for events, payloads and limits.

`--usage-report` opens the database read-only at this release's schema version, reads one transaction snapshot without taking the service lock, and exports metadata only; admissions are not provider charges (see [cost methodology](cost.md)). `GET /api/cycles/{id}/evidence` and `--export-run` assemble `RunEvidenceV1`, a read-only account of what one cycle and its tasks saved (see [run evidence](run-evidence.md)). Keep a full state backup before replacing a binary.

## Operating modes and recovery

`paused`, `run_once` and `continuous` are durable modes. One-shot membership is recorded apart from mutable task retry state, so a later retry can neither erase a batch failure nor join the batch, and the successful cycle, complete queue, decision memory, lineage and one-shot phase change commit together. Typed blocked reasons determine valid operator actions; stale context requires supersession and fresh discovery.

Interrupted planning is never replayed. The scheduler marks a running cycle interrupted once its planning worker has exited, even after storage recovers from refused terminal writes, so its evidence can be inspected and archived without a restart; continuous operation keeps its schedule and starts a fresh cycle with new grounding when due.

While a background recovery write is refused, the scheduler records a recovery error, starts no work in that pass and keeps the saved mode and schedule for the next attempt. During an unresolved episode the first eight recorded redacted causes appear once each, then at most one notice that further causes are suppressed, and the current cause stays visible until recovery succeeds. A publication checkpoint whose worker has exited becomes blocked as `publication_uncertain` once storage accepts the update, keeping its evidence and PR reservation for explicit reconciliation without replaying delivery.

Recovery also settles workerless active tasks and baseline checks, finishing all bounded pages before allowing admission. The scheduler reads broker readiness outside the admission gate, then rechecks operator intent, configuration and runtime ownership before starting work; maintenance merging holds the gate across its bounded external mutation, so pause and merge authorization stay serialized.

## Bounded observation and storage

SQLite JSON records remain canonical, and transactional triggers maintain indexed task, cycle and PR summaries and proposal listings. Dashboard queries limit rows before JSON decoding, read counts and summaries in one transaction, and fetch full evidence only on demand. The store exposes named read models and typed pages and keeps SQL inside itself, and `store.Snapshot` gives live and read-only exports the same canonical evidence and usage queries in one transaction. Historical proposal assessments stay raw so malformed recorded evidence can still be exported and identified.

`model.Task.VerificationFor` selects the latest saved command result by append order; publication, maintenance eligibility and exported evidence share it, and a newer failed or wrong-revision result always supersedes an older success.

Housekeeping runs regardless of operation mode. Completed planning-role clones are disposable once evidence and source checks pass, while unresolved work stays retained until explicit resolution. Archive and discard change workspace lifecycle without deleting database evidence, and each happens once per task or cycle (repeating it conflicts); see [retention](deployment.md#limits-and-retention).

Application storage is measured separately from runner transcripts, and storage admission is a pre-turn check, not a filesystem quota. Measurement is bounded in descriptors, depth, entries and time, and never follows a symlink or changes permissions. A directory under an owned root that cannot be read (or is swapped during the walk, or nests deeper than git's default tree depth allows) holds unknown bytes, so admission refuses its owning task, planning cycle or baseline with the storage limit until that work is resolved, while other owners count only what was measured. An unreadable directory outside the owned roots refuses every admission until it is made readable or moved out of the data directory; a data directory at a filesystem's root has an unreadable `lost+found`, so use a subdirectory. An owner over its entry budget stays unknown, and exhausting the whole scan budget refuses all admissions.

Diagnostic subprocess output keeps the first 256 KiB of each stream and, for a longer stream, its last 64 KiB, with explicit truncation flags. Truncated head and tail are trimmed back to a whole line or word and scrubbed so a cut cannot expose part of a secret, and failure texts and evidence carry a truncation marker where output was dropped. Machine stdout is complete up to 16 MiB or fails as `outputTooLarge`, and invalid UTF-8 fails explicitly; Git and GitHub machine consumers never parse a truncation marker.

State response size and per-snapshot allocation stay independent of history size: queries limit rows before decoding. The bound is checked by `OCTOMUS_SCALE_TEST=1 go test ./internal/store -run '^TestBoundedHistoryScale$' -count=1 -v`, which the weekly workflow (`.github/workflows/weekly.yml`) also runs.

## Decision memory and outcomes

Planning prompts include at most 100 decisions for the default branch and eligible open PR targets. Admission checks the complete stored history for each exact repository, target and problem identity independently of that prompt limit. Decisions record relevant paths, rationale, source context and reconsideration time. Matching problem identities are reused across wording changes. Relevant blob changes or 30 elapsed days permit reconsideration; unresolved tasks continue to suppress duplication. Audit acceptance remains a recommendation and does not suppress subsequent execution. Explicit supersession requests receive their own fresh accepted/rejected/deferred decision.

After the first idle cycle, successive idle outcomes double the interval, capped at 24 hours without shortening a longer configured interval. Lightweight repository and PR-head observation every five minutes shortens extended idle backoff on changed context while preserving the ordinary configured cadence. Manual one-shot runs bypass backoff. PR outcomes are observations distinct from task delivery; known follow-up outputs are recognized when checking external head movement. The service never infers provider charges. It never merges a PR outside the opt-in maintenance delivery mode, whose trusted orchestrator may squash-merge a small, clean-reviewed, verified maintenance delivery only after fresh remote checks, review state, identity and protections confirm the recorded head; every other delivery stays manual.

## Chat control surface

The optional Slack/Discord surface (`internal/chat`) is a third operator front-end beside the dashboard and CLI, and like HTTP it reaches state only through the engine's actions and reads. Both transports are outbound WebSockets (Slack Socket Mode, Discord Gateway), so no inbound endpoint is added. Only host-allowlisted chat user IDs are operators; everyone else is ignored. Commands resolve their repository by explicit flag, thread context, channel binding or the primary repository; work asked for in chat is filed as an ordinary operator request, and state-changing commands require a one-shot, per-operator, five-minute confirmation. Provider redeliveries are deduplicated against a durable seen-events table so a command never executes twice. Chat workers run under the fleet context and reconnect independently; a chat outage never blocks scheduling. See [Slack and Discord chat](chat.md).

## API and dashboard contract

The application owns named API response envelopes in `internal/engine/api_views.go`, the store owns bounded indexed summaries and HTTP owns transport envelopes. `web/api_contract_test.go` derives the dashboard's checked-in TypeScript types from these types, and normal Go tests reject drift. State responses carry operation capabilities, blocker messages and per-cycle `allowed_actions`; these are current display hints, and action handlers still revalidate admission, cleanup ownership and durable state before acting. Pause remains available when configuration is incomplete or saved-state recovery is pending.

The dashboard reads state through one coalescing session whose confirming reads follow every accepted mutation, and through independent resources for task detail, activity, evidence, baseline and settings that keep stale content visible while a read is in flight and let only the newest requested revision become confirmed fresh state: a request for the revision already loading drops older queued intent instead of letting it run afterwards. Relative timestamps tick on a shared timer so retained records continue to age while polling fails, and settings drafts keep display snapshots and patch rules apart from canonical values.
