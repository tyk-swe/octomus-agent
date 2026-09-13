# Day 2 result — run evidence inspection and export (2026-09-13)

**DRAFT — owner review required before public sharing.** This document describes code
validated against explicitly synthetic fixtures. No live model session, remote Git,
account, billing or `.octomus/` state was touched, and no Day 1 raw evidence was loaded.

## Implemented capabilities

- `RunEvidenceV1` (`src/evidence.rs`): a read-only model of what one planning cycle and
  its tasks actually saved. Reviewer verdicts are positional per slot and reported as
  `recorded`, `missing`, `duplicate` or `malformed`; tasks join on `(cycle_id, proposal_id)`;
  the latest saved review round and the latest result per configured command govern.
- `GET /api/cycles/{id}/evidence` inside the existing authenticated router, and
  `octomus-agent --data-dir <dir> --export-run <cycle-id>` on the CLI. Both use one
  assembler and one single-transaction snapshot reader.
- Dashboard: an **Inspect run** panel (`RunEvidence.svelte`), a **Recorded result** summary
  in task details, a latest-run summary on the overview, session-wide request abort and
  401 handling in `api.ts`, synthetic browser fixtures and screenshot captures.
- Documentation: `docs/launch/run-evidence.md`.

## Changed files

Day 2 commits `1b72737..c6bc667` (25 files) plus this validation pass:

| Area | Files |
| --- | --- |
| Rust | `src/evidence.rs` (new), `src/api.rs`, `src/lib.rs`, `src/main.rs`, `src/store/queries.rs`, `tests/evidence.rs` (new) |
| Dashboard | `web/src/lib/evidence.ts`, `RunEvidence.svelte`, `TaskDetail.svelte`, `EvidenceFact.svelte`, `EvidenceText.svelte`, `Sha.svelte`, `Icon.svelte`, `api.ts`, `clipboard.ts`, `types.ts`, `web/src/routes/+page.svelte`, `web/src/app.css` |
| Browser tests | `web/tests/run-evidence.spec.ts`, `captures.spec.ts`, `synthetic.ts` (new), `dashboard.spec.ts`, `tests/serve_ui.py` |
| Other | `.gitignore` (`/web/artifacts/`), `docs/launch/run-evidence.md` |
| Validation pass (2026-09-13) | `src/evidence.rs` (blocked-reason vocabulary fix), `tests/evidence.rs` (regression test), `docs/launch/run-evidence.md` (copy caveats), this file |

Day 1 files (`src/process.rs`, `src/codex.rs`, `tests/process_lifecycle.rs`, launch docs)
are untouched by Day 2.

## Defect found and fixed

`TaskEvidence.blocked_reason` was built from the Rust `Debug` name lower-cased
(`verificationfailed`), while `GET /api/tasks/{id}` serializes the same saved value as
`verification_failed`. The export now emits the serde token, so one saved reason has one
spelling in the task view, the run panel, the API and the CLI. Regression test:
`blocked_reason_uses_the_task_api_vocabulary` in `tests/evidence.rs`.

No other release-blocking defect was reproduced. Probe findings that are documented
rather than changed are listed under *Known gaps*.

## Validation actually performed

All fixtures are synthetic temporary databases or route-mocked responses.

| Check | Command | Result |
| --- | --- | --- |
| Evidence integration tests | `cargo test --locked --test evidence` | 16 passed before the fix; 17 passed after |
| Evidence unit tests | `cargo test --locked --lib evidence` | 3 passed |
| Reused proposal IDs across cycles | `repeated_proposal_ids_across_cycles_do_not_cross_runs` | Each cycle links only its own task |
| Missing, duplicate, malformed, unconfirmed, extra batches | four dedicated tests | Never reconstructed into a verdict; slots never shift |
| Zero findings with `completed=false` or blank summary | `latest_review_governs_and_incomplete_or_empty_summaries_are_not_clean` | `clean=false` |
| Later failure, missing command, other-revision pass | `later_failures_missing_results_and_mismatched_revisions_are_not_passing` | `failed`, `no_result`, `passed_at_other_revision`; `all_passed_at_output_revision=false` |
| Audit acceptance without a task | `audit_acceptance_without_tasks_is_not_execution` | No execution claim, no gap by design |
| Private fields | `private_fields_are_omitted_from_the_export` | Prompt, transcript, workspace, binary path, command output, error and review summary text absent |
| CLI read-only and explicit failure | `cli_export_is_read_only_and_errors_explicitly`, `export_run_flag_returns_before_touching_application_state` | Bytes, listing and record count unchanged; missing DB or cycle is an error; no data dir or lock created |
| Route auth and method | scratch server probe (`--listen 127.0.0.1:4311`, synthetic DB) | No token 401, wrong token 401, GET 200, POST 405, PUT 405, unknown cycle 404, traversal 404 |
| No mutation while browsing | same probe, 6 GETs then compare | Record count/bytes and event count unchanged; CLI export while the service ran matched the API apart from `generated_at` |
| Hostile text and URLs | CLI probe with `<img onerror>`/`javascript:` values | Carried verbatim as data; the dashboard renders text nodes only and `safeUrl` reduces non-GitHub URLs to `#` (browser probe: no `img`/`script` element created, no dialog, no page error) |
| Browser: run evidence and captures | `npx playwright test tests/run-evidence.spec.ts tests/captures.spec.ts` (desktop + mobile) | 48 passed, 4 skipped (captures run once per viewport), 0 failed |
| Temporary probe (deleted after the run, desktop only) | hostile text/URLs in both panels; keyboard-only path; existing **Retry task** action | 2 passed: no `img`/`script` element created, no dialog, PR links reduced to `#`, focus returns to **Inspect run**, retry issued exactly one `POST /api/tasks/task-blocked/retry` |
| `make check` (final tree) | `make check` | Exit 0: dashboard build, `cargo fmt --check`, `cargo clippy --all-targets -- -D warnings`, `svelte-check`, Prettier |
| `make test` (final tree) | `make test` | Exit 0: 83 Rust tests passed (17 evidence, 4 `process_lifecycle`, 4 ignored opt-in), 61 integration scenarios passed (`e2e.py`, `e2e_runners.py`, `e2e_hardening.py`, `distribution.py`, `crate_guards.py`), 70 browser tests passed and 4 skipped across desktop and mobile, including the Astra rehearsal preset, configuration drafts and task actions in `dashboard.spec.ts` |

### Opt-in checks not run

`make audit` (`cargo audit`, `npm audit`), the ignored `tests/contracts.rs` and
`tests/runners.rs` smoke tests (need pinned real client binaries), the ignored
`history_scale` allocation test, `tests/systemd.py` and `tests/crate.py`. No real client,
model, GitHub or billing call was made.

## Screenshots

Generated by `web/tests/captures.spec.ts` into `web/artifacts/captures/after/` (43 PNG
files, Git-ignored via `.gitignore` `/web/artifacts/`, confirmed with `git check-ignore`).
Every file name starts with `synthetic-` and covers 1440×1000, 1280×800 and 390×844:
populated overview, keyboard focus on **Inspect run**, run evidence for a published,
an unattributed and a mismatched proposal (viewport and full-height), stale and failed
evidence states, task evidence and verification tabs, and the unconfigured overview.

Provenance: Playwright Chromium against `tests/serve_ui.py` (synthetic SQLite records)
with route-mocked evidence from `web/tests/synthetic.ts`. All reviewer, proposal and
event text is prefixed "Synthetic fixture" or "Synthetic browser-test". **These images are
not evidence of a real run** and are not part of the built dashboard or the binary
(`include_dir!` embeds only `web/build`, which was scanned and contains no export, state
path or secret).

Inspected during this pass: 1440 overview, published/mismatch/missing full panels, task
evidence; 390 overview, evidence and task evidence viewports. Labels read "Planning
complete" (never "work complete"), "Published describes delivery, not merge", "requested
routes, not verified runtime identity", "Review incomplete", "0 of 3 passed at the output
commit", "Malformed batch", "Acceptance is not execution". No horizontal overflow.

## Known gaps

- **Copying only `state.db` loses WAL records silently.** The database runs in WAL mode.
  A copy without `state.db-wal`/`state.db-shm` exported cleanly on a synthetic fixture but
  omitted a task written after the last checkpoint, reporting "accepted but no task is
  linked". The exporter cannot detect this. Copy all three files or export from the
  original directory (documented in `run-evidence.md`).
- A copy inside a directory the exporting user cannot write to fails with
  `attempt to write a readonly database` (SQLite WAL index). Explicit, not silent.
- Tasks whose saved proposal ID is not among the cycle's proposals are only counted in
  the run-level gaps; their review/check evidence is not exported.
- The overview's "Tasks from this run" counts come from the dashboard's recent task
  window (up to 300 records), not from the evidence scan. The evidence panel itself uses
  the direct `cycle_id` scan.
- Task details retry the evidence request on every 4-second poll while the cycle record
  is missing (for example after retention). Read-only, but repeated.
- Unchanged from Day 1: runtime model identity is unreported in retained records; cost is
  unavailable; stored verification output is capped at 16 KiB.

## Export usage

```
octomus-agent --data-dir <dir-containing-state.db> --export-run <cycle-id> > <private-dir>/run-<cycle-id>.json
```

Minimum input: a directory with `state.db` (plus `-wal`/`-shm` when present) and the
cycle ID. The command returns before directory creation, permission changes, the service
lock, migrations or worker start. Write the output only to a private location; nothing
under `web/static`, `docs/` or any other Git path.

## Real archive data

**NOT PROVIDED.** The uploaded source carries the Day 1 narrative but not its raw evidence
archive, and no read-only snapshot with explicit access permission was supplied for this
pass. No raw record was fabricated from the narrative and no run was launched. A
directory matching the Day 1 archive layout exists on this development workspace; its
location is withheld here and reported to the owner separately. It was not opened.

If the owner grants access to a **copy**, the expected comparison against
`day1-result.md` is: execution cycles `1a25930b-4c9e-4bb3-9b0c-1ac6eee09936` (13
sessions, task `73aaf60b-cedc-49a1-b8ce-374022c4afe7` blocked before its first executor
turn) and `aebbd918-063b-440e-97c9-1758c93b36b0` (proposal
`rediscover-73aaf60b-cedc-49a1-b8ce-374022c4afe7`, task
`85231f37-a1a0-41c9-aa09-901ad2e3980f`, both reviewer slots `recorded`/`accepted`, four
deferred), reviewer session `01a096c2-6151-7b43-97f3-e266b7e4bbfc` clean at output
revision `06bdf7a8…`, comparison base `a2f8f628…`, PR #3 as a recorded reference only.

## The Day 1 story, unchanged by Day 2

- The first attempt blocked at executor startup (`thread/resume`: no rollout found).
- A development fix and a supported **Supersede and rediscover** preceded the successful
  second attempt.
- Both proposal reviewers accepted; four proposals were deferred (deferred is not rejected).
- Live repair was not needed.
- Cost is **UNAVAILABLE**.
- Independently reported runtime model identity is **UNREPORTED**; saved routes are
  requested routes.
- Human usefulness approval and dedicated-deployment acceptance are **NOT PROVIDED** and
  are not implied by any export or screenshot.

## Remaining owner approval

- Grant or withhold access to a copy of the Day 1 state archive for a real export.
- Review any real export (free text is model-authored) before it leaves the private
  location. No export is approved for sharing by this pass.
- Decide on the public account of the rehearsal and on human usefulness.
