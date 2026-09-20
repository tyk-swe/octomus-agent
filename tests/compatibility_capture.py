#!/usr/bin/env python3
"""Frozen, synthetic M0 state/API/export contracts for later migration gates.

Default: verify Rust output. OCTOMUS_TEST_BINARY selects another implementation.
--update is an explicit Rust-reference-only recapture, never a Go approval path.
"""
import argparse
from contextlib import closing
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile

from e2e import BINARY, Service

PROJECT = Path(__file__).resolve().parents[1]
FIXTURES = PROJECT / 'tests/fixtures/compatibility'
REFERENCE = '3c2b5cd50924033873d7f740f9df44daee5685db'
CYCLE_ID = '11111111-1111-4111-8111-111111111111'
TASK_ID = '22222222-2222-4222-8222-222222222222'


def schema(db):
    return [list(row) for row in db.execute(
        "SELECT type,name,tbl_name,sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type,name")]


def export(root, flag):
    env = {k: v for k, v in os.environ.items() if not k.startswith('OCTOMUS_')}
    args = [str(BINARY), '--data-dir', str(root), flag]
    if flag == '--export-run':
        args.append(CYCLE_ID)
    result = subprocess.run(args, env=env, text=True, capture_output=True, timeout=15)
    assert result.returncode == 0 and not result.stderr, (args, result.stderr)
    value = json.loads(result.stdout)
    # Only the export timestamp varies; saved timestamps and identity links stay.
    assert len(value['generated_at']) > 10
    value['generated_at'] = '<export-time>'
    return value


def record_inputs():
    corpus = json.loads((FIXTURES / 'm1.json').read_text())
    cases = {c['name']: json.loads(c['input']) for c in corpus['cases']
             if c['name'] in ['Task/legacy-snapshot', 'Cycle/complete']}
    task = cases['Task/legacy-snapshot']
    task.update(id=TASK_ID, cycle_id=CYCLE_ID, branch='tyk/synthetic-frozen',
                created_at='2026-01-01T00:00:00Z', updated_at='2026-01-01T00:01:00Z',
                workspace='/synthetic-private-workspace')
    task['proposal'].update(id='synthetic-proposal', prompt='SYNTHETIC-PRIVATE-PROMPT')
    cycle = cases['Cycle/complete']
    cycle.update(id=CYCLE_ID, repository='Fixture/Project', status='completed',
                 started_at='2026-01-01T00:00:00Z', completed_at='2026-01-01T00:01:00Z',
                 proposals=[task['proposal']],
                 lifecycle={'archived_at': None, 'discarded_at': '2026-01-01T00:02:00Z'})
    cycle.pop('mode', None)  # Supported older records did not save mode.
    # A previously discarded planning workspace prevents startup housekeeping
    # from changing this snapshot while we test migrations and read-only views.
    return [
        ['settings', 'config', {'github_repo': 'Fixture/Project'}],
        ['settings', 'control', {'paused': True, 'cycle_number': 1, 'next_cycle_at': 0, 'error': None}],
        ['cycle', cycle['id'], cycle], ['task', task['id'], task],
    ]


def capture():
    records = record_inputs()
    output = {'reference': REFERENCE, 'records': records}
    with tempfile.TemporaryDirectory(prefix='octomus-frozen-contracts-') as directory:
        root = Path(directory)
        legacy = root / '.octomus'
        legacy.mkdir()
        path = legacy / 'state.db'
        with closing(sqlite3.connect(path)) as db:
            db.execute('CREATE TABLE records(kind TEXT NOT NULL,id TEXT NOT NULL,data TEXT NOT NULL,PRIMARY KEY(kind,id))')
            db.execute('CREATE TABLE usage(day TEXT PRIMARY KEY,sessions INTEGER NOT NULL)')
            db.executemany('INSERT INTO records VALUES(?,?,?)', [(kind, identity, json.dumps(value)) for kind, identity, value in records])
            db.execute("INSERT INTO usage VALUES('2026-01-01',3)")
            db.commit()
            output['legacy_schema'] = schema(db)
        before = path.read_bytes()
        output['legacy_usage'] = export(legacy, '--usage-report')
        output['legacy_evidence'] = export(legacy, '--export-run')
        assert path.read_bytes() == before and not (legacy / 'service.lock').exists(), 'read-only export wrote state'
        # Service only loads paused synthetic state. No Git repo, runner or real
        # account is configured, and no scheduler action is requested.
        service = Service(root)
        try:
            service.start()
            # Startup housekeeping records the storage observation in the
            # background; wait for it so the paused snapshot is complete.
            service.wait(lambda: service.request('/state')['storage'], 'storage measurement')
            responses = {route: service.request(route) for route in [
                '/config', '/state', f'/tasks/{TASK_ID}', f'/cycles/{CYCLE_ID}/evidence',
            ]}
            state = responses['/state']
            capacity = state['planning_capacity']
            assert len(capacity['day']) == 10 and capacity['next_reset_at'] > 0
            capacity['day'], capacity['next_reset_at'] = '<UTC-day>', '<next-UTC-midnight>'
            assert state['events'] == [], 'paused fixture unexpectedly changed state'
            state['storage']['measured_at'] = '<measurement-time>'
            responses[f'/cycles/{CYCLE_ID}/evidence']['generated_at'] = '<export-time>'
            output['api'] = responses
        finally:
            service.stop()
            service.log.close()
        with closing(sqlite3.connect(path)) as db:
            output['current_schema'] = schema(db)
            # Canonical saved records remain byte-equivalent after migrations.
            actual = [[k, i, json.loads(v)] for k, i, v in db.execute('SELECT kind,id,data FROM records ORDER BY kind,id')]
            for record in records:
                assert record in actual, ('migration rewrote source', record[0:2])
        output['current_usage'] = export(legacy, '--usage-report')
        output['current_evidence'] = export(legacy, '--export-run')
    return output


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--update', action='store_true')
    args = parser.parse_args()
    if args.update:
        assert BINARY.resolve() == (PROJECT / 'target/debug/octomus-agent').resolve(), 'capture expectations only from Rust'
    actual = capture()
    path = FIXTURES / 'state.json'
    if args.update:
        path.write_text(json.dumps(actual, indent=2, ensure_ascii=False) + '\n')
    else:
        assert actual == json.loads(path.read_text()), 'frozen state/API/export contracts differ'
    print('Frozen synthetic state contracts passed: legacy/current schemas, read-only reports and exports, paused API responses.')
