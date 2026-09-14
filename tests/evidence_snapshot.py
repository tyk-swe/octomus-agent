#!/usr/bin/env python3
"""Synthetic-only backup/export/private-review examples; accepts no operator input."""
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import sys
import tempfile

PROJECT = Path(__file__).resolve().parents[1]
BINARY = PROJECT / 'target/debug/octomus-agent'


def example_body(document, marker, delimiter):
    """Run the documented bodies with synthetic paths, never the owner placeholders."""
    section = (PROJECT / document).read_text().split(f'<!-- {marker} -->', 1)[1]
    block = section.split('```sh\n', 1)[1].split('\n```', 1)[0]
    return block.split(f"<<'{delimiter}'\n", 1)[1].rsplit(f'\n{delimiter}', 1)[0]


def run(args, code=None):
    return subprocess.run(args, input=code, text=True, capture_output=True,
                          cwd=PROJECT, timeout=30)


def exported(directory):
    result = run([str(BINARY), '--data-dir', str(directory), '--export-run', 'synthetic-cycle'])
    assert result.returncode == 0, result.stderr
    assert not (directory / 'service.lock').exists()
    return json.loads(result.stdout)


def main():
    backup = example_body('docs/launch/run-evidence.md', 'owner-sqlite-backup', 'PY')
    validate = example_body('docs/launch/showcase.md', 'private-payload-check', 'JS')
    config_result = run([str(BINARY), '--print-config'])
    assert config_result.returncode == 0, config_result.stderr
    config = json.loads(config_result.stdout)
    config['verification_commands'] = ['synthetic required check']
    timestamp = '2026-01-01T00:00:00Z'
    proposal = {
        'id': 'synthetic-proposal', 'title': 'Synthetic WAL task',
        'problem': 'Synthetic missing behavior', 'benefit': 'Synthetic fixture only',
        'scope': 'Synthetic scope', 'evidence': ['synthetic fixture'],
        'category': 'correctness', 'target': 'main', 'tier': 'S', 'dependencies': [],
        'prompt': 'SYNTHETIC-PRIVATE-PROMPT', 'decision': 'accepted',
        'reason': 'Synthetic final decision, with missing reviewer evidence',
    }
    deferred = {**proposal, 'id': 'synthetic-deferred', 'decision': 'deferred'}
    cycle = {
        'id': 'synthetic-cycle', 'number': 1, 'mode': 'execution', 'status': 'completed',
        'started_at': timestamp, 'completed_at': timestamp, 'grounding': None,
        'repository': 'synthetic/fixture', 'proposals': [proposal, deferred],
        'assessments': [], 'sessions': [], 'error': None,
    }
    task = {
        'id': 'synthetic-task', 'cycle_id': cycle['id'], 'proposal': proposal,
        'status': 'blocked', 'blocked_reason': 'verification_failed',
        'route': config['tiers']['S'], 'config': config,
        'source_revision': 'a' * 40, 'comparison_base': 'a' * 40,
        'default_revision': 'a' * 40, 'output_commit': 'b' * 40,
        'branch': 'tyk/synthetic-wal', 'workspace': '/synthetic-private-workspace',
        'execution_session': None, 'repair_session': None, 'sessions': [], 'reviews': [],
        'verification': [{
            'command': 'synthetic required check', 'success': False,
            'output': 'SYNTHETIC-PRIVATE-OUTPUT', 'revision': 'b' * 40,
            'created_at': timestamp,
        }],
        'pr_number': None, 'pr_url': None, 'attempts': 1,
        'error': 'SYNTHETIC-PRIVATE-ERROR', 'created_at': timestamp, 'updated_at': timestamp,
    }

    os.umask(0o077)
    # Explicit /tmp keeps raw fixtures and candidates outside Git/build roots even
    # when the invoking shell has TMPDIR set to a directory inside the checkout.
    with tempfile.TemporaryDirectory(prefix='octomus-synthetic-snapshot-', dir='/tmp') as temp:
        root = Path(temp)
        source, snapshot, lossy = (root / name for name in ('source #?', 'snapshot', 'lossy'))
        for directory in (source, snapshot, lossy):
            directory.mkdir(mode=0o700)
        state = source / 'state.db'
        with closing(sqlite3.connect(state)) as writer:
            assert writer.execute('PRAGMA journal_mode=WAL').fetchone() == ('wal',)
            writer.execute('PRAGMA wal_autocheckpoint=0')
            writer.execute('CREATE TABLE records (kind TEXT, id TEXT, data TEXT, PRIMARY KEY(kind,id))')
            writer.execute('INSERT INTO records VALUES (?,?,?)',
                           ('cycle', cycle['id'], json.dumps(cycle)))
            writer.commit()
            assert writer.execute('PRAGMA wal_checkpoint(TRUNCATE)').fetchone()[0] == 0
            writer.execute('INSERT INTO records VALUES (?,?,?)',
                           ('task', task['id'], json.dumps(task)))
            writer.commit()
            wal = source / 'state.db-wal'
            assert wal.stat().st_size > 0

            # Negative control: a valid main-file copy silently loses the WAL task.
            shutil.copyfile(state, lossy / 'state.db')
            lost = exported(lossy)
            assert lost['proposals'][0]['linked_tasks'] == []
            assert lost['proposals'][0]['gaps']

            # Keep an uncommitted edit open during the backup. It must not enter
            # the snapshot, while the earlier committed WAL task must be included.
            writer.execute("UPDATE records SET data='{}' WHERE kind='task'")
            source_bytes = (state.read_bytes(), wal.read_bytes())
            saved = snapshot / 'state.db'
            result = run([sys.executable, '-', str(state), str(saved)], backup)
            assert result.returncode == 0, result.stderr
            assert (state.read_bytes(), wal.read_bytes()) == source_bytes
            with closing(sqlite3.connect(saved)) as reader:
                assert reader.execute('PRAGMA integrity_check').fetchall() == [('ok',)]
                saved_task = reader.execute("SELECT data FROM records WHERE kind='task'").fetchone()[0]
                assert json.loads(saved_task) == task
            writer.rollback()

        before = saved.read_bytes()
        refused = run([sys.executable, '-', str(state), str(saved)], backup)
        assert refused.returncode != 0
        assert saved.read_bytes() == before, 'backup overwrote an existing destination'
        absent = root / 'absent.db'
        refused = run([sys.executable, '-', str(absent), str(root / 'unused.db')], backup)
        assert refused.returncode != 0
        assert not absent.exists() and not (root / 'unused.db').exists()

        evidence = exported(snapshot)
        assert evidence['cycle']['id'] == 'synthetic-cycle'
        assert evidence['cycle']['planning']['decisions'] == {'accepted': 1, 'deferred': 1}
        accepted, held = evidence['proposals']
        assert held['final_decision'] == 'deferred' and held['linked_tasks'] == []
        assert [v['state'] for v in accepted['reviewer_verdicts']] == ['missing', 'missing']
        assert len(accepted['linked_tasks']) == 1
        linked = accepted['linked_tasks'][0]
        assert linked['id'] == 'synthetic-task' and linked['status'] == 'blocked'
        assert linked['error_recorded'] and linked['blocked_reason'] == 'verification_failed'
        assert linked['pull_request'] is None and linked['latest_review']['latest'] is None
        assert linked['required_commands']['commands'][0]['state'] == 'failed'
        assert not linked['required_commands']['all_passed_at_output_revision']
        assert evidence['gaps'] and linked['gaps']
        assert evidence['review_required_before_sharing'] and len(evidence['limitations']) == 9
        assert 'SYNTHETIC-PRIVATE-' not in json.dumps(evidence)

        # Synthetic wrapping only: no real facts, redactions, approval or public build.
        candidate = root / 'synthetic-candidate.json'
        payload = {'public_schema_version': 1, 'mode': 'recorded', 'evidence': evidence}
        candidate.write_text(json.dumps(payload, indent=2) + '\n')
        candidate_bytes = candidate.read_bytes()
        result = run(['node', '--input-type=module', '-', str(candidate)], validate)
        assert result.returncode == 0, result.stderr
        assert hashlib.sha256(candidate_bytes).hexdigest() in result.stdout
        assert 'no sharing approval granted' in result.stdout
        assert candidate.read_bytes() == candidate_bytes
        assert candidate.stat().st_mode & 0o077 == 0

        # The private check reuses P02's rejection of overwritten private members.
        candidate.write_text('{"evidence":{"transcript":"SYNTHETIC-PRIVATE-TRANSCRIPT"},'
                             + candidate_bytes.decode()[1:])
        result = run(['node', '--input-type=module', '-', str(candidate)], validate)
        assert result.returncode != 0 and 'Duplicate JSON object key' in result.stderr
        assert 'Candidate SHA-256:' not in result.stdout
    print('Synthetic snapshot examples passed: WAL backup, private CLI export, adverse evidence, hash check; no approval or build.')


if __name__ == '__main__':
    main()
