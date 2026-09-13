# Day 3 result — evidence polish

Starting commit: `877ff88e0d5d0a0658f9e46b52c45565a0ef60a4`; starting worktree clean.
Read AGENTS.md, Day 2 results, overview/task evidence components and evidence/operator
browser tests. Both reported defects remained in source. Existing stable list
positions, draft retention, navigation and touch sizing were already implemented.

The overview now labels counts as recent-window observations, explicitly says they
are not cycle totals, and directs operators to Inspect run for complete retained
run evidence. No recent matches now allows for older tasks. No new polling added.
Task evidence failures retain their cycle/task/revision key: 404 and transient
errors remain unavailable until explicit Retry or a changed saved task revision.
Retry is disabled while pending. Existing stale labels, 401 clearing, cancellation
and late-response guards remain. Transient errors intentionally use explicit retry,
not automatic backoff. Regression tests cover both errors, pending retries,
revision recovery, and complete evidence outside a populated or empty recent window.

Actual local checks:

- Installed locked dashboard dependencies with `npm ci --prefix web`; no upgrades.
- `make check` passed: dashboard build, Rust formatting/clippy, Svelte/TypeScript
  (zero errors/warnings), and Prettier. `cargo build --locked` passed for fixtures.
- From `web`, ran `CAPTURE_LABEL=day3 npx playwright test tests/run-evidence.spec.ts
  tests/operator-experience.spec.ts tests/captures.spec.ts`. It ended with signal
  143 after 72 passes and five intentional viewport skips, without assertion failures.
  Removed its orphaned local fixture server after an occupied-port retry failed.
  Ran the remaining 15 mobile evidence cases successfully: 87 distinct passes total,
  five intentional skips across batches, including list positions and draft retention.
- After adding keyboard-focus/mobile Retry sizing assertions, reran
  `CAPTURE_LABEL=day3 npx playwright test tests/captures.spec.ts --project=desktop
  --grep 'synthetic captures at'`: three passed. Prettier checked the changed test.

Opened and visually inspected synthetic captures at 390×844, 1280×800 and 1440×1000:
populated overview/task evidence, missing evidence, stale retained results and errors,
including regenerated Retry keyboard-focus captures. Text wraps, stale labels and
focus rings remain clear; no concrete overflow or styling defect warranted CSS
changes. Browser overflow/accessibility checks and the mobile 44px Retry target
assertions also passed. Green/off-white styling and existing controls are preserved.
Captures are ignored, outside tracked source: `web/artifacts/captures/day3/`, with
explicit `synthetic-<width>x<height>-...png` filenames. Logs: `/tmp/octomus-day3-*.log`.
These are synthetic fixtures, not live evidence.

Unresolved gates: full `make test` was not run; no live validation or owner
VM/bot deployment/usefulness acceptance. Day 2's evidence limitations remain.
No live state/accounts/models, dependency upgrades, pushes or publication occurred.
