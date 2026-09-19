# M2 — Port SQLite and prove read-only compatibility

[Roadmap](README.md) · **Depends on:** [M1](m1-go-foundations.md) ·
**Enables:** [M4](m4-runner-adapters.md), once M3 is also complete

## Deliverable

The Go store, supported migrations, indexed projections, admission transactions,
reservations, notification storage, usage reporting, and CLI run-evidence export.

Keep the existing database model for the first Go release. Preserve canonical
JSON records and the `user_version` migration sequence, including additive tables
and triggers outside that sequence. Keep storage redesign outside this milestone.

Settle `modernc.org/sqlite` compatibility early in M2, before depending on it for
orchestration. Exercise the single-connection model, WAL, `synchronous=FULL`, busy
timeout, transaction ownership, and read-only access. If concrete evidence requires
a different driver, record one replacement decision here; retain one driver.

## Acceptance criteria

1. Go opens fresh, current, and supported historical fixture databases and produces
   the expected schema and query results.
2. Reopening an upgraded database neither rewrites historical canonical records
   nor invents historical admission entries.
3. Counter/admission updates, accepted-plan commits, lineage updates, cancellation
   markers, reservations, and outbox enqueueing retain their transaction boundaries.
4. Injected failures roll back every related change. An admission cannot increment
   only the counter or write only the ledger.
5. Read-only exports work without creating missing state, taking the service lock,
   running migrations, starting workers, or requiring an operator token.
6. Exports observe one consistent transaction snapshot, including committed data
   still in WAL.
7. An unsupported future schema is refused explicitly without destructive repair.
8. Go reads Rust-created records. The frozen Rust reference reads Go-written
   compatible records in an isolated rollback fixture.
9. The selected driver passes the compatibility gate with connection settings and
   transaction behavior verified; any replacement has recorded evidence.

## Verification and evidence

Port relevant cases from `tests/usage.rs`, `tests/review_regressions.rs`, store
inline tests, and `tests/evidence.rs`. Run `tests/evidence_snapshot.py` against Go.
Follow the M0 inventory for any additional storage cases in mixed-scope suites.

Use copies or synthetic fixtures for cross-language tests. Never run Rust and Go
concurrently against the same state directory. Record fixture provenance, schema
versions, driver version, commands, and results. M9 extends these checks into a
full upgrade/rollback rehearsal with packages and service locking.

## Progress record

```text
Milestone: M2
Status: TODO
Implementation revision: Not started.
Delivered output: None.
Acceptance tests and commands: Not run.
Results: No acceptance evidence recorded; SQLite compatibility gate pending.
Intentional behavior differences: None approved.
Unrun required checks and blockers: All acceptance checks unrun; depends on M1.
Next eligible milestone: M4 after M2 and M3 are DONE.
```
