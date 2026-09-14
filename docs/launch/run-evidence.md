# Run evidence (`RunEvidenceV1`)

A narrow, read-only representation of what Octomus **actually saved** for one planning
cycle and the tasks that cycle produced. It answers "what review and check evidence is
on record for this run?" — nothing more.

**Every export requires review before sharing.** It is a private operator artifact, not
a public-safe or publication-approved one. Exports are never written to `web/static` and
are never marked public-safe automatically.

## What this is not

The representation is deliberately named **recorded review/check evidence**, not "safe to
publish". Producing it performs no live checks: no HEAD resolution, no workspace
inspection, no remote or GitHub query, no authorization check, and no current
pull-request state. Every field is a fact about a saved record.

The same nine caveats ship inside each export as `limitations`:

1. Recorded review and check evidence only; no live checks were performed.
2. Planning completion is not task completion — a completed cycle records decisions.
3. Deferred is not rejected.
4. A recorded PR describes delivery, not merge. Published is not merged.
5. Audit acceptance is a recommendation; audit cycles never create an execution queue,
   so an accepted audit proposal has no linked task by design.
6. Saved session routes are **requested** routes. Runtime model identity is not
   independently reported.
7. Costs, delivery time and replay timelines are not inferred.
8. Zero or multiple task matches are preserved as recorded; no task is selected for you.
9. Remaining free text is model-authored and still requires manual review.

## Interfaces

| Interface | Call |
| --- | --- |
| HTTP | `GET /api/cycles/{id}/evidence` with `Authorization: Bearer <operator token>` |
| CLI | `octomus-agent --data-dir <dir> --export-run <cycle-id>` |

Both share one assembler (`octomus_agent::evidence::assemble`) and one snapshot reader,
so their output is identical apart from `generated_at`.

The HTTP route sits inside the existing authenticated API router, so it inherits the
operator token check, the authentication backoff and the response-wide redaction pass.
Unknown cycles return `404 {"error":"Cycle not found"}`.

`--export-run` follows `--usage-report`: it opens the existing database read-only and
returns **before** directory creation, permission changes, service locking, migrations,
`App` construction and worker startup. Diagnostics go to stderr; stdout carries only the
JSON. It conflicts with `--doctor`, `--print-config` and `--usage-report`. A missing
state database or a missing cycle is an explicit error, never an empty successful export.

### Exporting from a copy of saved state

Use only a **specific offline snapshot or export explicitly authorized by the owner**.
A narrative report, an archive mentioned in a previous note, or a discovered directory
is not permission to read it. If no input is supplied, request its path and permission
once, continue synthetic tooling work, and report **REAL DATA BLOCKED**. Do not search
for archives or reconstruct records from prose.

Keep the authorized original untouched. Work on a disposable copy in a private,
owner-only directory outside Git and public build roots (including `dist`, `web/build`,
`web/static` and capture directories). Git-ignore alone is not a privacy boundary.
Use the existing exporter against that working copy:

```
octomus-agent --data-dir <copy-dir> --export-run <cycle-id> > <private-dir>/run-<cycle-id>.json
```

Use `umask 077` before creating copies or redirecting exports. An existing authorized
`RunEvidenceV1` export needs no database access or second exporter. Cycle IDs are matched
exactly, including case and whitespace.

#### Running source: owner creates a SQLite backup

**Sequentially copying `state.db`, `state.db-wal` and `state.db-shm` from a running
source is not a consistent backup**, even if all three eventually arrive. Writers and
checkpoints can change them between copies. Copying only `state.db` can silently omit
committed WAL records; a successful export or `PRAGMA integrity_check` cannot prove that
no records were lost. The exporter's read transaction cannot repair an inconsistent
input copy.

For a running source, the owner can use SQLite's supported
[Online Backup API](https://www.sqlite.org/backup.html), for example Python's
[`Connection.backup`](https://docs.python.org/3/library/sqlite3.html#sqlite3.Connection.backup).
It includes committed WAL data in a consistent destination database without sequential
sidecar copying. This backs up the database only, not workspaces or runner state.

The following is an **owner-operated example**, not authorization for an agent to read
live state. The source path must be explicitly chosen by the owner; use a new private
destination outside any checkout or served directory. Only synthetic sources are used
to demonstrate this example in `tests/evidence_snapshot.py`.

<!-- owner-sqlite-backup -->
```sh
(
set -eu
umask 077
OWNER_STATE_DB=/absolute/owner-selected/source/state.db
PRIVATE_RUN=/absolute/private/new-run-review
mkdir -m 700 "$PRIVATE_RUN"
mkdir -m 700 "$PRIVATE_RUN/snapshot"
python3 - "$OWNER_STATE_DB" "$PRIVATE_RUN/snapshot/state.db" <<'PY'
from contextlib import closing
from pathlib import Path
import sqlite3
import sys

source = Path(sys.argv[1]).resolve(strict=True)
destination = Path(sys.argv[2])
# Refuse to overwrite an existing file, including the source.
with destination.open("xb"):
    pass
with closing(sqlite3.connect(source.as_uri() + "?mode=ro", uri=True)) as reader:
    with closing(sqlite3.connect(destination)) as snapshot:
        reader.backup(snapshot)
        if snapshot.execute("PRAGMA integrity_check").fetchall() != [("ok",)]:
            raise RuntimeError("Snapshot integrity check failed; do not use it")
PY
)
```

Use the destination only after successful completion; discard an incomplete destination
after an error and retry with a new private filename. Record the capture method, UTC
time, source identity and snapshot checksum privately. The owner then supplies the
offline snapshot and explicit read permission. No live backup is performed by this task.

#### Genuinely offline consistent archive

An archive captured with all SQLite connections closed and no process changing the
files during capture is a different case. After the owner identifies and authorizes it,
copy its `state.db` and matching `state.db-wal` if present into a new writable private
directory. Preserve any captured `state.db-shm` with that same set; it is a rebuildable
WAL index, not a substitute for the WAL. Never mix sidecars from other captures or
discard a WAL because the main database opens successfully. An archive made with
sequential live copies does not become consistent merely by being offline now.

Do not open the original archive with SQLite, checkpoint it, migrate it, or run the
service against it. Even a read-only SQLite connection can create or update WAL-index
sidecars on the working copy; a non-writable copy can fail with
`attempt to write a readonly database`. The exporter issues no application writes, but
that is not a promise of byte-for-byte filesystem immutability for SQLite sidecars.
Record original and working-copy provenance privately and export only from the copy.

## Consistency and joins

The cycle and its tasks are read inside **one** database transaction, so a cycle can
never be paired with tasks from a later write. The dashboard's recent-task window is not
used; task selection is a direct `kind='task'` scan filtered on the saved `cycle_id`.

Tasks are joined to proposals strictly on `(cycle_id, proposal_id)` — never by title,
never by proposal ID alone, and never by "whichever task is newest". Proposal IDs repeat
across cycles by design (`rediscover-…` candidates especially), so the cycle half of the
key is load-bearing. Zero matches and multiple matches are both preserved and reported as
gaps; no single task is chosen.

## Shape

```
RunEvidenceV1
  schema_version, generated_at, kind, review_required_before_sharing,
  review_requirement, limitations[9], gaps[]
  cycle
    id, number, mode, status, started_at, completed_at, repository,
    grounding_revision
    planning: status, planning_finished, proposal_count, decisions{},
              creates_execution_queue, error_recorded, reviewer_batches_saved
  proposals[]
    id, title, target, tier, category, problem, benefit, scope, evidence[],
    final_decision, final_reason, gaps[]
    reviewer_verdicts[]: reviewer, state, decision, reason, note
    linked_tasks[]
      id, cycle_id, proposal_id, status, branch, attempts, blocked_reason,
      error_recorded, created_at, updated_at, gaps[]
      revisions: source, comparison_base, default_branch, output
      sessions[]: id, role, status, requested_route, started_at
      latest_review: rounds_recorded, clean, clean_at_output_revision, latest
      required_commands: state, commands[], all_passed_at_output_revision
      pull_request: number, url, source
```

`Option` fields serialize as `null` rather than being omitted, so the key set is stable.
The TypeScript mirror is `RunEvidenceV1` in `web/src/lib/types.ts`.

### Reviewer verdicts

Proposal assessments are stored as untyped JSON batches on the cycle, pushed in role
order by `review_proposals`/`attach`, with reviewer identity recorded separately as
`adversary-a` / `adversary-b` sessions. Reviewer slots here are therefore **positional
and fixed**: a malformed batch keeps its slot instead of shifting the next batch into the
missing reviewer's identity.

`state` is one of:

| `state` | Meaning |
| --- | --- |
| `recorded` | Exactly one readable entry for this proposal in this reviewer's batch. |
| `missing` | No batch for the slot, or the batch has no entry for this proposal. |
| `duplicate` | Several entries. `decision` is filled only when they agree; a note says whether they agree or are inconsistent. |
| `malformed` | The batch exists but carries no readable assessment list. |

Verdicts are never inferred from the cycle's final decision. Extra batches beyond the two
slots are reported as unattributable and are never reassigned. A batch saved without a
matching completed reviewer session is marked `unconfirmed` in its note.

### Review evidence

`latest` is the **latest saved** review round, not the last convenient passing one.
`clean` requires `completed=true`, a non-blank summary and zero findings.
`clean_at_output_revision` additionally requires that the round's revision equals the
task's recorded output revision. The review summary text is not exported; only
`summary_present` and the structured findings are.

### Required commands

Commands come from the **task's saved execution configuration**, not from live config.
For each command, the latest recorded result is authoritative:

| `state` | Meaning |
| --- | --- |
| `passed` | Latest result succeeded at the recorded output revision. |
| `passed_at_other_revision` | Latest result succeeded, but at a different revision. |
| `failed` | Latest recorded result failed — a newer failure invalidates an older pass. |
| `no_result` | The command is configured with nothing recorded. Not passing. |

With no configured commands the state is `not_configured` — never "passing".
`all_passed_at_output_revision` is true only when at least one command is configured and
every one of them is `passed`. Raw command output is not exported.

## Allowlisted fields

Output is an explicit allowlist. Omitted by default: configuration dumps, workspace,
storage and executable paths, proposal and role prompts, raw session transcripts and
summaries, raw command output, task error text and arbitrary logs. Where a record would
otherwise be summarized by private text, only the fact survives — `error_recorded`,
`summary_present`, `results_recorded`.

The store's existing redaction runs over the assembled value as defense in depth, after
all facts are computed, so redacting a display string can never change a reported fact.

## Scope

No saved-state migration, no execution-policy change, no new dependency. The Day 1
executor-startup and process-lifecycle behavior is untouched.

## Tests

`tests/evidence.rs` covers complete and partial cycles, repeated proposal IDs across
cycles, malformed and missing reviewer batches, duplicate and unconfirmed verdicts, audit
cycles without tasks, incomplete and empty-summary reviews, later failed checks and
mismatched revisions, unconfigured checks, unknown cycles, authentication enforcement,
preserved cycle-action routes, omission of private fields, read-only export behavior, and
agreement between the API and CLI. Every fixture is an explicitly synthetic temporary
database.

`tests/evidence_snapshot.py` exercises the exact documented backup example on a synthetic
open WAL database, including committed task records and an uncommitted change. It exports
through `--export-run`, checks adverse/missing evidence, and validates/hashes a private
synthetic wrapper with P02's existing public contract without staging a build. Run after
`cargo build --locked` with `python3 tests/evidence_snapshot.py`; also part of `make test`.
