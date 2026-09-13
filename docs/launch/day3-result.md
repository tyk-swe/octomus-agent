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

## Public static “Explore a run” — 2026-09-13

Added an explicitly selected standalone Svelte/Vite build in `web/showcase/`, using
Day 2's pure evidence/route helpers and existing `EvidenceFact` / `EvidenceText`
components. The authenticated/polling `RunEvidence.svelte`, dashboard, Rust exporter,
backend contracts and dependency versions are unchanged. The public explorer exposes
proposals, both fixed reviewer slots, final rationale, every linked task match,
comparison/output revisions, the latest recorded review/findings, configured checks,
requested session routes and recorded PR references. Fragment record positions preserve
duplicate identities and zero/multiple matches. Gaps and adverse records remain visible.

The build requires an explicit local public wrapper and explicit fixture/recorded mode.
Its allowlist rejects extra/private fields and unsupported or contradictory normalized
facts rather than stripping or repairing them. Recorded mode requires a supplied
owner-review reference and matching SHA-256 of the exact public wrapper bytes; fixture
mode is prominently labeled “Synthetic example — not a real run.” No approval was
granted: recorded-mode tests use temporary, explicitly synthetic test attestations.
The original operator-export warning and all nine limitations remain unchanged.
A hash binds reviewed bytes, not truth or an independent signature.

The output is `dist/showcase/`, separate from `web/build` and the binary. It uses local
assets with no operator API imports/requests, auth headers, token storage, polling,
remote data fetching or executable model-authored HTML. Unsupported PR URLs remain
visible text. See `docs/launch/showcase.md` for the exact public input contract and
local build/preview commands. No full review history, command-output transcript,
complete replay timeline, current GitHub state or merge is invented.

Actual focused validation:

- Installed locked dependencies with `npm ci --prefix web`; no new dependencies or
  upgrades. Installed the existing Playwright Chromium browser prerequisite.
- Seven contract tests passed, including actual fixture/recorded CLI builds, absent or
  mismatched approval, byte changes, malformed/unsupported input, unknown/inconsistent
  statuses, reviewer slots, revisions/checks and adverse/zero/multiple-match retention.
- Six showcase browser tests passed across desktop and mobile on Python's ordinary
  static HTTP server under `/showcase/`, with no Rust service involved. They cover
  fragment refresh/back navigation, missing selections, both reviewers, failed and
  unconfigured checks, findings, hostile text/URLs, recorded-mode provenance, and
  network isolation beyond the operator panel's ten-second polling interval. Only
  document/local JS/CSS requests occurred, with no authorization or browser storage.
- Accessibility and overflow checks passed. Opened and visually inspected ignored
  synthetic overview/adverse captures at 1440×1000 and 390×664 in
  `web/artifacts/showcase/synthetic-*.png`.
- Fixed two defects found during verification: shared TypeScript imports initially
  picked up the dashboard's absent generated SvelteKit config (standalone Vite now
  supplies its own transform config), and selected proposal indices had insufficient
  contrast (darkened). Corrected a test locator's whitespace assumption as well.
- `make check` passed: dashboard build, Rust formatting/clippy, both dashboard and
  showcase Svelte/TypeScript checks (zero errors/warnings), and Prettier. Final CSS/test
  adjustments were formatted and verified by the successful browser pass.
- `make test` was attempted. All 84 Rust tests passed; four opt-in tests were ignored.
  The command then ended with signal 143 during its following debug-binary build, with
  no assertion failure. Resumed `cargo build --locked` passed, as did
  `python3 tests/crate_guards.py`; remaining stage results are recorded below.

Logs are `/tmp/octomus-showcase-*.log`. All observations above use synthetic fixtures.
No live state/accounts/models, pushes, deployment or public approval occurred.

Completed remaining stages after the interrupted aggregate invocation:

- `python3 tests/e2e.py`, `python3 tests/e2e_runners.py`,
  `python3 tests/e2e_hardening.py`, and `python3 tests/distribution.py` all exited 0.
  These used deterministic local runner/GitHub peers, temporary Git repositories,
  embedded-binary HTTP checks and local installer fixtures, not live services.
- `npm test --prefix web` finished with **109 passed and five intentional viewport
  skips** across the existing desktop/mobile dashboard suite (9.3 minutes). Operator
  behavior and Day 2/Day 3 evidence regressions passed unchanged.
- Together with the earlier Rust, crate-guard and showcase passes, every stage of
  `make test` was covered successfully across batches. The interrupted single
  `make test` invocation itself is not reported as an end-to-end pass.

Restored the intended deliverable with the documented explicit command:

```sh
npm run showcase --prefix web -- --mode fixture --input showcase/synthetic.public.json
```

It produced `dist/showcase/` successfully. The exact checked-in synthetic public
payload SHA-256 is
`e539e68bba1ef033eeb8c65e945dc7d556153c1ed0f69e568ca11255176d0ecc`.
The final artifact is fixture-labeled, not the recorded-mode test build. Preview with
`python3 -m http.server 4307 --bind 127.0.0.1 --directory dist` and open
`http://127.0.0.1:4307/showcase/`. No genuine recorded-mode public build is approved;
that still requires the owner's exact reviewed payload and bound approval reference.

### Review fix: duplicate JSON keys

Closed the duplicate-key disclosure bypass in the public build gate. Payload and
approval parsing now checks original JSON text for repeated object member names before
allowlist validation, including escaped-equivalent keys and nested objects in arrays.
The native parser still owns JSON syntax; separate objects may reuse the same key.
Rejected inputs produce no showcase output. Accepted payload bytes and their SHA-256
approval binding remain unchanged; no normalization or reserialization was substituted.

Actual local validation: `make check` passed, and `npm run showcase:test --prefix web`
passed **nine contract tests and six desktop/mobile browser tests**. CLI regressions
cover the overwritten-transcript reproduction in fixture and recorded modes, nested
keys, Unicode-escaped keys, duplicate approval keys, and refusal even with a matching
synthetic approval hash. Valid exact-byte downloads and existing isolation tests passed.
Logs: `/tmp/octomus-showcase-duplicate-{check,test}.log`. No live state or owner approval
was used. The full unrelated Rust/integration/operator behavior suite was not rerun for
this build-only fix; its preceding results remain recorded above.

## First-run path: setup checklist and README — 2026-09-13

Starting commit `1817020`, clean worktree. Read AGENTS.md, README, distribution and
Astra rehearsal docs, `Settings.svelte`, `AstraRehearsal.svelte`, the overview and the
operator browser tests before changing anything. No onboarding subsystem, persistent
readiness schema, scheduler change, dependency, or automatic configuration change was
added; the initial checklist implementation left Rust source untouched. The subsequent
connection-check snapshot correction below updates the doctor API.

Configuration now opens with a compact **Setup checklist** (`web/src/lib/setup.ts`,
`SetupChecklist.svelte`) of five steps: repository details, model routes, verification
policy, connection check, and choosing Audit or Run once. Each step is labelled from
tab-local state only: *Incomplete/None*, *Entered, not saved* (typed in this tab),
*Saved* (the last loaded or saved configuration), *Passed/Failed · mode · time*
(the explicit connection check keyed to the exact saved serialization), and the
latest cycle actually executed from the polled snapshot. Routes report how many of the
ten execution routes (three audit routes) are selected and how many match a catalog
loaded for the entered executable; a match is explicitly not a connection check.
Populated fields, catalog matches and a passed check are never described as proof of
repository push permission or model inference; the doctor endpoint only validates the
origin remote, GitHub CLI login and runner catalogs, and the checklist says so.

The check result is invalidated whenever the saved configuration changes (save,
external refresh), shown as *Unsaved edits* with a "covered the previously saved
values" note while the draft is dirty, restored by Discard, and reset on disconnect,
session expiry or reload because the Settings component unmounts. Checklist links only
move focus to the existing controls (repository path, verification commands, runner
catalog buttons, orchestrator route, Astra effort selector, both check buttons); the
Audit/Run once links navigate to the Overview and focus the existing header control.
Saving, navigating, applying the preset, hiding the checklist and every link never
issue a control request; Run once still drains the queue, audits still plan only, and
continuous operation remains a separate explicit control. Existing draft persistence,
executable-keyed catalogs, unsupported-route errors and active-work restrictions are
unchanged; the Astra preset is reused, and custom/OpenCode routes are untouched until
confirmed.

The README's Getting started section now states the dedicated Ubuntu 24.04 VM and the
owner-supplied Codex/OpenCode and repository-restricted GitHub accounts as
prerequisites, keeps the source-build path, keeps the installer and crates.io options
marked pending (a read-only check on 2026-09-13 found `releases/latest` redirecting to
the empty releases list and crates.io returning 404 for `octomus-agent`; no install
flags were invented), and walks the first run as enter, save, check, then choose. The
public showcase gained a "Run it on your own repository" panel whose only link is the
README first-run anchor with `rel="noopener noreferrer"`; its network-isolation test
still passes because a link is a visitor action. CHANGELOG, the operator checklist and
the Astra rehearsal doc mention the checklist.

Actual local results (synthetic fixtures; no live models, accounts, state, pushes or
publication):

- `npm ci --prefix web` installed the locked dependencies; nothing was upgraded.
- Browser coverage added to `operator-experience.spec.ts`: unconfigured, entered,
  partially configured (three audit routes saved, "Saved, audit routes only"), preset
  applied to the draft, saved, checked, dirty/stale, discarded, invalidated by a saved
  change, failed check, link focus targets, Overview hand-off with focus on Run once /
  Run an audit, hide/show, active task, running audit and continuous restrictions, and
  `Not checked` after disconnect, expiry and reload. Every test asserts that no
  `/api/control` request is made. The focused run of these ten cases passed on
  desktop and mobile (50 s), and again after the partial-configuration additions.
- Full dashboard suite `npx playwright test`: **110 passed, 5 intentional viewport
  skips, 3 failed** in 9.5 min. The three failures were the `captures.spec.ts`
  viewport captures, whose traces were deleted from the shared `test-results`
  directory by a showcase suite I had started concurrently (ENOENT on
  `.playwright-artifacts-0/traces/...`, no assertion failure). Rerun alone with
  `CAPTURE_LABEL=day3-setup`, all three captures, the unconfigured first-run overview
  and the checklist cases passed: **14 passed, 4 intentional skips** (2.2 min),
  including the mobile 44 px target and 16 px input checks over the new controls.
- `npm run showcase:check` and `npm run showcase:test` rerun alone: nine contract
  tests and **6 browser tests passed** (39.8 s), including the new CTA assertions and
  the unchanged network-isolation window.
- Opened synthetic captures of the checklist at 1440×1000 and iPhone 13 width
  (scratchpad only, not tracked): steps, badges and links wrap cleanly with no
  horizontal overflow.
- `make check` passed: dashboard build, Rust formatting/clippy, dashboard and showcase
  Svelte/TypeScript checks (zero errors/warnings) and Prettier.
- `cargo build --locked` re-embedded the dashboard, then `python3 tests/distribution.py`
  passed both stages (embedded binary HTTP/JS/SPA/override/listener checks and the
  local installer fixtures across architectures and failure modes). This is a local
  fixture package check, not dedicated-deployment acceptance.
- `cargo test --locked` passed (84 tests, 4 opt-in tests ignored) and
  `python3 tests/e2e.py` passed every scenario, including the audit cases that keep
  the queue paused with no publication. Rust source was not changed; these confirm the
  re-embedded dashboard did not disturb the service. `make test` was not run as a
  single invocation; its stages above were covered individually except
  `e2e_runners.py`, `e2e_hardening.py` and `crate_guards.py`, which exercise
  unchanged Rust behaviour.

Unresolved gates are unchanged: no live validation, no owner VM/bot deployment, no
public release or crate, and no owner-approved recorded showcase payload.

### Connection-check snapshot correction

The doctor API now returns the exact configuration snapshot it checked on both success
and failure. The dashboard binds the result to that snapshot using an identity that
ignores object-key ordering. A save in another tab before or during a check therefore
cannot label different form values as checked. Transport failures clear the prior
result. Integration assertions cover the returned snapshot, and browser cases cover
external saves, reordered keys and transport failures for execution and audit checks.

Final shipment validation on September 13 ran `make check test` successfully as one
invocation, including Rust checks and tests, all three integration suites, distribution
and crate guards, and both browser suites. The dashboard browser suite passed 117 tests
with five intentional viewport skips; the showcase passed nine contract tests and six
browser tests. This supersedes the partial-suite coverage recorded above and remains
local fixture validation, not live deployment acceptance.
