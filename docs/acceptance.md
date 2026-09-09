# MVP acceptance coverage

This maps the PRD to implementation and automated evidence. Integration tests run the real Rust service, SQLite, scheduler and Git with deterministic app-server/GitHub peers. They validate orchestration behavior, not a live model's judgment or an account's permissions.

| PRD | Implementation / evidence |
| --- | --- |
| AC-01 | Recorded grounding includes current default revision and all open prefixed PRs; existing-PR integration scenario preserves earlier work. The feedback scenario records requested changes, a failing check, conflicts and redacted comments for an owned PR and names it as a feedback target in discovery and consolidation prompts. |
| AC-02 | Configured 8–10 independent discovery sessions; integration suite asserts nine complementary prompts and separate workspaces. |
| AC-03 | Both adversarial assessments and the final orchestrator decision are durable; proposal validation rejects duplicate accepted titles, missing context and invalid dependency graphs. |
| AC-04 | Each queued task has a refined prompt, target, tier and snapshot of its exact route; unsupported effort test verifies no fallback. |
| AC-05 | Parallel integration scenario publishes two independent tasks with distinct execution identities/workspaces; dependent same-branch scenario verifies ordered delivery. |
| AC-06 | Integration suite requires three different reviewer thread IDs and one reused repair thread per task using its saved configurable route; custom-route and retry scenarios verify route preservation. |
| AC-07 | Every recorded review uses the same full comparison base, including after both repairs. |
| AC-08 | Malformed review, incomplete review and failed verification scenarios remain blocked without publication. |
| AC-09 | Default-branch workflow pushes an owned branch and creates one PR; the default revision remains untouched. |
| AC-10 | Existing-PR workflow appends a follow-up to PR #42, preserves prior files, and creates no duplicate PR. |
| AC-11 | Configurable categories, maintenance cadence, changed-line threshold and PR-age threshold drive regular maintenance targeting through the normal pipeline. |
| AC-12 | Empty discovery completes an idle cycle with no tasks or repository mutation. |
| AC-13 | Desktop/mobile browser tests exercise private login, navigation, task review/verification evidence, filtering and saved configuration; overview accessibility checks cover WCAG 2 A/AA rules. |
| AC-14 | Crash after GitHub's creation side effect automatically reconciles the PR; a second scenario closes that PR before restart and verifies no duplicate is created. |
| AC-15 | Rust controls app-server JSON RPC; SvelteKit builds into served assets; every fixture turn asserts no-sandbox/never-approve settings. Dedicated-host systemd packaging is included. |

Additional checks cover concurrent daily-budget admission, cancelled process-group cleanup, API authentication/content-type enforcement, stale remote branch preservation, default-branch dependency rejection, default-branch movement during execution (rebased before the first review) and after a clean review (rebased, then re-reviewed and re-verified before one publication), a conflicting rebase that blocks with the workspace restored, operator guidance reaching every planning turn and no worker turn, and webhook notifications for published/blocked tasks, failed cycles and audits delivered to a local receiver.

A real host must pass **Check connection** and an initial authenticated cycle before unattended commissioning. Automated fixtures cannot validate live model availability, quality of generated improvements, GitHub entitlements, or a target project's verification commands. These are operational acceptance checks, not replaced by the passing local suite.

## Audit, security and distribution coverage

- Audit scenarios cover accepted/rejected/deferred and empty plans, malformed
  consolidation, exhausted budgets, failed starts, interruptions and existing
  paused queues. No audit dispatch or publication is allowed; execution behavior
  remains covered by the original delivery scenarios.
- Rust tests exercise audit readiness, conflicting controls, embedded assets,
  auth backoff and legacy state loading. Usage reports distinguish cycle modes.
- Desktop/mobile tests cover audit progress and filtered decisions using synthetic
  responses; these images remain fixture evidence.
- Distribution tests start the copied executable without external dashboard assets,
  load SPA scripts, check override/listener errors, and verify installer success
  and failure paths against local release fixtures.
- CI prepares native x86_64/aarch64 release smoke tests and exercises hardened
  systemd write paths/process cleanup on disposable hosts. Actual workflow runs
  and fresh-owner-VM acceptance require separate live evidence; follow the
  [operator checklist](operations.md).
