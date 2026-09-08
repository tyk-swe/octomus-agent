# Octomus Agent — Product Requirements Document

**Product:** `octomus-agent`  
**Scope:** MVP  
**Status:** Draft compiled from the four supplied handwritten pages  
**Date:** 2026-09-07

> When the project builds itself.

## 1. Product overview

Octomus Agent is an autonomous, continuously running project-improvement system. It understands a repository and its existing work, discovers worthwhile improvements, challenges and consolidates proposed tasks, implements them through Codex sessions, reviews and repairs the changes, and delivers the results as pull requests.

The working shorthand is **“Dependabot, with features.”** Its scope extends beyond dependency maintenance to features, bug fixes, performance, user experience, developer experience, refactoring, testing, documentation, and architectural health.

After initial configuration, the normal discovery-to-PR workflow runs without repeated prompting or routine human supervision. The operator uses a web dashboard to configure and observe the system rather than manually inventing and dispatching every task.

The output is useful, coherent project evolution—not a constant stream of changes for their own sake.

## 2. Goals and product principles

### 2.1 Autonomous delivery

Continuously discover, implement, and publish beneficial improvements as new or updated PRs. Account for work already present on the default branch and in open Octomus PRs. Continue improving substantial in-progress branches instead of treating every cycle as a greenfield task.

### 2.2 Sustained code health

Keep the project understandable, maintainable, and architecturally coherent while it grows. Address incomplete features, technical friction, unnecessary complexity, unhealthy tests, stale documentation, dependency work, and required migrations.

Simplification must preserve useful capabilities. Smaller code is not automatically better code; deleting functionality or weakening verification merely to produce a smaller diff is not an improvement.

### 2.3 High throughput without uncontrolled expansion

Use multiple discovery agents and complexity-aware execution to cover a broad range of opportunities. Reject speculative features, redundant work, gratuitous abstractions, and changes whose ongoing maintenance burden outweighs their benefit.

The system must be able to conclude that no worthwhile work is currently available. Continuous operation does not require continuous repository mutation.

### 2.4 Minimal machinery

The product itself should remain straightforward to maintain. Use the specified Rust core, Codex app-server integration, and TypeScript/SvelteKit dashboard without turning the MVP into a general-purpose agent platform.

Verification should be proportional to the change. Maintenance may remove unnecessary gates, redundant tests, and overengineering, but must not remove useful protection simply to make a task pass.

## 3. Operator, deployment, and MVP boundary

The primary operator is a repository owner or maintainer who wants ongoing improvement without repeatedly writing prompts, coordinating agents, and preparing PRs.

The MVP runs continuously on a **dedicated VPS or VM**. Its execution environment is deliberately unsandboxed: all Codex sessions run in the requested **YOLO/no-sandbox mode**.

| Area | MVP requirement |
| --- | --- |
| Core | Rust owns orchestration and controls Codex app-server. |
| Web interface | TypeScript and SvelteKit provide a configurable dashboard. |
| Runtime | Long-running, unattended operation, intended to work 24/7. |
| Workspace | Tasks execute under `.octomus/tasks/<session-id>/workspace`. |
| Delivery | Create or update Octomus-owned branches and their PRs. |
| Execution protection | No application-provided sandbox in this MVP. |
| Packaging exclusions | Docker, sandbox backends, and alternative isolated execution environments are out of scope. |

The workspace convention separates task files and working copies; it is not a security boundary. The dedicated host must be treated as an environment in which the configured agent processes can exercise their account's host permissions.

“Ships” means **publishes a PR or updates an existing PR** in this document. Automatic merging, production deployment, and live production migrations are not specified in the source notes and are excluded by the proposed defaults in Section 12.

## 4. Improvement coverage

Discovery and maintenance must cover the following areas. These are coverage responsibilities, not a requirement to introduce a permanent agent role or subsystem for every row.

| Area | Expected opportunities |
| --- | --- |
| Features | Large and small features; major and minor enhancements; completion of partially implemented features. |
| Correctness | Reproducible bugs, incorrect behavior, missing edge-case handling, and regressions. |
| Performance | Concrete improvements to relevant execution paths, resource use, or responsiveness. |
| UX and DX | User-facing usability, developer workflows, tooling friction, confusing behavior, and avoidable complexity. |
| Refactoring and maintenance | Small and medium refactors, clearer structure, better code health, and coherent architectural boundaries. |
| Simplification | Removal of unnecessary abstractions, duplicated mechanisms, code bloat, unhelpful gates, and other overengineering without losing useful features. |
| Test health | Useful missing coverage, broken or unhealthy tests, redundant tests, and maintenance of meaningful regression protection. |
| Dependencies and migrations | Dependency changes together with the code, configuration, compatibility work, and migrations needed to make them complete. |
| Documentation | Accurate documentation, completion of missing explanations, and trimming of stale, redundant, or unnecessary material. |

A proposal must identify a project-specific benefit. A category being available does not, by itself, justify creating a task in that category.

## 5. Autonomous operating loop

```text
Ground current repository and open Octomus PRs
    → Discover opportunities with 8–10 subagents
    → Challenge proposals with the orchestrator and two adversarial reviewers
    → Deduplicate and consolidate beneficial work
    → Produce refined task prompts
    → Assign XS / S / M / L / XL execution tiers
    → Execute each task in a separate Codex session
    → Fresh review ↔ repair loop
    → Create or update an Octomus PR
    → Refresh context and repeat
```

### 5.1 Ground current project context

At the start of a cycle, the orchestrator builds or refreshes its understanding of the current project. This includes the codebase, architecture, conventions, build and test workflows, relevant documentation, and the current state of existing Octomus work.

It must inspect open PRs associated with `octomus/*` branches. Their scope and accumulated changes are part of the planning context, not an afterthought. A proposal must distinguish work against the default branch from work that belongs in an existing Octomus PR.

Record the relevant repository and branch revisions so proposals and subsequent execution can be tied to concrete source state. Refresh stale context when intervening changes materially affect the task.

**Output:** Current project context, relevant open-PR context, and identified candidate targets for discovery.

### 5.2 Discover opportunities with 8–10 subagents

Launch **8–10 discovery subagents** in the standard cycle. Give them complementary exploration scopes spanning the coverage in Section 4. They may propose improvements to the main project or to an existing Octomus branch.

Discovery agents propose work; they do not independently publish changes. Their proposals should be concise and include the problem or opportunity, supporting repository evidence, expected benefit, approximate scope, and whether the task belongs on the default branch or an existing PR.

The discovery fan-out does not imply that 8–10 implementation sessions must run concurrently. Discovery concurrency and implementation concurrency are separate concerns.

**Output:** A set of grounded candidate tasks, including the option to return no useful candidates.

### 5.3 Challenge proposals before execution

The orchestrator and **two adversarial reviewers** evaluate the proposed tasks before implementation begins.

The review asks whether the problem is real, the expected benefit is meaningful, the work fits the project's direction, and an existing implementation or PR already addresses it. It also challenges unnecessary complexity, disproportionate scope, likely regressions, missing dependencies, and ideas that would make the project harder to maintain.

Adversarial review is intended to reject weak ideas and improve good ones, not invent objections to satisfy a quota. Conflicting assessments must be resolved explicitly rather than silently ignored.

These are **proposal reviewers**. They are separate from the post-implementation code-review sessions described in Section 7.

**Output:** Accepted, rejected, or deferred proposals with concise reasons.

### 5.4 Consolidate beneficial tasks

Compile the accepted proposals into a coherent task set. Deduplicate overlapping ideas and merge related work when implementing it together produces a cleaner result. Preserve separate tasks where their objectives, dependencies, or verification are meaningfully different.

Account for task dependencies and overlapping branch work. Avoid scheduling two agents to independently implement competing versions of the same improvement.

Large changes may be decomposed when their parts have useful independent outcomes. Do not mechanically split a cohesive change merely to force it into a lower complexity tier, or combine unrelated changes into one oversized PR.

**Output:** A nonredundant, dependency-aware set of worthwhile tasks.

### 5.5 Produce polished task prompts

Turn each accepted task into a **polished, trimmed, refined prompt** suitable for its execution session.

The prompt must communicate the objective, relevant context, intended target, scope boundaries, required outcome, and proportionate verification. Include important dependencies and constraints, but avoid repetitive boilerplate, vague motivational language, or an implementation script that prevents the executor from making better local design decisions.

Each prompt should be sufficiently self-contained for a fresh session. Information essential to correctness must not exist only in the orchestrator's private conversation history.

**Output:** An executable prompt attached to a specific task and target.

### 5.6 Assign complexity and route execution

Assign each task one of five tiers based on its reasoning difficulty, architectural reach, uncertainty, and change scope—not simply anticipated line count.

| Tier | Intended scope | Requested model | Requested reasoning effort |
| --- | --- | --- | --- |
| XS | Very small, narrowly scoped work with little ambiguity. | `gpt-5.6-luna` | `xhigh` |
| S | Small work with limited interactions and a clear objective. | `gpt-5.6-luna` | `max` |
| M | Moderately complex implementation or cross-cutting local work. | `gpt-6-astra` | `low` |
| L | Substantial work requiring broader design and coordination. | `gpt-6-astra` | `medium` |
| XL | The most complex accepted work, with significant uncertainty or architectural scope. | `gpt-6-astra` | `high` |

The model and effort pairs above preserve the requested routing. They are configuration requirements, not a claim that a particular installed Codex version supports every identifier or effort value.

Model availability must be checked against the configured runtime. An unsupported route must be visible and must not silently become a different model or reasoning setting.

The notes do not specify default models for the orchestrator, discovery agents, proposal reviewers, or post-implementation reviewer. Those roles remain configurable rather than inheriting an invented default.

**Output:** A task with a recorded tier, execution model, and reasoning effort.

### 5.7 Execute each task separately

Execute each prompt in its own Codex session controlled by the Rust core through Codex app-server. All sessions use the MVP's configured YOLO/no-sandbox mode.

Task work happens under:

```text
.octomus/tasks/<session-id>/workspace
```

A task workspace starts from the appropriate default-branch or existing-PR revision. Separate sessions must not share a mutable working directory. Parallel work must respect dependencies and overlapping targets; conflicting writers must not race to update the same branch.

The executor implements the complete accepted task, including relevant tests, documentation, dependency adjustments, and migrations within its scope. It runs the project's appropriate verification rather than creating a new verification framework for the task.

An executor declaring completion is not sufficient for publication. Its changes must pass through the review-and-repair workflow.

**Output:** Implemented changes, verification results, and enough session state to begin review.

### 5.8 Review, repair, and publish

Apply the lifecycle in Sections 7 and 8. Only changes that have completed the required review and verification are eligible for normal PR publication.

After publication, update the orchestrator's knowledge of the branch, PR, and completed task so the next cycle does not rediscover the same work.

### 5.9 Refresh and repeat

Return to project grounding and continue the loop. Incorporate merged, updated, or closed PRs and any changes made outside Octomus.

Run regular maintenance alongside feature and improvement discovery as described in Section 9. Idle when no beneficial work is ready instead of manufacturing changes merely to remain active.

## 6. Agent and session responsibilities

| Role | Responsibility | Context lifecycle |
| --- | --- | --- |
| Orchestrator | Ground the project, coordinate discovery and proposal decisions, consolidate tasks, route work, and manage progress. | Refreshes project state between cycles. |
| Discovery subagents | Explore complementary areas and propose grounded improvements. | Scoped to the relevant discovery cycle and target. |
| Two adversarial proposal reviewers | Challenge task value, scope, duplication, design impact, and feasibility. | Receive the proposals and evidence needed for that decision. |
| Task executor | Implement one accepted prompt. | Separate execution session for each task. |
| Code reviewer | Review the completed change set and subsequent repairs. | **Fresh review context on every round.** |
| Repair agent | Address actionable review findings. | **Fresh at the first repair round, then reused for subsequent repairs of that task.** |

The repair agent uses the task’s saved configurable repair route, defaulting to **`gpt-6-astra` with `medium` reasoning effort**. Its context is separate from the original implementation session.

A fresh reviewer and a persistent repair session serve different purposes and must not be collapsed into one continuing conversation.

## 7. Post-task review and repair loop

After each task finishes, start a fresh code-review session through Codex app-server with behavior equivalent to the requested **`/review`** workflow. This specifies the desired review behavior, not a literal app-server protocol method name.

The lifecycle is:

1. Review the task's complete current change set in a fresh context.
2. When actionable findings exist, start a fresh repair session using the task’s saved repair route.
3. Have the repair agent address the findings and rerun relevant verification.
4. Start another fresh review against the full updated change set, including the repairs—not merely the latest fix commit.
5. Send additional actionable findings back to the **same repair session** that handled the first round.
6. Continue until a completed review reports no remaining actionable findings and the required verification succeeds, or the task enters a visible blocked/failed state under the configured operating limits.

Use an explicit, recorded comparison base throughout the task's review loop. When working on an existing PR, supply its accumulated context so the reviewer can assess interactions with earlier changes.

Findings must be evaluated on their technical merits. An unsupported finding may be rejected with a recorded rationale; a real issue must not be dismissed simply to reach a clean status. Preserve the outcome of each round and its associated revision.

Review completion is evidence, not a promise that the code is defect-free. An empty, interrupted, failed, or unparseable review result is not a clean review.

The proposed limits in Section 12 prevent indefinite repair loops. Exhausting a limit does **not** turn unresolved work into publishable work.

## 8. Branch and pull-request lifecycle

### 8.1 Work originating from the default branch

The source notes refer to the default branch as `main`. Interpret “work on main” as work based on that branch's revision in a task workspace, not permission to push generated changes directly to the shared default branch.

After the task completes its review-and-repair loop, create a new `octomus/*` branch for the result, push that branch, and open a PR against the configured default branch.

The exact suffix format is unspecified; it must identify the task without colliding with another active branch. Implementation may establish the local task branch earlier, provided the publication behavior remains the same.

### 8.2 Work on an existing Octomus PR

Start from the relevant `octomus/*` branch. Implement and review the improvement with awareness of the PR's current scope. Push the resulting changes to that branch and update the existing PR instead of creating a duplicate PR for the same work.

Recheck branch and PR state before publication. When a PR has changed, merged, or closed during execution, reconcile against that new state rather than blindly publishing against stale assumptions.

Only update branches and PRs identified as belonging to Octomus. The proposed ownership and conflict defaults appear in Section 12.

### 8.3 PR contents and publication record

A PR must explain the problem or opportunity, what changed, and why the result is beneficial. Include relevant verification results and material limitations, migration notes, or remaining risks.

Record the association between task, source revision, output commit, branch, and PR. Publication retries must reconcile existing remote state so an interrupted operation does not create duplicate PRs or discard existing work.

Publication is the completion boundary for the autonomous task. Merge decisions are separate unless explicitly added to a future scope.

## 9. Continuous maintenance and coherence

Run regular maintenance loops against both the default branch and **large, long-lived, or substantially growing open Octomus PRs**. Maintenance must not be postponed indefinitely while the system adds features.

Maintenance covers code cleanup, dependency work, required migrations, documentation trimming, test health, removal of unnecessary gates, and simplification of overengineered structures. It also addresses inconsistent patterns or fragmented design introduced by successive tasks.

For a large PR, maintenance should improve the same ongoing change rather than repeatedly opening disconnected cleanup PRs around it.

Maintenance proposals use the same value assessment, task preparation, execution, and review lifecycle as other work. They do not gain permission to bypass review because they are labeled cleanup.

The governing standard is **coherent evolution without capability loss**. Do not delete useful features, regression coverage, documentation, or architectural boundaries merely to shrink the repository. Preserve behavior unless a behavior change is an explicit part of an accepted task.

Do not equate a green test run with good architecture, or a reduced test count with healthier testing. Assess whether the resulting project is easier to understand and maintain while still satisfying its intended behavior.

## 10. Product architecture and dashboard

### 10.1 Rust core

The Rust core owns the operational loop, task scheduling, session lifecycle, model routing, task state, review/repair coordination, and Git/PR publication coordination. It controls Codex app-server and consumes the session events and results needed to advance work.

Keep policy and execution state authoritative in one place. The dashboard must not implement a second independent scheduler or a competing interpretation of task status.

The PRD does not prescribe a database, message broker, Rust web framework, or internal RPC design. Choose the smallest maintainable implementation that supports the required behavior.

### 10.2 Codex app-server integration

The integration must support starting separate task and review sessions, retaining and resuming the repair session, tracking progress and completion, and surfacing failures. Preserve the configured model and reasoning settings for each role.

A task session, fresh review session, and resumed repair session must remain distinguishable in application state and in the dashboard. Do not rely on a single ever-growing conversation to represent the entire system.

### 10.3 TypeScript and SvelteKit dashboard

Provide a web dashboard for configuration, progress, and operational control. The following is the minimum proposed dashboard surface derived from the notes' requirement for a highly configurable web interface:

| View | Required information or control |
| --- | --- |
| Overview | Running, paused, idle, or unhealthy state; current cycle; active tasks; recent results. |
| Proposals and queue | Accepted, rejected, and deferred proposals; reasons; task scope; tier; target branch or PR; dependencies. |
| Task detail | Prompt, model and effort, session progress, workspace, verification, review findings, repair rounds, and final status. |
| Branches and PRs | Active Octomus work, associated tasks, publication status, and PR links. |
| Configuration | Repository settings, role models, tier routing, scheduling, concurrency, maintenance, and operating limits. |
| Operations | Pause or resume new work, cancel a task, retry eligible failed or blocked work, and inspect errors. |

Keep routine use simple. Configuration depth must not require a complex setup ceremony or a separate workflow language.

### 10.4 Configuration domains

Support operator configuration of the target repository and default branch, branch prefix, role-specific model settings, tier routing, discovery fan-out, execution concurrency, enabled improvement categories, maintenance cadence, and repository verification commands.

Operating limits, retention, and the criteria for treating an open PR as large or maintenance-due must also be explicit rather than buried in prompts. Their initial values are implementation decisions, not numbers supplied by the handwritten specification.

Do not expose secrets in dashboard views or session summaries. Access to the dashboard is an operator capability, not an authorization for repository content to change the system's own operating policy.

## 11. Reliability and minimum acceptance criteria

### 11.1 Unattended operation

Keep enough durable state to recover task identity, selected target, session associations, review status, and publication progress after interruption. Recovery must reconcile actual workspace and remote state rather than assuming the previous operation either fully succeeded or fully failed.

One failed task must not silently stop the entire service. Surface the failure and allow unrelated eligible work to continue when doing so is safe. Authentication failures, unavailable routes, exhausted configured budgets, and conflicting source changes must produce actionable states rather than invisible retry loops.

Bound accumulated task workspaces and logs according to an explicit retention policy. Preserve material needed to inspect active or failed tasks; do not silently discard unresolved work to reclaim space.

These are operating requirements for a 24/7 product, not a mandate for distributed infrastructure.

### 11.2 Acceptance criteria

| ID | Scenario | Required result |
| --- | --- | --- |
| AC-01 | A cycle begins with existing Octomus PRs. | Planning incorporates the current repository and those PRs before proposing work. |
| AC-02 | Standard discovery runs. | 8–10 subagents explore complementary opportunities and return grounded proposals. |
| AC-03 | Discovery produces weak or overlapping ideas. | The orchestrator and two adversarial reviewers assess them; rejected work is not executed; duplicates are consolidated. |
| AC-04 | An accepted task becomes executable. | It has a refined prompt, a target, a complexity tier, and the requested model/effort route. |
| AC-05 | Multiple tasks execute. | Each receives a separate Codex session and workspace; conflicting branch writers do not race. |
| AC-06 | A task produces review findings. | A fresh repair session using the task’s saved route handles the first repair; subsequent rounds reuse that session while every review starts fresh. |
| AC-07 | A repair changes previously implemented code. | The next review examines the complete updated task change set, not only the latest patch. |
| AC-08 | Review or verification does not complete successfully. | The task is not represented as clean or published as completed work. |
| AC-09 | A reviewed task originated from the default branch. | Octomus creates and pushes an owned branch and opens a new PR. |
| AC-10 | A reviewed task belongs to an existing Octomus PR. | Octomus updates the existing branch and PR without creating a duplicate. |
| AC-11 | The main project or a large open PR needs maintenance. | Maintenance work can address dependencies, migrations, code/docs, test health, and unnecessary complexity through the normal lifecycle. |
| AC-12 | No beneficial work is available. | The service can idle and revisit later without generating artificial churn. |
| AC-13 | The operator uses the dashboard. | Progress, configuration, task/review history, PRs, and the core operational controls are accessible. |
| AC-14 | The service is interrupted around publication. | Recovery reconciles existing work and remote state instead of duplicating publication. |
| AC-15 | The MVP is deployed. | The Rust core controls Codex app-server, the TypeScript/SvelteKit dashboard is available, and task execution uses the dedicated-host YOLO/no-sandbox model. |

### 11.3 Success measures

Assess whether the system produces useful PRs with manageable operator intervention, maintains coherent architecture, completes accepted work, and avoids repeated proposals or repair churn. Track task outcomes and review failures so these judgments are grounded in actual operation.

PR count, lines changed, and continuous agent activity are not success targets. No acceptance threshold for throughput, cost, or defect rate was supplied; establish those targets from observed operation rather than inventing promises in the MVP specification.

## 12. Interpretations, proposed defaults, and unresolved choices

This section distinguishes source interpretation and implementation defaults from decisions explicitly written in the notes.

### 12.1 Transcription and terminology

The product name is transcribed as **Octomus Agent**, with `octomus-agent` as its identifier. The handwritten `octomus/~`-style branch notation is interpreted as a branch namespace and normalized to **`octomus/*`**, not a literal branch name.

The workspace notation is preserved as `.octomus/tasks/<session-id>/workspace`. Treat the session ID in this path as the task's initial execution-session identity; fresh reviewers and the repair session act on the same task workspace, not unrelated copies of its changes.

The repeated final step number in the notes is normalized into the review/publish stage followed by the refresh-and-repeat stage.

### 12.2 Proposed operating defaults

**PR-only delivery.** Do not automatically merge PRs, push generated commits to the shared default branch, deploy releases, or run migrations against production. Repository migration code and documentation remain valid task outputs.

**Owned-branch updates.** Use `octomus/` as the default prefix, associate branches with recorded tasks and PRs, and do not overwrite unrelated or externally changed work without reconciliation. Serialize updates to the same target branch.

**Bounded autonomy.** Configure time, retry, repair-round/no-progress, concurrency, and resource or spending limits appropriate to the host and account. On exhaustion, preserve the workspace and report a blocked or failed state; never reinterpret a limit as approval to publish unresolved work.

**No silent route substitution.** Preserve the requested model mapping. Missing or unsupported models and reasoning values require a visible configuration correction or an explicit operator-configured fallback.

**Private operator access.** Expose the dashboard only through an operator-controlled access boundary. A public multi-tenant service is not part of this MVP.

**Recovery and retention.** Persist the minimum state needed for task and publication reconciliation, and make cleanup policy explicit. Do not introduce a distributed control plane solely to achieve restart recovery on one host.

### 12.3 Choices intentionally left open

The source does not select a Git hosting provider, authentication mechanism, default models for non-executor roles, persistence technology, internal transport, exact branch suffix format, precise scheduling intervals, concurrency limits, or the threshold for a “very large” open PR.

Those choices must be resolved during implementation without changing the core product contract: autonomous, project-aware improvement; challenged and consolidated proposals; separate tier-routed execution; fresh reviews with a persistent repair session; PR-based delivery; recurring maintenance; and a configurable dedicated-host MVP.
