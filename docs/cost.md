# Cost and usage

**Live measurements are pending.** No day-price or per-task dollar claim has been
validated. Repository preparation and fixture tests do not establish live usage
or billing evidence.

Use existing provider subscription allowance only, with no paid overage.
Record real usage measurements, the subscription fee and verified incremental
charges when per-task dollar attribution is unavailable. A missing charge
measurement is **unavailable**, not zero.

## What is counted

`octomus-agent --data-dir PATH --usage-report` exports a read-only JSON snapshot.
Its schema version is 1. `daily` includes the original daily budget counter,
attributed admissions and historical unattributed admissions. `cycles` includes
planning wall time (not subsequent task execution), mode (audit/execution), status, proposal counts,
planning admissions and task admissions associated with that cycle. `tasks`
includes execution tier, saved routes, admissions, status and PR URL. `tiers`
counts observed tasks and their admissions. `admissions` retains each reservation's
UTC timestamp, cycle, optional task, role and exact route, including the runner
and any OpenCode provider/variant. Legacy routes are reported as Codex.

An **admission** reserves budget before starting work. Failed thread starts,
failed clone setup and interrupted attempts can consume admissions without a
completed provider turn. Each repair turn and retried executor reserves again,
even if the runner session is reused. The daily counter and ledger entry commit
atomically. Existing counters from before this ledger remain unattributed.

`recorded_completed_sessions` counts persisted thread records, not turns: repeated
repairs share one record, and interrupted persistence can leave incomplete session
metadata. Do not use thread counts to estimate billing. The report makes no
provider charge or merged-PR inference. It does not collect tokens, account
allowance, raw transcripts or prices; capture account observations separately.

## Unmeasured planning arithmetic

With nine discovery agents, a fully completed planning pass normally admits
13 sessions: one grounding, nine discovery, two adversaries and one consolidation.
An audit uses the same planning admissions and queues no tasks; it is not free.
A task with no retries admits two turns when the first review is clean; four
repair rounds raise this to ten (one executor, five reviewers, four repair turns).
Repair rounds are budgeted per attempt: an explicit retry starts a fresh round
budget from the reviews already recorded. Failed starts and retries alter these counts. These are workflow calculations,
not measured provider consumption or dollar prices.

The conservative six-hour cadence in the [operator checklist](operations.md)
allows at most four starts in a rolling 24 hours when planning takes nonzero time;
queue processing may reduce this.
Four completed planning passes imply 52 planning admissions, before task work.
Shipped defaults remain 30 minutes, two concurrent tasks, five tasks per cycle
and 150 daily admissions until live validation supports changing them. At those
defaults, eleven complete 13-admission planning passes consume 143 admissions;
the next pass can exhaust the daily budget partway through. No dollar cap follows
from this admission limit.

## Measurement table

Fill this from daily operating logs, report exports and owner account evidence.
Keep logs in UTC with cycle IDs, durations, admission counts, proposal outcomes,
blocked-task resolutions and PR links. Store raw account evidence privately.
Separate idle planning, task-only work and whole-day totals. Group task results by
tier **and exact saved route**, with sample counts and completed/blocked outcomes.
Do not force unnecessary L/XL work to populate a table.

| Workload | Live samples | Admissions (range / mean) | Allowance consumption | Attributable dollars |
| --- | --- | --- | --- | --- |
| Idle cycle | 0 | Unmeasured | Unavailable | Unavailable |
| XS task | 0 | Unmeasured | Unavailable | Unavailable |
| S task | 0 | Unmeasured | Unavailable | Unavailable |
| M task | 0 | Unmeasured | Unavailable | Unavailable |
| L task | 0 | Unmeasured | Unavailable | Unavailable |
| XL task | 0 | Unmeasured | Unavailable | Unavailable |
| Day at conservative profile | 0 | Unmeasured | Unavailable | Unavailable |
| Day at shipped defaults | 0 | Unmeasured | Unavailable | Unavailable |

| Billing fact | Observed value / evidence |
| --- | --- |
| Subscription plan, fee, currency and billing period | Pending owner evidence |
| Observation dates and timezone | Pending commissioning; log in UTC |
| Incremental charges during observation | Unavailable until verified |
| Other activity using the same allowance | Record explicitly; shared usage prevents attribution |
| Overage disabled | Owner verification required before cycles start |

Record before/after allowance observations with reset boundaries and concurrent
account activity where known. Do not divide a monthly subscription fee by task
count and label it a measured marginal price. Report verified zero incremental
charges separately from the nonzero subscription fee. Label any projection with
its measured inputs and assumptions; never substitute it for a measured day.
