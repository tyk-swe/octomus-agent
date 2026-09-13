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
