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

## Implemented surface

Rust remains the default executable and complete service. The Go executable
now performs the two read-only commands itself; service startup and `--doctor`
still fail explicitly until M7.

```sh
make build-go                 # bin/octomus-agent-go, with the real dashboard
make check-go test-go         # formatting, vet, unit/race, CLI/embed and evidence snapshot
make test-go-storage          # cross-language checks against the frozen Rust reference
bin/octomus-agent-go --data-dir .octomus --usage-report
bin/octomus-agent-go --data-dir .octomus --export-run <CYCLE_ID>
```

`internal/store` is the port of `src/store.rs` and its `migrate`, `queries`,
`capacity` and `notifications` modules: the same DDL, the same `user_version`
1–6 sequence with its additive projections, triggers and indexes, and the same
SQL for every indexed view, admission, plan commit, lineage update, cancellation
marker, reservation, inventory and outbox operation. Records stay canonical
`jsoncompat` JSON; the Go migration path never rewrites a stored record.
`internal/report` and `internal/evidence` port `src/report.rs` and
`src/evidence.rs`, including the nine verbatim limitations, the review
requirement, positional reviewer attribution and every gap sentence.

### SQLite driver decision

`modernc.org/sqlite` v1.59.0 (`modernc.org/libc` v1.75.7) is the single driver.
No replacement was needed; the compatibility gate is
`internal/store/driver_test.go` and `compat_test.go`:

- One physical connection: `sql.DB` limited to one open connection and pinned to
  a `*sql.Conn` for the store's lifetime, so every setting runs on every
  connection the service uses. Transactions are explicit `BEGIN`/`BEGIN
  IMMEDIATE`/`COMMIT`/`ROLLBACK` statements on that connection; nothing calls back
  through `sql.DB` while a transaction is open.
- Settings verified in-process: `journal_mode=wal`, `synchronous=2` (FULL),
  `busy_timeout=5000`, `query_only=0` for the service; `busy_timeout=5000` and
  `mode=ro` for reporting. Multi-statement `Exec` batches run migrations exactly
  as Rust's `execute_batch` did.
- Behavior verified: a foreign `BEGIN IMMEDIATE` blocks a store write until it
  commits (busy timeout honoured); a read-only deferred transaction sees one
  snapshot including committed WAL frames and ignores a concurrent commit until
  the next snapshot; no rollback journal is ever created; a read-only open of a
  missing path, an insert through the read-only connection, and a `PRAGMA
  user_version` write through it all fail without creating state.
- Read-only openers `os.Stat` the path first, so a missing database is reported
  as `Cannot open existing state database for read-only ...` and no directory,
  lock, WAL or migration is created.

### Acceptance coverage

| Criterion | Executable evidence |
| --- | --- |
| 1. Fresh, current and legacy fixtures open with the expected schema and query results | `TestFrozenStateContracts`, `TestFreshDatabaseMatchesReferenceSchema`, `TestLegacyReportingIsReadOnlyAndUpgradePreservesUnattributedUsage`, the four `...AfterUpgrade`/`...LegacyFallbacks` cases with in-test legacy projections and `user_version` 2–5, `TestIndexedViewsAnswerFromOneSmallState` (scheduling, status, cycle, baseline, proposal, PR-history, batch and event views), `tests/go_storage.py` direction 1 |
| 2. Reopening rewrites no canonical record and invents no admission | `TestFrozenStateContracts` (byte-equal records, zero admissions, usage still 3 across two reopens), `tests/go_storage.py` (Go reopen of the Rust-upgraded fixture; Rust rollback afterwards) |
| 3. Transaction boundaries for counters/admissions, plan commits, lineage, cancellation markers, reservations and outbox | `TestDurableAndBudgetAtomic`, `TestAdmissionAndCounterCommitTogetherAcrossDaysAndRestarts`, `TestCommitPlanIsAtomicOnLineageFailure`, `TestRepositoryHistoryAndRediscoveryLineageIgnoreRepositoryCasing`, `TestCancellationGuardsPublicationCheckpoints`, `TestPrAdmissionAndReservationsShareOneTransaction`, `TestOutboxEnqueueSharesTheWriterTransaction` |
| 4. Injected failures roll back every related change; an admission never writes only the counter or only the ledger | `TestFailedAdmissionsRollBackCounterAndLedgerTogether` (storage limit, exhausted budget, duplicate ledger id, zero budget, unparseable timestamp), `TestCommitPlanIsAtomicOnLineageFailure`, outbox rollback in `TestOutboxEnqueueSharesTheWriterTransaction` |
| 5. Read-only exports without creating state, locking, migrating, starting workers or a token | `TestReportNeverCreatesMissingState`, `TestReadOnlyAccessNeverCreatesOrWritesState`, `evidence.TestCLIExportIsReadOnlyAndErrorsExplicitly`, `cmd/octomus-agent TestReadOnlyExportsReturnBeforeTouchingApplicationState`, `tests/go_foundations.py`, `tests/evidence_snapshot.py` |
| 6. Exports observe one consistent snapshot including committed WAL data | `TestReadOnlySnapshotIsConsistentAndIncludesWAL`, `tests/evidence_snapshot.py` (task committed only in WAL is exported; main-file-only copy is not), `evidence.TestStoreAndCLIFactsAgreeApartFromGenerationMetadata` |
| 7. Unsupported future schema refused explicitly without destructive repair | `TestFutureSchemaIsRefusedWithoutRepair` (`user_version` 7: open, usage report and run export refuse; schema, bytes and version unchanged) |
| 8. Go reads Rust-created records; the frozen Rust reference reads Go-written records in an isolated rollback fixture | `tests/go_storage.py` with `tests/go/rollbackfixture`: Rust-upgraded legacy fixture → Go exports byte-identical to the Rust reference (apart from `generated_at`) → Go reopen → Rust exports and paused API views unchanged; Go-written plan, admissions, event, PR observation and reservation → isolated copy → Rust exports byte-identical, Rust API serves the task/cycle/evidence, records and ledger untouched → Go re-reads the copy |
| 9. Driver compatibility gate with verified settings and transaction behavior | `TestConnectionSettingsMatchTheStorageContract`, `TestBusyTimeoutWaitsForForeignWriters`, `TestReadOnlySnapshotIsConsistentAndIncludesWAL`, race-enabled `go test` |

Ported suites: `tests/usage.rs` (all three cases), `src/store.rs` inline tests
(`durable_and_budget_atomic`, `planning_capacity_reflects_policy_usage_and_utc_day`,
`redacts_tokens`), the M0-allocated `tests/review_regressions.rs` and
`tests/hardening.rs` storage cases, `src/evidence.rs` inline tests and every
`tests/evidence.rs` case except the API route. `tests/evidence_snapshot.py` runs
unchanged against the Go executable. `tests/compatibility_capture.py` keeps
capturing from Rust only; its schema and export sections are asserted against Go
by `TestFrozenStateContracts`, and its `api` section waits for M7.

Cases whose store half is ported here and whose remainder belongs elsewhere:
the two cancellation regressions (`cancel_task` guard and cancel markers here;
`App::recover`, worker checkpoints and the cancel/supersede routes in M5/M7),
`unresolved_problem_identity_survives_rewording` (duplicate lookup here;
`validate_proposals` in M5), `task_guard_keeps_reservation_until_fallback_write_finishes`
(engine task guard, M5) and `evidence_route_requires_auth_and_reports_unknown_cycles`
(M7). `tests/history_scale.rs` is an ignored allocation benchmark and was not ported.

Fixture provenance: `tests/fixtures/compatibility/state.json` (captured from
Rust 3c2b5cd, legacy schema `records`+`usage`, current schema 43 objects at
`user_version` 6), the synthetic `tests/evidence_snapshot.py` database, and the
synthetic records written by `tests/go/rollbackfixture`. No live state was used.

## Progress record

```text
Milestone: M2
Status: DONE
Implementation revision: Working tree based on dda039e; frozen behavior reference remains 3c2b5cd50924033873d7f740f9df44daee5685db (target/debug/octomus-agent built from the unchanged Rust sources).
Delivered output: internal/store (store, migrate, queries, capacity, notifications), internal/report, internal/evidence, jsoncompat.Float64/FormatFloat, Go --usage-report and --export-run, tests/go/rollbackfixture, tests/go_storage.py, make test-go-storage and the go-storage-compatibility CI job; modernc.org/sqlite v1.59.0 as the single driver.
Acceptance tests and commands: gofmt -l cmd internal tests/go web/embed*.go; go vet ./...; go test ./...; CGO_ENABLED=1 go test -race ./...; CGO_ENABLED=0 go build -trimpath -o bin/octomus-agent-go ./cmd/octomus-agent; OCTOMUS_TEST_BINARY=bin/octomus-agent-go python3 tests/go_foundations.py --go-m1; OCTOMUS_TEST_BINARY=bin/octomus-agent-go python3 tests/evidence_snapshot.py; OCTOMUS_TEST_BINARY=bin/octomus-agent-go python3 tests/go_storage.py; Rust-default python3 tests/compatibility_capture.py; cargo fmt --check; cargo clippy --all-targets --locked -- -D warnings; make check; make test.
Results: PASS. 30 store cases, 16 evidence cases and 3 CLI cases pass with and without -race (store package 98 s under -race). go_foundations: 84 frozen CLI cases plus explicit read-only failures for missing state. evidence_snapshot against Go: WAL backup, private CLI export, adverse evidence and hash check. go_storage: Go read the Rust-upgraded fixture (5 records, 43 schema rows) with byte-identical exports, and the frozen Rust reference read Go-written state (7 records, 2 admissions) from an isolated rollback copy with byte-identical exports and matching paused API views. compatibility_capture against Rust: frozen schema/export/API contracts unchanged. make check and make test (Rust, dashboard, integration, browser, showcase and public-site suites) exit 0 on this tree, so the Rust reference and its gates are unchanged.
Intentional behavior differences: (1) Go refuses a state database whose user_version exceeds 6 with an explicit error before applying any setting or migration; Rust has no future-schema check (criterion 7). (2) Go applies journal_mode=WAL and synchronous=FULL after that check instead of at connect time, so a refused database is not modified; supported databases end in the same state. (3) tests/go_foundations.py now expects --usage-report and --export-run to run in Go and to fail read-only on missing state instead of reporting them unimplemented. No export, record or schema differences: usage reports and run evidence are byte-identical to the Rust reference apart from generated_at.
Unrun required checks and blockers: None for M2. Live operation, the API evidence route, cancellation/supersede routes, engine task guards and proposal validation belong to M5/M7 and are not claimed here.
Next eligible milestone: M3 (independent of M2); M4 after M2 and M3 are DONE.
```
