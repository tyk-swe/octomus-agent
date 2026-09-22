#!/usr/bin/env python3
"""Cross-language storage checks for the Go store (roadmap M2, criterion 8).

Every database here is a synthetic temporary fixture. The frozen Rust reference
binary and the Go executable never run concurrently against one directory: each
hand-off copies the state files into an isolated directory first, and the
sequence is strictly serial.

Direction 1: Go reads Rust-created records. The Rust reference upgrades the
frozen legacy fixture and adds its own startup records; the Go executable then
exports the same usage report and run evidence, and a Go store reopen leaves
every record and the schema unchanged, after which the Rust reference still
produces identical exports (rollback).

Direction 2: the Rust reference reads Go-written records. The Go fixture writer
authors a plan, admissions, an event, a PR observation and a reservation through
the Go store; the Rust reference exports and serves that state from an isolated
copy and leaves the Go-written records intact.

Environment: OCTOMUS_TEST_BINARY names the Go executable; OCTOMUS_RUST_REFERENCE
names the frozen Rust reference (default target/debug/octomus-agent).
"""
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
from pathlib import Path

PROJECT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PROJECT / 'tests'))
import compatibility_capture  # noqa: E402
import e2e  # noqa: E402

GO = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(PROJECT / 'bin/octomus-agent')))
RUST = Path(os.environ.get('OCTOMUS_RUST_REFERENCE', str(PROJECT / 'target/debug/octomus-agent')))
FIXTURE = PROJECT / 'tests/fixtures/compatibility/state.json'
STATE_FILES = ['state.db', 'state.db-wal', 'state.db-shm', 'state.db-journal']


def clean_env():
    return {key: value for key, value in os.environ.items() if not key.startswith('OCTOMUS_')}


def run(binary, args, cwd=None):
    return subprocess.run([str(binary), *args], cwd=cwd, env=clean_env(), capture_output=True, text=True, timeout=120)


def export(binary, data_dir, flag, cycle_id):
    """A read-only export that must succeed silently and never leave a lock.

    Returns the parsed document and its text with the generation time removed,
    so callers can require byte-identical output across implementations."""
    args = ['--data-dir', str(data_dir), flag] + ([cycle_id] if flag == '--export-run' else [])
    # A stopped service leaves its lock file behind; an export must never create one.
    locked = (data_dir / 'service.lock').exists()
    result = run(binary, args)
    assert result.returncode == 0 and result.stderr == '', f'{binary.name} {flag}: {result.returncode}\n{result.stderr}'
    assert locked or not (data_dir / 'service.lock').exists(), f'{binary.name} {flag} took the service lock'
    value = json.loads(result.stdout)
    assert isinstance(value.pop('generated_at'), str)
    lines = [line for line in result.stdout.splitlines() if not line.startswith('  "generated_at": ')]
    return value, '\n'.join(lines)


def exports(binary, data_dir, cycle_id):
    """Parsed exports plus their exact text, which must match across implementations."""
    usage, usage_text = export(binary, data_dir, '--usage-report', cycle_id)
    evidence, evidence_text = export(binary, data_dir, '--export-run', cycle_id)
    return {'usage': usage, 'evidence': evidence, 'text': {'usage': usage_text, 'evidence': evidence_text}}


def schema(path):
    with sqlite3.connect(path) as db:
        return compatibility_capture.schema(db)


def records(path):
    """Every canonical record as stored bytes; migrations must never rewrite them."""
    with sqlite3.connect(path) as db:
        rows = db.execute('SELECT kind, id, data FROM records ORDER BY kind, id').fetchall()
        version = db.execute('PRAGMA user_version').fetchone()[0]
        admissions = db.execute("SELECT count(*) FROM sqlite_master WHERE name='admissions'").fetchone()[0]
        ledger = db.execute('SELECT id, at, day, data FROM admissions ORDER BY at, id').fetchall() if admissions else None
    return {'rows': rows, 'user_version': version, 'admissions': ledger}


def canonical(snapshot):
    """Records the service treats as history plus the ledger; a running service
    rewrites only its own settings (for example the storage measurement)."""
    return {'rows': [row for row in snapshot['rows'] if row[0] != 'settings'],
            'user_version': snapshot['user_version'], 'admissions': snapshot['admissions']}


def state_files(directory):
    return sorted(name for name in os.listdir(directory) if name.startswith('state.db'))


def copy_state(source, destination):
    """Isolate a hand-off: copy the database and any WAL sidecars, nothing else."""
    destination.mkdir(parents=True)
    for name in STATE_FILES:
        if (source / name).exists():
            shutil.copy2(source / name, destination / name)
    return destination


def rust_service(root, checks):
    """Run the frozen Rust reference paused against root/.octomus and collect API views."""
    e2e.BINARY = RUST
    service = e2e.Service(root)
    service.start()
    try:
        service.wait(lambda: service.request('/state')['storage'], 'storage measurement')
        return checks(service)
    finally:
        service.stop()


def go_fixture(mode, path):
    result = subprocess.run(['go', 'run', './tests/go/rollbackfixture', mode, str(path)], cwd=PROJECT,
                            env=clean_env(), capture_output=True, text=True, timeout=600)
    assert result.returncode == 0, f'rollbackfixture {mode}: {result.returncode}\n{result.stderr}'
    return json.loads(result.stdout)


def go_reads_rust_state(root):
    fixture = json.loads(FIXTURE.read_text())
    cycle_id = compatibility_capture.CYCLE_ID
    task_id = compatibility_capture.TASK_ID
    data_dir = root / 'rust-origin/.octomus'
    data_dir.mkdir(parents=True)
    with sqlite3.connect(data_dir / 'state.db') as db:
        for _, _, _, sql in fixture['legacy_schema']:
            db.execute(sql)
        db.executemany('INSERT INTO records VALUES(?,?,?)',
                       [(kind, identity, json.dumps(value)) for kind, identity, value in fixture['records']])
        db.execute("INSERT INTO usage VALUES('2026-01-01',3)")
    # The Rust reference upgrades the legacy database and records its own startup state.
    rust_views = rust_service(root / 'rust-origin', lambda s: {
        'task': s.request(f'/tasks/{task_id}'), 'cycle_ids': sorted(c['id'] for c in s.request('/state')['cycles'])})
    assert rust_views['task']['id'] == task_id and rust_views['cycle_ids'] == [cycle_id]
    rust_before = exports(RUST, data_dir, cycle_id)
    for name in ['usage', 'evidence']:
        frozen = dict(fixture[f'current_{name}'])
        frozen.pop('generated_at')
        assert rust_before[name] == frozen, f'Rust reference {name} export drifted from the frozen fixture'
    assert schema(data_dir / 'state.db') == fixture['current_schema'], 'Rust reference schema drifted from the frozen fixture'
    before = records(data_dir / 'state.db')
    assert before['user_version'] == 6

    # Go exports observe the same facts from the Rust-written database.
    go_before = exports(GO, data_dir, cycle_id)
    assert go_before == rust_before, f'Go exports differ from the Rust reference\n{json.dumps(go_before, indent=2)}\n{json.dumps(rust_before, indent=2)}'
    assert records(data_dir / 'state.db') == before, 'Go export changed Rust-written state'

    # A Go store reopen runs the idempotent migration path without touching a record.
    files_before = state_files(data_dir)
    summary = go_fixture('reopen', data_dir / 'state.db')
    assert summary['tasks'] == [task_id] and summary['cycles'] == [cycle_id], summary
    assert summary['counts'].get('blocked') == 1 and summary['attention_tasks'] == [task_id], summary
    assert records(data_dir / 'state.db') == before, 'Go reopen rewrote Rust-written records or admissions'
    assert schema(data_dir / 'state.db') == fixture['current_schema'], 'Go reopen changed the schema'
    assert state_files(data_dir) == files_before, state_files(data_dir)

    # Rollback: the Rust reference reads the database Go reopened, unchanged.
    assert exports(RUST, data_dir, cycle_id) == rust_before, 'Rust exports changed after the Go reopen'
    rust_after = rust_service(root / 'rust-origin', lambda s: {
        'task': s.request(f'/tasks/{task_id}'), 'cycle_ids': sorted(c['id'] for c in s.request('/state')['cycles'])})
    assert rust_after == rust_views, 'Rust API views changed after the Go reopen'
    assert canonical(records(data_dir / 'state.db')) == canonical(before), 'The Rust rollback rewrote records'
    return {'cycle': cycle_id, 'records': len(before['rows']), 'schema_rows': len(fixture['current_schema'])}


def rust_reads_go_state(root):
    data_dir = root / 'go-origin/.octomus'
    data_dir.mkdir(parents=True)
    summary = go_fixture('write', data_dir / 'state.db')
    cycle_id, task_id = summary['cycle_id'], summary['task_id']
    assert summary['tasks'] == [task_id] and summary['reservations'] == 1 and summary['events'] == 1, summary
    written = records(data_dir / 'state.db')
    assert written['user_version'] == 6 and len(written['admissions']) == 2, written['user_version']
    kinds = sorted({kind for kind, _, _ in written['rows']})
    assert kinds == ['cycle', 'decision', 'pr', 'settings', 'task'], kinds
    go_exports = exports(GO, data_dir, cycle_id)
    assert go_exports['usage']['has_admission_ledger'] and len(go_exports['usage']['admissions']) == 2
    # Run evidence omits private fields; both reports redact token-shaped text.
    assert 'SYNTHETIC-PRIVATE-' not in json.dumps(go_exports['evidence']), 'Go evidence leaked private text'
    assert 'ghp_' not in json.dumps(go_exports), 'Go export leaked a token'

    # The Rust reference works on an isolated copy so the Go-written original is never shared.
    rollback = copy_state(data_dir, root / 'rollback/.octomus')
    rust_exports = exports(RUST, rollback, cycle_id)
    assert rust_exports == go_exports, f'Rust reference exports differ from Go\n{json.dumps(rust_exports, indent=2)}\n{json.dumps(go_exports, indent=2)}'
    assert records(rollback / 'state.db') == written, 'Rust export changed Go-written state'

    def views(service):
        state = service.request('/state')
        task = service.request(f'/tasks/{task_id}')
        evidence = service.request(f'/cycles/{cycle_id}/evidence')
        assert isinstance(evidence.pop('generated_at'), str)
        return {'tasks': sorted(t['id'] for t in state['tasks']), 'cycles': sorted(c['id'] for c in state['cycles']),
                'blocked': state['counts'].get('blocked'), 'task_status': task['status'],
                'blocked_reason': task['blocked_reason'], 'branch': task['branch'], 'evidence': evidence}
    rust_views = rust_service(root / 'rollback', views)
    assert rust_views['tasks'] == [task_id] and rust_views['cycles'] == [cycle_id], rust_views
    assert rust_views['blocked'] == 1 and rust_views['task_status'] == 'blocked', rust_views
    assert rust_views['blocked_reason'] == 'verification_failed' and rust_views['branch'] == 'tyk/synthetic-go', rust_views
    assert rust_views['evidence'] == go_exports['evidence'], 'Rust API evidence differs from the Go export'

    # The Rust service left every Go-written canonical record and admission intact,
    # and the Go executable still reads the state the Rust reference served.
    after = records(rollback / 'state.db')
    assert canonical(after) == canonical(written), 'Rust rewrote Go-written records or changed the ledger'
    assert schema(rollback / 'state.db') == schema(data_dir / 'state.db'), 'Rust changed the Go schema'
    assert exports(GO, rollback, cycle_id) == go_exports, 'Go exports changed after the Rust service ran'
    assert records(data_dir / 'state.db') == written, 'the Go-written original was touched'
    return {'cycle': cycle_id, 'records': len(written['rows']), 'admissions': len(written['admissions'])}


def main():
    for binary, label in [(GO, 'Go executable (OCTOMUS_TEST_BINARY)'), (RUST, 'Rust reference (OCTOMUS_RUST_REFERENCE)')]:
        assert binary.exists(), f'{label} is missing: {binary}'
    with tempfile.TemporaryDirectory(prefix='octomus-go-storage-') as directory:
        root = Path(directory)
        first = go_reads_rust_state(root)
        second = rust_reads_go_state(root)
    print(f'Cross-language storage checks passed: Go read the Rust-upgraded fixture ({first["records"]} records, '
          f'{first["schema_rows"]} schema rows) and the Rust reference read Go-written state '
          f'({second["records"]} records, {second["admissions"]} admissions) from an isolated rollback copy.')


if __name__ == '__main__':
    main()
