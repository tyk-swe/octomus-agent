# Cost and usage

Octomus publishes no live cost measurements and makes no per-task dollar claim.
Admissions are not charges, and the service never infers provider billing. Set
spending limits on your provider account, and capture usage and fees from the account
itself. A missing charge measurement is **unavailable**, not zero.

## What is counted

`octomus-agent --data-dir PATH --usage-report` exports a read-only JSON snapshot.
Its schema version is 1. `daily` includes the daily budget counter and attributed
admissions; `has_admission_ledger` (always true) and `unattributed_admissions` (zero)
stay in the report shape. `cycles` includes
planning wall time (not subsequent task execution), mode (audit/execution), status,
per-decision proposal counts, planning admissions and task admissions associated
with that cycle. `tasks` includes execution tier, saved routes, admissions, status
and PR URL. `tiers` counts observed tasks and their admissions. `admissions`
retains each reservation's UTC timestamp, cycle, optional task, role and exact
route, including the runner and any OpenCode provider/variant.

An **admission** reserves budget before starting work. Failed thread starts,
failed clone setup and interrupted attempts can consume admissions without a
completed provider turn. Each repair turn and retried executor reserves again,
even if the runner session is reused. The daily counter and ledger entry commit
atomically.

`recorded_completed_sessions` counts persisted thread records, not turns: repeated
repairs share one record, and interrupted persistence can leave incomplete session
metadata, so do not use thread counts to estimate billing. The report makes no
provider charge or merged-PR inference and collects no tokens, account allowance, raw
transcripts or prices.

The dashboard's **Insights** view (`GET /api/insights?days=7|30|90`) shows the same
measurements over a rolling window: admissions reserve budget before work starts — they
include failed starts and retries and are not completed turns or billed usage; task
outcomes are the current status of tasks created in the window; proposal decisions count
execution cycles started in the window; and pull-request states are the latest
observation, not live GitHub state.

Admissions per created task uses only window admissions for tasks created in that
window. Total tier admissions also include work and retries on older tasks.
