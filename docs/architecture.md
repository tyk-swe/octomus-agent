# Architecture and operational contract

## Components

```text
SvelteKit static dashboard
          │ same-origin JSON API + bearer token
          ▼
Rust / Axum service
 ├─ SQLite state, event log, daily admission counter
 ├─ one scheduler, configured task concurrency, branch writer locks
 ├─ Codex app-server subprocesses over newline-delimited JSON RPC
 └─ Git + GitHub CLI publication coordination
```

The dashboard polls authoritative Rust state and never schedules work itself. The production server serves the compiled dashboard directly. No database service, message broker, Docker runtime, or sandbox backend is required.

## Code map

| Module | Responsibility |
| --- | --- |
| `src/config.rs` | Defaults, explicit role routes, exact tier mapping, input validation |
| `src/model.rs` | Task, cycle, proposal, session, review, verification and PR records |
| `src/store.rs` | SQLite WAL persistence, atomic plan commit, event retention, session budget, redaction |
| `src/codex.rs` | App-server handshake, model catalog, thread start/resume, correlated RPC/events, structured results |
| `src/process.rs` | Bounded output capture, timeouts, cancellation and process-group ownership |
| `src/git.rs` | Source snapshots, owned PR context, immutable review commits, revision leases, idempotent delivery |
| `src/engine.rs` | Grounding, discovery, proposal challenges, task scheduling, review/repair, recovery, retention |
| `src/api.rs` | Operator authentication, configuration, observation and controls |
| `web/src` | Responsive Svelte/TypeScript dashboard |

## Planning

Each cycle records the remote default-branch revision, open prefixed PRs, their heads and accumulated scope, maintenance targets, and task history. The orchestrator inspects the repository and PR diffs. Each discovery and proposal-review role has a separate clone and Codex thread. Planning sessions are instructed to inspect rather than mutate, and their worktree/HEAD must remain unchanged.

The standard cycle runs nine discovery agents (configurable from eight to ten), followed by two adversarial reviewers and orchestrator consolidation. The final result must account for every original proposal ID, with a decision and reason. Accepted work needs project evidence, benefit, scope, a self-contained prompt, a supported tier, and an eligible target. Unknown dependencies, dependency cycles, duplicate accepted titles, and unowned targets are rejected by the core.

Semantic value, overlapping ideas, and conflicting assessments are judged by the proposal reviewers and orchestrator; their results are recorded. The core cannot independently prove an idea's product value. Returning an empty task set is a successful idle cycle.

Maintenance is prioritized at the configured cadence for the main project and PRs above the size/age thresholds. It follows the same proposal and delivery gates as feature work.

The complete accepted queue and successful cycle are committed in one SQLite transaction. Incomplete discovery cannot become executable work after a restart.

## Tasks and dependencies

A task snapshots its configuration, route, source revision, default-branch context, refined prompt and dependencies. Its initial app-server thread identity determines `.octomus/tasks/<thread-id>/workspace`. Each task uses an independent Git clone. Review and repair threads work in that same task clone.

Independent tasks can run concurrently. Existing PR branch writers are serialized. Dependent tasks on the same existing PR wait for their prerequisites to publish; the source revision is advanced only to a recorded prerequisite output and its ancestry is checked. External branch movement blocks stale work.

Code-dependent default-branch proposals must be consolidated into a cohesive task or deferred until the prerequisite PR has merged. Merely publishing a separate PR does not make its code available on the default branch. Octomus does not auto-merge or implicitly create stacked PRs.

## Review, verification and delivery

The executor's changes are committed locally. Review uses a fixed comparison base: the original default revision for new work, or the merge base with the default branch for existing PR work. Every review round examines the entire accumulated diff against that base.

A reviewer is always a fresh Codex thread. A repair thread starts separately with `gpt-6-astra` / `medium` and is resumed for subsequent repair turns. Interrupted, failed, missing, malformed, or explicitly incomplete results never count as clean reviews. Rounds and their revisions are recorded.

Configured shell verification runs after a completed clean review. Every command must pass on exactly the reviewed revision; worktree or HEAD changes during review or verification block publication. Commands run from the assigned workspace with Bash `pipefail`. Net-empty changes are not publishable, even if an executor made commits.

Publication requires recorded clean review and successful verification evidence for the output commit. Before writing, the core verifies repository identity, PR ownership/open state, source/default revisions, worktree cleanliness and commit ancestry. It pushes only the assigned prefixed branch, using an exact Git lease to guard the check/push race. The push destination comes from the validated configured checkout, not an agent-editable task remote. No default-branch push is generated.

GitHub is the MVP provider. Existing PRs require the configured prefix, matching head repository, and an Octomus task marker in their body. Prefixed PRs without that marker contribute planning context but are not writable targets. New branch suffixes use unique task IDs. PR bodies record the objective, scope, benefit, verification, implementation summary and publication marker. Follow-ups append their task record to the existing PR.

Before retrying a publication, the service searches all matching PR states, including closed and merged PRs. A matching task marker and output head confirm previously completed delivery without another push or duplicate PR. Changed remote state is surfaced for reconciliation.

## Runtime and trust

[Codex app-server documentation](https://developers.openai.com/codex/app-server) and the bindings generated by installed CLI 0.153.4 informed the integration. The client uses `initialize`, `model/list`, `account/read`, `thread/start`, `thread/resume`, and `turn/start`, correlating completion events with thread and turn IDs. Structured schemas constrain proposal/review responses. A role uses `approvalPolicy: never` and `danger-full-access`; returned model, effort and sandbox settings must match.

Unexpected interactive requests fail visibly. RPCs, turns, whole tasks, command output and admission counts are bounded. No model turns are started by the automated test fixtures or dashboard's model-catalog check.

Repository content and agent outputs are never deserialized into operating configuration. API access requires the operator token, which is excluded from child-process environment variables. JSON is redacted before being returned to the dashboard, and rendered as text rather than trusted HTML.

Because execution is deliberately unsandboxed, prompts and application policy are **not a host security boundary**. A process with the service user's permissions can exercise those permissions. Use the dedicated-host deployment model described in the PRD.

SQLite uses full synchronous writes and WAL. Only one service may hold the state-directory lock. Restart recovery preserves workspace/session identities and retries initialized interrupted tasks within the retry limit. A deployment supervisor must terminate old processes before recovery; the supplied systemd unit uses control-group termination.
