# M2 — State upgrades

[Roadmap](README.md) · **Depends on:** a version-7 database from
[M1](m1-release-v0.1.0.md)'s tag or its candidate · **Enables:**
[M4](m4-v0.2.0.md) · **Target:** Fri 2026-10-30

## Why

`internal/store/schema.go` opens two kinds of database: an empty one, which it
creates at version 7, and one already at exactly version 7. Anything else is
refused with "this release requires a fresh version-7 data directory". That was
right while no operator held state. Once v0.1.0 ships, any schema change would
strand everyone who installed it.

Schema changes are already waiting:
- New notification events need new triggers, because the attention outbox is
  filled by triggers in `internal/store/schema.sql` (M4).
- `admissions.day` can only be dropped with a version bump.

## Deliverable

Forward-only migrations from version 7, applied at startup after a consistent
backup, with a checked-in v0.1.0 database that every later release must open.
Documentation explains how to upgrade and how to roll back.

## Design constraints

- **Ordered and forward-only.** Migrations run in sequence from version 7. Each
  one runs in a single `BEGIN IMMEDIATE` transaction that also sets
  `user_version`, so a failure leaves the previous version intact.
- **Check first.** The version check still runs before any schema or journal
  change. A version newer than the binary knows is refused and left untouched, as
  an unknown version is today.
- **Back up before the first migration.** Write a consistent copy beside the
  database before migrating, using the SQLite backup API and an integrity check,
  as the snapshot procedure in [run evidence](../run-evidence.md) does. Keep it
  until the operator removes it.
- **One fresh DDL.** `schema.sql` stays the complete DDL for the latest version;
  a fresh database never replays migrations.
- **Records stay decodable.** `wirejson.DecodeRecord` ignores unknown keys but
  rejects a missing field unless it is a pointer or tagged `wire:"default"`
  (`internal/wirejson/json.go`). A field added to a saved record uses one of
  those, or a migration fills it in.
- **Rollback means restoring the backup.** An older binary refuses a newer schema.
  There is no down-migration.

## Work

1. Replace the exact-version check in `internal/store/schema.go` with a migration
   list and runner. Keep `createSchema` for empty databases.
2. Make the backup step and name its file so an operator can find it.
3. Generate a golden version-7 database from the v0.1.0 binary (or its release
   candidate) by running a fixture e2e scenario. Use synthetic data with no
   secrets, and check it into `internal/store/testdata/`.
4. Add a test-only v7→v8 migration. It exercises ordering, the backup and
   failure handling without shipping a real schema change.
5. Document upgrade and rollback in [releasing](../releasing.md) and in the
   "Backup and upgrade" section of [deployment](../deployment.md).

## Acceptance criteria

1. The golden v0.1.0 database opens on `main`, and its dashboard state, task
   detail and `--export-run` evidence all read back.
2. The test migration:
   - creates the backup;
   - with a failure injected mid-migration, leaves the database at version 7,
     and that database still opens.
3. A fresh database's schema matches one migrated from version 7, compared
   through `sqlite_master`.
4. `TestUnsupportedStateRefused` (or its successor) still refuses unknown and
   newer versions before any schema or journal change.
5. The upgrade and rollback procedure is documented.
6. `make test` passes.

## Verification

- `go test ./internal/store/...` and `make test-go-race` for the migration and
  golden-database tests.
- `make test` before delivery.
- The golden database's provenance (the binary's commit and the scenario used) is
  recorded in the progress record.

## Progress record

```text
Milestone: M2
Status: TODO
Revision: Not started.
Delivered output: None.
Commands and results: Not run.
Unrun checks and blockers: All checks unrun; the golden database needs M1's tag or candidate.
Next: M4 after M2 is DONE.
```
