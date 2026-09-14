# Day 3 result — evidence polish and showcase (2026-09-13)

**DRAFT — owner review required before public sharing.** Everything below was validated
against explicitly synthetic fixtures unless a section says otherwise.

## Real-run showcase input

**DATA READY — owner-approved public bytes staged locally on 2026-09-14.** The owner
explicitly approved the exact SHA-256 below; the literal reply, `approve.`, is the public
`approval_reference`. Nothing was published or deployed. The Day 1 raw evidence archive
remains unopened, and human usefulness approval is absent.

After a single input/permission request, the owner instructed a temporary Octomus run on
this machine against this repository, followed by cleanup. One supported Run once was
executed on the committed application/target baseline
`79bddb5433b2e271f83c0f5ccd8862269ff00431`, using pinned Codex 0.153.4 and exact
Codex / `gpt-6-astra` / `medium` routes. No historical archive was searched for or
accessed, and no Day 1 record was reconstructed.

The new task, **Require exact, unambiguous adversarial assessment batches**, completed
execution and a fresh code review with zero findings. The saved required command
`npm ci --prefix web && make check && make test` passed at the same revision as the clean
review. Publication was then refused by the temporary remote-write hold: the task is
**blocked**, `blocked_reason: publication_uncertain`, `error_recorded: true`, with no PR
reference and no repair session. These adverse facts remain in the candidate; this is not
a published-success record.

| Comparison | Day 1 historical report | New retained evidence |
| --- | --- | --- |
| Cycle / task | `aebbd918-063b-440e-97c9-1758c93b36b0` / `85231f37-a1a0-41c9-aa09-901ad2e3980f` | `197ff951-c22f-47ea-9ee0-8ae33a7d8c47` / `1b9f2721-10b2-43ee-b604-f034e55aa752`; different records and work. |
| Proposal decisions | One accepted, four deferred; both reviewers accepted the selected proposal. | One accepted, **five deferred, one rejected**; both fixed reviewer slots accepted `d4-assessment-contract`, with recorded reasons and no unconfirmed notes. |
| Output / review / checks | Reported output `06bdf7a81dde2bb932e9d18b102568423493690d`. | Output, clean latest review and successful required command all identify `49851c34c2479a0f3ba46e7ff37055784276107c`. |
| Publication | Historically reported PR #3. | Publication held locally; blocked task and null PR. No fresh observation of PR #3 was made. |
| Failure / intervention sequence | First-attempt startup failure, development fix, supported Supersede and rediscover, then publication. | No corresponding sequence in this new run. The historical first cycle/task and intervention records were not supplied; the narrative remains unvalidated. |
| Repair | No live repair reported. | No repair session recorded; no repair intervention performed. |
| Cost / runtime identity / human usefulness | Unavailable / unreported / not provided. | Still unavailable / unreported / not provided. Requested routes do not establish independent runtime identity. |

The private `candidate.public.json` is **23,365 bytes**, with exact-byte SHA-256:

`c958b2b0975d0344152779fb58d79e7ac02190fe7bcc8771e0a53e9ec04c1a3b`

The recorded-mode showcase build accepted the supplied owner approval. The staged
`dist/showcase/public-run.json` is byte-for-byte identical to the private candidate, and
its emitted approval metadata matches the supplied record. Every exported field, record,
order, null, gap, adverse state, warning and all nine original limitations are retained;
no additional string redaction was needed. Three added limitations identify the new run's
historical coverage limit, withheld publication, and missing cost/runtime/usefulness/
deployment evidence. Model-authored proposal claims remain visible.

Provenance: after observing paused/idle state, the temporary service was stopped and its
exit, child-process exit and closed listener were checked. The stopped database was copied
to a private original snapshot and a disposable working copy; only the existing
`--export-run` interface produced the raw export. Snapshot SHA-256:
`d8672e96cb5c13b99821d0eeb45289178e6221ffb2c930db764584854306d2bf`;
raw export SHA-256:
`a7e61ff9ceb8d52b0f74fbb9747897cd8178106de6b6c3c40127b2e85a0b8746`.
The original snapshot was unchanged after export. A private review packet records field
references, redactions, limitations, source/export hashes, authorization and cleanup
evidence.

Cleanup removed the temporary service, instance/workspaces, clone, pinned tools, launcher
helpers and token. Private evidence and a Git bundle of the generated output were retained
outside Git and public build roots. Existing account configuration and history were
preserved. Fifteen sessions were admitted; admissions are not dollars. The observed
request-to-final-idle span was 4,306.29 seconds, including observation delay; this is
private operator timing, not a delivery time inferred from the export.

Local staging is complete. Nothing has been pushed, published or deployed.
Dedicated-deployment acceptance is not established. Any change to the approved payload
invalidates approval. Human usefulness approval remains separate and was not granted.

## Implementation summary

Delivered on 2026-09-13 from starting commit `877ff88`, in this order. Durable behavior is
described in the linked documentation, and [CHANGELOG.md](../../CHANGELOG.md) carries the
user-facing entries.

- **Evidence retries and recent-window counts.** The overview labels its counts as
  recent-window observations and directs operators to Inspect run for complete retained
  evidence. Task evidence failures keep their cycle/task/revision key and stay unavailable
  until an explicit Retry or a changed saved task revision; transient errors use explicit
  retry, not automatic backoff.
- **Public static showcase** (`web/showcase/`, [showcase.md](showcase.md)): a standalone
  Svelte/Vite build of a public `RunEvidenceV1` wrapper with explicit fixture/recorded
  modes, an allowlist gate that rejects rather than repairs input, hash-bound owner
  approval for recorded mode, duplicate-JSON-key rejection at every depth, and network
  isolation (local assets only, no operator API, no polling).
- **First-run path.** Configuration opens with a **Setup checklist** (`web/src/lib/setup.ts`,
  `SetupChecklist.svelte`) that labels repository details, routes, verification policy, the
  connection check and the Audit/Run once choice as entered, saved, checked or ran from
  tab-local state, links to the existing controls and never issues a control request. The
  README first-run section was rewritten around enter, save, check, then choose, and the
  showcase gained a "Run it on your own repository" panel linking to it.
- **Connection-check snapshot.** The doctor API returns the exact configuration snapshot it
  checked on success and failure; the dashboard binds the result to that snapshot with a
  key-order-insensitive identity, so a save in another tab cannot label different values
  as checked. Transport failures clear the prior result.
- **SQLite copy guidance** in [run-evidence.md](run-evidence.md): sequential live copies of
  the database and WAL sidecars are not a consistent backup. The documented Online Backup
  API example and offline-archive procedure are exercised by `tests/evidence_snapshot.py`,
  part of `make test`.

## Validation record

All results use synthetic fixtures; no live model, account, state, push or publication
was involved.

- Final shipment validation on 2026-09-13 ran `make check test` as one invocation: Rust
  formatting/clippy and 84 tests (four opt-in tests ignored), all three integration suites,
  distribution and crate guards, 117 dashboard browser tests with five intentional
  viewport skips, and nine showcase contract plus six showcase browser tests.
- An independent re-verification on a clean `22ea222` worktree reviewed the shipped
  first-run path against its brief, re-ran `make check`, the operator and Astra-preset
  browser suites, distribution and crate guards and the showcase tests, all passing, and
  found nothing to correct. It re-checked the README's pending-release wording read-only:
  the GitHub API reported zero releases and zero tags for `tyk-swe/octomus-agent`, and the
  crates.io API returned 404 for `octomus-agent`.
- The recorded-mode showcase build with the real-run candidate above passed after owner
  approval, and the staged output contained only `index.html`, local JavaScript/CSS
  assets, `public-run.json` and `approval.json`. The synthetic showcase build was removed
  after testing.

## Unresolved gates

No live validation on the owner's dedicated VM and bot, no public release or crate, no
owner-approved public sharing of the recorded showcase, no human usefulness approval, and
no dedicated-deployment acceptance. The evidence limitations recorded in
[day2-result.md](day2-result.md) remain.
