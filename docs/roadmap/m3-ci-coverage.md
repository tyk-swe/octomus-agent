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
Status: TODO
Revision: Not started.
Delivered output: None.
Commands and results: Not run.
Unrun checks and blockers: All checks unrun.
Next: M4 after M3 is DONE.
```
