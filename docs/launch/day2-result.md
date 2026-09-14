# Day 2 result — run evidence inspection and export (2026-09-13)

**DRAFT — owner review required before public sharing.** This document describes code
validated against explicitly synthetic fixtures. No live model session, remote Git,
account, billing or `.octomus/` state was touched, and no Day 1 raw evidence was loaded.

## Delivered

- `RunEvidenceV1` (`src/evidence.rs`): a read-only model of what one planning cycle and
  its tasks actually saved. Reviewer verdicts are positional per slot and reported as
  `recorded`, `missing`, `duplicate` or `malformed`; tasks join on `(cycle_id, proposal_id)`;
  the latest saved review round and the latest result per configured command govern.
- `GET /api/cycles/{id}/evidence` inside the existing authenticated router, and
  `octomus-agent --data-dir <dir> --export-run <cycle-id>` on the CLI. Both use one
  assembler and one single-transaction snapshot reader.
- Dashboard: an **Inspect run** panel (`RunEvidence.svelte`), a **Recorded result** summary
  in task details, a latest-run summary on the overview, and session-wide request abort
  and 401 handling in `api.ts`, with synthetic browser fixtures and screenshot captures.
- The field contract and operator procedure are in [run-evidence.md](run-evidence.md).

## Defect found and fixed

`TaskEvidence.blocked_reason` was built from the Rust `Debug` name lower-cased
(`verificationfailed`), while `GET /api/tasks/{id}` serializes the same saved value as
`verification_failed`. The export now emits the serde token, so one saved reason has one
spelling in the task view, the run panel, the API and the CLI. Regression test:
`blocked_reason_uses_the_task_api_vocabulary` in `tests/evidence.rs`.

## Validation performed

All fixtures are synthetic temporary databases or route-mocked responses.

- `tests/evidence.rs` (17 integration tests) and the `evidence` unit tests cover repeated
  proposal IDs across cycles, missing/duplicate/malformed/unconfirmed/extra reviewer
  batches, incomplete or blank-summary reviews, later failures and mismatched revisions,
  audit acceptance without tasks, omission of private fields, and read-only CLI export
  that errors explicitly on a missing database or cycle.
- A scratch server probe confirmed authentication and method enforcement on the evidence
  route (401 without or with a wrong token, 405 for POST/PUT, 404 for unknown or
  traversal cycle IDs), no record or event mutation across repeated reads, and CLI/API
  agreement apart from `generated_at`.
- Hostile text and URLs (`<img onerror>`, `javascript:`) are carried verbatim as data; the
  dashboard renders text nodes only and reduces non-GitHub URLs to `#`.
- `make check` and `make test` passed on the final tree: 83 Rust tests, 61 integration
  scenarios and 70 desktop/mobile browser tests with 4 intentional skips.
- Not run: `make audit`, the ignored pinned-client contracts and smoke tests, the
  history-scale benchmark, `tests/systemd.py` and `tests/crate.py`.

Screenshot captures from `web/tests/captures.spec.ts` land in `web/artifacts/captures/`
(Git-ignored). Every file name starts with `synthetic-`; the images are not evidence of a
real run and are not part of the built dashboard or the binary.

## Known limitations

The durable limits of the export, including the WAL-copy hazard and the joins it cannot
make, are recorded in [run-evidence.md](run-evidence.md). Unchanged from Day 1: runtime
model identity is unreported in retained records, cost is unavailable, and stored
verification output is capped at 16 KiB.

## Real archive data

**NOT PROVIDED.** The Day 1 narrative in [day1-result.md](day1-result.md) carries no raw
evidence archive, and no read-only snapshot with explicit access permission was supplied.
No raw record was fabricated from the narrative and no run was launched for this pass. A
directory matching the Day 1 archive layout exists on the development workspace; its
location was reported to the owner separately and it was not opened. Only an
owner-specified offline snapshot or export with explicit read permission may be used.
[day3-result.md](day3-result.md) records the separately authorized new run that supplied
the showcase candidate instead.

## Remaining owner decisions

- Grant or withhold access to a copy of the Day 1 state archive for a real export.
- Review any real export (free text is model-authored) before it leaves its private
  location.
- Decide on the public account of the rehearsal and on human usefulness.
