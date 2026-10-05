# M3 — CI coverage

[Roadmap](README.md) · **Depends on:** — · **Enables:** [M4](m4-v0.2.0.md) ·
**Target:** Fri 2026-11-06

## Why

CI runs on every push and pull request, and its `verify` job requires every other
job (`.github/workflows/ci.yml`). Some checks still only run on a tag, or never
run automatically:

- The `package` job builds and checks only the x86_64 archive. The aarch64 archive
  is built only by `release.yml`, so an arm64 break first shows up on a tag.
- The scale check (`OCTOMUS_SCALE_TEST=1`) skips unless its environment is set.
  It was last measured on 2026-09-22 at `529b63f` ([architecture](../architecture.md)).
- `make test-race-e2e`, the e2e scenarios against a race-instrumented build, is
  opt-in.
- One browser spec, `task-activity-latency.spec.ts`, exists only on the unmerged
  `tyk/audit-task-activity-reads` branch.

## Deliverable

Pull requests check both release archives. A scheduled job runs the scale check
and race-instrumented e2e every week. The one stray spec is either merged or
dropped.

## Work

1. **Both archives in CI.** Extend the `ci.yml` package job to build and check the
   aarch64 archive the way `release.yml` does, with `tests/distribution.py
   --package` on each archive.
2. **Weekly scheduled workflow.** Run the store scale check and
   `make test-race-e2e` on a schedule, and on demand through manual dispatch.
   When a budget or measurement changes, update the table in
   [architecture](../architecture.md).
3. **The stray spec.** If `task-activity-latency.spec.ts` covers a case `main`
   lacks, fold it into `web/tests/controls.spec.ts`. Otherwise record why it is
   redundant and drop the branch.
4. **Owner housekeeping.** Prune the remote `tyk/*` branches already merged into
   `main`.

## Acceptance criteria

1. A pull request that breaks the aarch64 archive fails CI.
2. The scheduled workflow passes on two consecutive runs, and a failure is visible
   on the Actions page.
3. The decision on `task-activity-latency.spec.ts` is recorded, with the spec
   merged or the branch closed.
4. `verify` still requires every CI job, including the extended package job.

## Verification

- A deliberately broken aarch64 build on a throwaway branch fails the package job.
- Workflow run URLs for the two scheduled passes.
- `make test-browser` passes if the spec is merged.

## Progress record

```text
Milestone: M3
Status: DONE
Revision: a53606d0495fb28fc4d5952c60bc59c873b5bc12.
Delivered output: CI packages and smoke-tests each archive on its native Ubuntu
24.04 runner. verify still gates the complete package matrix. weekly.yml runs
the 100,000-record scale check and all race-instrumented fixture e2e scenarios
each Monday at 06:17 UTC, with manual dispatch for acceptance verification.
The activity-latency branch adds coverage main lacks: task controls appear while
the initial activity read is held, and accepted cancellation waits for the
canonical task response while a delayed activity read cannot delay new controls.
Both cases are folded into controls.spec.ts, using the shared synthetic helpers.
They exposed TaskDetail's task/activity Promise.all: it held canonical controls
behind activity. Task and activity now refresh independently, with separate
activity loading/errors and a regression case for activity outage recovery.
Commands and results: make check passed. The scale check passed at all three
history sizes; architecture.md records the new 2026-10-05 measurements.
make test passed (regular and race Go, distribution, all ten fixture e2e
scenarios, and 58 browser cases; six non-applicable mobile evidence-unit cases
remain intentionally skipped). make audit and make package passed, as did the
native x86_64 archive smoke test.
Repository CI passed, including both native package jobs and verify:
https://github.com/tyk-swe/octomus-agent/actions/runs/37278189021
The throwaway tyk/roadmap-native-arm64-negative commit f8d468a deliberately
corrupted only the arm64 archive. Its arm64 smoke test and verify failed while
the x86_64 package and every other job passed:
https://github.com/tyk-swe/octomus-agent/actions/runs/37278271540
Two consecutive manual dispatches of the scheduled weekly workflow passed
both scale and race-e2e (the Monday schedule is configured, not claimed to have
already fired twice):
https://github.com/tyk-swe/octomus-agent/actions/runs/37278265977
https://github.com/tyk-swe/octomus-agent/actions/runs/37278782824
Pruned 104 ancestry-merged remote tyk/* branches with atomic exact-head leases.
The two unique latency cases are incorporated and their branch had no PR; it
was deleted with an exact-head lease. The negative-test branch was also removed.
Unrun checks and blockers: None.
Next: M4.
```
