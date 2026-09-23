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
Its schema version is 1. `daily` includes the daily budget counter and attributed
admissions. The `unattributed_admissions` field remains in the report shape and
is zero for version-7 state. `cycles` includes
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
atomically. Existing counters from before this ledger remain unattributed.

`recorded_completed_sessions` counts persisted thread records, not turns: repeated
repairs share one record, and interrupted persistence can leave incomplete session
metadata. Do not use thread counts to estimate billing. The report makes no
provider charge or merged-PR inference. It does not collect tokens, account
allowance, raw transcripts or prices; capture account observations separately.
