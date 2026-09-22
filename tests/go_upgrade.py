#!/usr/bin/env python3
"""Service-level upgrade/rollback rehearsal and state-directory locking (M9).

Every database here is a synthetic temporary fixture. The two executables are
selected through OCTOMUS_TEST_BINARY (the Go service) and
OCTOMUS_RUST_REFERENCE (the frozen Rust reference), matching go_storage.py.

upgrade_rehearsal — acceptance criterion 7 and the "Upgrade and rollback
rehearsal" section. Representative Rust-originated state is built two ways:
the frozen legacy fixture (tests/fixtures/compatibility/state.json) is seeded
and then migrated and served by the real Rust service, and the Go store writer
(tests/go/rollbackfixture, mode 'rehearsal') extends the migrated database with
a second committed cycle, a mid-`publishing` checkpoint task and a
`publication_uncertain` blocked task — all carrying native session identities
and pr_reservations rows. A final commit is then left resident in the WAL
(un-checkpointed) so the rehearsal proves both implementations read
WAL-resident rows — copying only state.db would silently drop them. The Go
service boots that state, performs real operator writes (pause, config save,
task cancel, cycle archive) and restarts to prove restart durability. Finally
the frozen Rust service opens an isolated copy of the Go-written state:
identical views, identical exports, schema still at version 6 — the rollback
contract.

state_directory_lock — acceptance criterion 8. Each implementation in turn
holds <data-dir>/service.lock via flock(LOCK_EX|LOCK_NB); a same-language
second instance and the other-language executable must both fail with the lock
error and exit nonzero without writing state, while a read-only export still
succeeds and a clean stop frees the directory for either implementation.

Rust-resume contract pinned by this rehearsal (audit of internal/model JSON
tags against the frozen src/model.rs — every serialized struct is currently
field-identical, and the m1 wire fixtures prove byte-identical encoding):

  Safe for the Rust reference to read and resume:
  - `records` of kinds task, cycle, pr, decision, baseline, settings, cancel —
    every field Go emits today exists in the frozen structs; Option fields are
    written as explicit nulls, `wire:"default"` fields match #[serde(default)],
    and omitzero/omitempty fields match skip_serializing_if.
  - `settings` ids: config, control, pr_inventory, baseline_latest, storage.
  - Tables events, usage, admissions, pr_reservations, notification_policy,
    notification_outbox, record_meta, record_counts, proposal_records,
    batch_members — identical DDL and trigger-maintained projections.
  - The control record: paused, mode (paused|run_once|continuous), batch
    (id, phase draining|planning|executing, cycle_id), idle_streak,
    context_fingerprint, cycle_number, next_cycle_at, error.
  - Publication checkpoint fields: execution_session/repair_session and
    sessions[] thread identities, output_commit, pr_number/pr_url,
    blocked_reason publication_uncertain, attempt_policy, review_baseline,
    run_id, lifecycle.{archived_at,discarded_at}, superseded_by/supersedes,
    rediscovery_*; and a leftover service.lock file from a stopped service.
  - Committed rows still resident only in state.db-wal.

  Unsafe / refused — nothing the current Go executable writes produces these,
  but they are the boundary M10's rollback rules must respect:
  - PRAGMA user_version > 6: both implementations refuse to open the database.
  - A record kind added later: Rust stores it untouched but its views ignore it.
  - A new field on a strict struct (Config, Route, Proposal, Finding, Review —
    #[serde(deny_unknown_fields)]): Rust fails to deserialize any record that
    carries it, including the config/route snapshots embedded in tasks,
    baselines and admissions.
  - A new field on a lenient struct (Task, Cycle, Session, ...): Rust reads it
    but silently drops it the next time it rewrites that record.
  - A new enum value (Status, BlockedReason, OperatingMode, CycleMode,
    BatchPhase, BaselineStatus, PlanningCapacityStatus, Backend): Rust fails
    deserialization of the containing record.
  - settings/storage, decision records and event messages are opaque JSON:
    new keys pass through untouched and stay safe.
"""
import json
import os
import shutil
import socket
import sqlite3
import subprocess
import sys
import tempfile
from pathlib import Path

PROJECT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PROJECT / 'tests'))
import compatibility_capture  # noqa: E402
import e2e  # noqa: E402
from go_storage import (canonical, copy_state, exports, go_fixture,  # noqa: E402
                        records, run, schema)

GO = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(PROJECT / 'bin/octomus-agent-go')))
RUST = Path(os.environ.get('OCTOMUS_RUST_REFERENCE', str(PROJECT / 'target/debug/octomus-agent')))
FIXTURE = PROJECT / 'tests/fixtures/compatibility/state.json'

LEGACY_CYCLE = compatibility_capture.CYCLE_ID
LEGACY_TASK = compatibility_capture.TASK_ID
# Authored by rollbackfixture 'rehearsal' mode (tests/go/rollbackfixture/main.go).
REHEARSAL_CYCLE = '66666666-6666-4666-8666-666666666666'
PUBLISHING_TASK = '77777777-7777-4777-8777-777777777777'
UNCERTAIN_TASK = '88888888-8888-4888-8888-888888888888'
TASKS = sorted([LEGACY_TASK, PUBLISHING_TASK, UNCERTAIN_TASK])
CYCLES = sorted([LEGACY_CYCLE, REHEARSAL_CYCLE])
WAL_MARKER = 'wal-resident committed row (rehearsal)'
WAL_DECISION = 'wal-resident-decision'
LOCK_ERROR = 'Another Octomus service is using this data directory'


def service(binary, root, checks):
    """Run one executable against root/.octomus and collect API views.

    Both implementations boot paused over a database that lacks a runnable
    configuration, so no runner or remote peer is ever contacted."""
    e2e.BINARY = binary
    svc = e2e.Service(root)
    svc.start()
    try:
        svc.wait(lambda: svc.request('/state')['storage'], 'storage measurement')
        return checks(svc)
    finally:
        svc.stop()
        svc.log.close()


def capture(svc):
    """The comparable API surface: durable state only, no host-specific fields.

    `storage` measures the containing directory and legitimately differs between
    the upgrade copy and the rollback copy, so it is excluded; everything else
    derives from committed records and must be identical across executables."""
    state = {key: value for key, value in svc.request('/state').items() if key != 'storage'}
    views = {'state': state, 'events': svc.request('/events'),
             'proposals': svc.request('/proposals'), 'prs': svc.request('/prs')}
    for task in TASKS:
        views[f'task:{task}'] = svc.request(f'/tasks/{task}')
    for cycle in CYCLES:
        views[f'cycle:{cycle}'] = svc.request(f'/cycles/{cycle}')
        evidence = svc.request(f'/cycles/{cycle}/evidence')
        assert isinstance(evidence.pop('generated_at'), str)
        views[f'evidence:{cycle}'] = evidence
    return views


def sidecars(path):
    """Every non-records table content: the durable surface records() misses."""
    with sqlite3.connect(path) as db:
        return {
            'events': db.execute('SELECT id, at, entity_id, kind, message FROM events ORDER BY id').fetchall(),
            'usage': db.execute('SELECT * FROM usage ORDER BY day').fetchall(),
            'admissions': db.execute('SELECT id, at, day, data FROM admissions ORDER BY at, id').fetchall(),
            'pr_reservations': db.execute('SELECT * FROM pr_reservations ORDER BY task_id').fetchall(),
            'record_meta': db.execute('SELECT * FROM record_meta ORDER BY kind, id').fetchall(),
            'record_counts': db.execute('SELECT * FROM record_counts ORDER BY 1, 2, 3').fetchall(),
            'proposal_records': db.execute('SELECT cycle_id, proposal_id, mode, number, decision, target, title, data, content_revision FROM proposal_records ORDER BY seq').fetchall(),
            'batch_members': db.execute('SELECT * FROM batch_members ORDER BY 1, 2').fetchall(),
            'notification_policy': db.execute('SELECT * FROM notification_policy').fetchall(),
            'notification_outbox': db.execute('SELECT * FROM notification_outbox').fetchall(),
            'user_version': db.execute('PRAGMA user_version').fetchone()[0],
        }


def upgrade_rehearsal(root):
    fixture = json.loads(FIXTURE.read_text())
    origin = root / 'rust-origin'
    data_dir = origin / '.octomus'
    data_dir.mkdir(parents=True)
    with sqlite3.connect(data_dir / 'state.db') as db:
        for _, _, _, sql in fixture['legacy_schema']:
            db.execute(sql)
        db.executemany('INSERT INTO records VALUES(?,?,?)',
                       [(kind, identity, json.dumps(value)) for kind, identity, value in fixture['records']])
        db.execute("INSERT INTO usage VALUES('2026-01-01',3)")

    # The frozen Rust reference upgrades the legacy database and writes its own
    # startup state — the state a real pre-upgrade deployment would leave.
    rust_views = service(RUST, origin, lambda s: {
        'tasks': sorted(t['id'] for t in s.request('/state')['tasks'])})
    assert rust_views['tasks'] == [LEGACY_TASK]
    migrated = records(data_dir / 'state.db')
    assert migrated['user_version'] == 6, migrated['user_version']

    # The Go store writer adds the publication-checkpoint records on the
    # Rust-migrated database (committed, checkpointed on close).
    summary = go_fixture('rehearsal', data_dir / 'state.db')
    assert summary['publishing_task_id'] == PUBLISHING_TASK and summary['uncertain_task_id'] == UNCERTAIN_TASK
    assert sorted(summary['tasks']) == TASKS and sorted(summary['cycles']) == CYCLES, summary
    assert summary['reservations'] == 2, summary

    # Leave one committed batch resident only in the WAL: autocheckpoint off, commit,
    # copy while the writer connection is still open (a close would checkpoint).
    conn = sqlite3.connect(data_dir / 'state.db')
    conn.execute('PRAGMA wal_autocheckpoint=0')
    conn.execute('PRAGMA synchronous=FULL')
    conn.execute("INSERT INTO events(at,entity_id,kind,message) VALUES ('2026-09-21T10:05:00Z','system','rehearsal',?)", (WAL_MARKER,))
    conn.execute("INSERT INTO records VALUES('decision',?,?)", (WAL_DECISION, json.dumps(
        {'id': WAL_DECISION, 'repository': 'Fixture/Project', 'title': 'WAL-resident decision', 'decision': 'deferred'})))
    conn.commit()
    assert (data_dir / 'state.db-wal').exists() and (data_dir / 'state.db-wal').stat().st_size > 0
    # A state.db-only copy omits the committed rows — the documented backup
    # procedure must carry the WAL sidecars (copy_state does).
    main_only = root / 'main-only'
    main_only.mkdir()
    shutil.copy2(data_dir / 'state.db', main_only / 'state.db')
    with sqlite3.connect(main_only / 'state.db') as probe:
        missing_events = probe.execute('SELECT count(*) FROM events WHERE message=?', (WAL_MARKER,)).fetchone()[0]
        missing_records = probe.execute('SELECT count(*) FROM records WHERE id=?', (WAL_DECISION,)).fetchone()[0]
    assert missing_events == 0 and missing_records == 0, 'the rehearsal rows must live only in the WAL'
    upgrade_dir = copy_state(data_dir, root / 'upgrade/.octomus')
    copy_state(data_dir, root / 'wal-rust/.octomus')
    conn.close()

    # The Rust reference reads the WAL-resident rows from its own copy, and its
    # startup recovery makes the same checkpoint decision Go will make below.
    def wal_checks(svc):
        assert any(e['message'] == WAL_MARKER for e in svc.request('/events')), 'Rust missed the WAL-resident commit'
        recovered = svc.request(f'/tasks/{PUBLISHING_TASK}')
        assert recovered['status'] == 'blocked' and recovered['blocked_reason'] == 'workspace_invalid', recovered
        return recovered
    service(RUST, root / 'wal-rust', wal_checks)

    # The Go service boots the upgraded state: recovery turns the in-flight
    # `publishing` task into a blocked checkpoint and reads the WAL rows.
    def go_checks(svc):
        events = svc.request('/events')
        assert any(e['message'] == WAL_MARKER for e in events), 'Go missed the WAL-resident commit'
        state = svc.request('/state')
        assert sorted(t['id'] for t in state['tasks']) == TASKS
        assert sorted(c['id'] for c in state['cycles']) == CYCLES
        publishing = svc.request(f'/tasks/{PUBLISHING_TASK}')
        assert publishing['status'] == 'blocked' and publishing['blocked_reason'] == 'workspace_invalid', publishing
        assert publishing['execution_session'] == 'codex-thread-exec-7' and publishing['repair_session'] == 'codex-thread-repair-7'
        assert publishing['output_commit'] == 'bbbb0002' and publishing['pr_number'] is None
        repair = next(s for s in publishing['sessions'] if s['id'] == 'codex-thread-repair-7')
        assert repair['status'] == 'interrupted', repair
        uncertain = svc.request(f'/tasks/{UNCERTAIN_TASK}')
        assert uncertain['status'] == 'blocked' and uncertain['blocked_reason'] == 'publication_uncertain'
        assert uncertain['execution_session'] == 'codex-thread-exec-9' and uncertain['pr_number'] == 7
        assert uncertain['output_commit'] == 'cccc0003' and 'reconcile' in uncertain['allowed_actions']
        # Real Go writes through the operator API, all durable and runner-free:
        # pause (control record), config save (settings record), task cancel
        # (cancel record + release trigger) and cycle archive (lifecycle write).
        svc.request('/control/pause', 'POST')
        config = svc.request('/config')
        config['cycle_interval_seconds'] = 3600
        config['verification_commands'] = ['true']
        saved = svc.save_config(config)
        assert saved['cycle_interval_seconds'] == 3600 and saved['verification_commands'] == ['true']
        svc.request(f'/tasks/{LEGACY_TASK}/cancel', 'POST')
        svc.request(f'/cycles/{LEGACY_CYCLE}/archive', 'POST')
        cancelled = svc.request(f'/tasks/{LEGACY_TASK}')
        assert cancelled['status'] == 'cancelled', cancelled['status']
        archived = svc.request(f'/cycles/{LEGACY_CYCLE}')
        assert archived['lifecycle']['archived_at'] and archived['lifecycle']['discarded_at']
        return capture(svc)

    go_views = service(GO, root / 'upgrade', go_checks)

    # Restart durability: a second Go boot on the same directory observes the
    # identical durable views — every write above survived.
    restarted = service(GO, root / 'upgrade', capture)
    assert restarted == go_views, 'the Go restart changed the durable views'

    # Rollback: the frozen Rust reference opens an isolated copy of the
    # Go-written state. A stopped service leaves service.lock behind; the copy
    # carries it to prove a stale lock file never blocks the next holder.
    rollback_dir = copy_state(upgrade_dir, root / 'rollback/.octomus')
    shutil.copy2(upgrade_dir / 'service.lock', rollback_dir / 'service.lock')
    written = sidecars(upgrade_dir / 'state.db')
    go_exports = {cycle: exports(GO, upgrade_dir, cycle) for cycle in CYCLES}
    for cycle in CYCLES:
        rust_export = exports(RUST, rollback_dir, cycle)
        assert rust_export == go_exports[cycle], f'Rust export drifted from the Go-written state for {cycle}'

    rust_views = service(RUST, root / 'rollback', capture)
    assert rust_views == go_views, 'Rust API views differ on the Go-written state'
    state = rust_views['state']
    assert state['control']['paused'] and state['status'] == 'paused', state['control']
    assert state['active_tasks'] == 0 and not state['cycle_active']

    # No schema downgrade and no record drift: the Rust service may rewrite only
    # its own settings rows (the storage measurement), nothing canonical.
    reverted = sidecars(rollback_dir / 'state.db')
    assert reverted['user_version'] == 6 and written['user_version'] == 6
    assert reverted == written, 'the Rust rollback changed Go-written durable state'
    assert schema(rollback_dir / 'state.db') == schema(upgrade_dir / 'state.db')
    assert canonical(records(rollback_dir / 'state.db')) == canonical(records(upgrade_dir / 'state.db'))

    # The Go executable still reads the state the Rust reference just served.
    for cycle in CYCLES:
        assert exports(GO, rollback_dir, cycle) == go_exports[cycle], 'Go exports changed after the Rust rollback'
    print(f'PASS upgrade rehearsal: Rust-migrated state + WAL-resident rows, Go service writes survived a restart, '
          f'and the Rust reference resumed the copy with identical views ({len(TASKS)} tasks, {len(CYCLES)} cycles)')


def contend(contender, data_dir, env):
    """One service start against a held data directory must fail without writing."""
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    result = subprocess.run([str(contender), '--data-dir', str(data_dir), '--listen', f'127.0.0.1:{port}',
                             '--assets', str(PROJECT / 'web/build')],
                            env=env, capture_output=True, text=True, timeout=60)
    assert result.returncode != 0, f'{contender.name} started on a locked data directory'
    assert LOCK_ERROR in result.stderr, f'{contender.name} stderr: {result.stderr}'


def lock_snapshot(data_dir):
    """Everything a failed contender could change: the file list and content."""
    return {'files': sorted(os.listdir(data_dir)), 'records': records(data_dir / 'state.db')}


def state_directory_lock(root):
    for holder, label in [(RUST, 'rust'), (GO, 'go')]:
        hroot = root / f'lock-{label}'
        hroot.mkdir()
        e2e.BINARY = holder
        svc = e2e.Service(hroot)
        svc.start()
        try:
            svc.wait(lambda: svc.request('/state')['storage'], 'storage measurement')
            data_dir = hroot / '.octomus'
            assert (data_dir / 'service.lock').exists()
            before = lock_snapshot(data_dir)
            # Same-language and cross-language contenders both lose the flock race.
            for contender in [RUST, GO]:
                contend(contender, data_dir, svc.env)
                assert lock_snapshot(data_dir) == before, f'{contender.name} wrote into a locked data directory'
            # Read-only exports take no lock by contract, even while held.
            report = run(GO if holder == RUST else RUST, ['--data-dir', str(data_dir), '--usage-report'])
            assert report.returncode == 0 and json.loads(report.stdout)['cycles'] == [], report.stderr
        finally:
            svc.stop()
            svc.log.close()
        # A clean stop releases the lock file; the other implementation can take over.
        other = GO if holder == RUST else RUST
        views = service(other, hroot, lambda s: s.request('/state'))
        assert views['control']['paused'], f'{other.name} did not start on the released directory'
        print(f'PASS lock: {label} holder excluded both contenders; {other.name} took over after a clean stop')


def main():
    for binary, label in [(GO, 'Go executable (OCTOMUS_TEST_BINARY)'), (RUST, 'Rust reference (OCTOMUS_RUST_REFERENCE)')]:
        assert binary.exists(), f'{label} is missing: {binary}'
    with tempfile.TemporaryDirectory(prefix='octomus-go-upgrade-') as directory:
        root = Path(directory)
        upgrade_rehearsal(root)
        state_directory_lock(root)
    print('Upgrade rehearsal and cross-language locking checks passed.')


if __name__ == '__main__':
    main()
