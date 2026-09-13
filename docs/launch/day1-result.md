# Day 1 rehearsal result — 2026-09-12

**DRAFT — owner review required before public sharing.** Private evidence and its
locations are not approved for publication.

## Verified observed outcome: PR published

Octomus published [PR #3](https://github.com/tyk-swe/octomus-agent/pull/3), fixing
false timeouts from descendants holding output pipes open, then returned to
paused/idle. Real model sessions used the requested machine, existing accounts
and a temporary instance. Dedicated-deployment acceptance was not established.

| Revision | Observed value |
| --- | --- |
| Initial application / target baseline / reviewed comparison base | `a2f8f6288ba7a546fb4c2a1ab993c2210778bb73` |
| Corrected application used for continuation | `14ffefc610a0f0330624c531f945f87ad28580b1` |
| Output commit = clean review = successful required check = public PR head | `06bdf7a81dde2bb932e9d18b102568423493690d` |

The GitHub API and public PR were inspected at 18:21 UTC. PR base `main` still
pointed to the recorded baseline. At rehearsal completion the PR was open; no
merge was performed during the rehearsal.

## Actual attempts and decisions

1. Execution cycle `1a25930b-4c9e-4bb3-9b0c-1ac6eee09936` completed 13 planning
   sessions: one proposal accepted, six deferred. Its task
   `73aaf60b-cedc-49a1-b8ce-374022c4afe7` blocked before its first executor turn:
   `thread/resume` returned `no rollout found`. The original failure is retained
   in the private state and verification evidence archive.
2. After the development fix and supported **Supersede and rediscover**, execution
   cycle `aebbd918-063b-440e-97c9-1758c93b36b0` replanned: one accepted, four
   deferred. Proposal `rediscover-73aaf60b-cedc-49a1-b8ce-374022c4afe7` produced task
   `85231f37-a1a0-41c9-aa09-901ad2e3980f` in that same cycle. Both proposal
   reviewers accepted it, requiring reproduction and preservation of output,
   exit status, concurrent draining and cancellation. The orchestrator accepted
   that bounded scope. No audit result was substituted.

Fresh reviewer `01a096c2-6151-7b43-97f3-e266b7e4bbfc` completed the full diff review
with zero findings. It did not reproduce the pre-fix failure; the operator's
isolated comparison supplied that evidence.

## Commands and regression evidence

| Check | Actual result |
| --- | --- |
| Separate target baseline: `npm ci --prefix web`, `make check`, `make test` | All exit 0. |
| Executor-startup regression using the existing deterministic integration scenario | Same missing-rollout failure before the fix, exit 1; identical regression after the fix, exit 0. |
| Corrected application: `make check`, `make test` | Both exit 0; 59 Rust tests, 58 integration scenarios, distribution/packaging guards and 22 browser tests passed. |
| Pinned Codex contract with synthetic local provider | Passed, including fresh-thread behavior and persisted-thread resume. |
| `cargo test --locked --test process_lifecycle`, isolated baseline and output checkouts | Four expected timeout failures before, exit 101; four passes after, exit 0. Identical test file on both. |
| Octomus-required `npm ci --prefix web && make check && make test` | Successful, with unchanged workspace/HEAD at the output commit. |

Regression file SHA-256:
`db7fa132143c6a84d78b5cf36f57acf6a1bc2122c3ecb2bf6650eaff60d6c865`.
The four cases cover inherited stdout/stderr, zero/nonzero exit status, retained
output and descendant termination. They establish regression behavior separately
from the real model sessions.

Four opt-in Rust tests remain excluded from the default Make target. Stored
verification output is capped at 16 KiB: its browser console tail is unavailable.
The successful command/revision record and final browser passed marker are present;
executor and reviewer summaries both report 22 browser tests passed. GitHub's
separate client-contract and `verify` jobs subsequently passed; all checks on the
PR head were confirmed green on September 13. The separate GitHub Codex review
marked completion, with no inline findings observed at 18:26 UTC on September 12.

## Provenance, usage and interventions

All four role routes, five execution tiers and repair were explicitly saved as
Codex / `gpt-6-astra` / `medium` and validated with real catalog and execution
doctor, using Codex 0.153.4 without warnings. Actual sessions performed planning,
S-tier execution and fresh code review. Live repair was not needed. Runtime model
identity independent of requested routes: **UNREPORTED** in retained API records.

Admissions: **29**, all attributed: first attempt 13 planning + 1 failed executor
startup; second attempt 13 planning + executor + reviewer. Limits changed from 30
to 40 to accommodate continuation; admissions are not dollars. Attributable cost:
**UNAVAILABLE**. No credit purchases or billing changes were made. Repository-only
credential scope and disabled paid overage were not independently verified.

Per-attempt monotonic timing runs from the control request through observed
paused/idle state, with ten-second polling: **371.32 s** and **2,806.72 s**.
UTC first-request-to-final-idle span, including the fix and checks: **4,301.72 s**.
Planning-only cycle durations are not used as delivery times.

Interventions: private client installation, configuration, two Run once requests,
failure preservation, development startup fix, paused restart, supported
rediscovery and admission-limit adjustment. Octomus owned publication. Cleanup at
18:25 UTC removed the temporary instance/token, tools, clones and caches after
archiving state/source evidence. No owned process or listener remained; existing
account credentials/history were not deleted.

Evidence references: private `publication-verified.json`, cycle/task records,
`usage-final.json`, baseline/development command results, paired regression logs,
`interventions.jsonl`, `cleanup-final.json`, and the state archive manifest.
Human usefulness assessment: **NOT PROVIDED**. The demonstrated technical benefit
is prevention of false verification timeouts. The remaining owner assessment is
human usefulness and suitability for a broader public account of the rehearsal.
No further live attempt is running.
