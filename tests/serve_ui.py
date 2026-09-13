#!/usr/bin/env python3
"""Temporary, clearly synthetic data for browser tests; never used by the shipped app."""
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
from datetime import datetime, timezone

project = Path(__file__).resolve().parents[1]
binary = project / 'target/debug/octomus-agent'
with tempfile.TemporaryDirectory(prefix='octomus-browser-') as directory:
    data = Path(directory)
    config = json.loads(subprocess.check_output([str(binary), '--print-config']))
    # Tasks embed the configuration they ran under; a configured check makes the recorded
    # check evidence exercisable instead of reporting "no checks configured".
    task_config = {**config, 'verification_commands': ['cargo test']}
    now = datetime.now(timezone.utc).isoformat()
    db = sqlite3.connect(data / 'state.db')
    db.execute('CREATE TABLE records (kind TEXT NOT NULL, id TEXT NOT NULL, data TEXT NOT NULL, PRIMARY KEY(kind,id))')
    def put(kind, identity, value):
        db.execute('INSERT INTO records VALUES (?,?,?)', (kind, identity, json.dumps(value)))
    rows = [('task-active', 'Complete the repository setup flow', 'queued', 'features', 'M'), ('task-reviewed', 'Explain the local development workflow', 'published', 'documentation', 'S'), ('task-blocked', 'Handle interrupted verification commands', 'blocked', 'correctness', 'M')]
    proposals = []
    for identity, title, status, category, tier in rows:
        proposal = {'id': identity, 'title': title, 'problem': 'A project-specific improvement grounded in repository evidence.', 'evidence': ['src/main.rs: service lifecycle'], 'benefit': 'A clearer and more reliable project.', 'category': category, 'target': 'main', 'tier': tier, 'scope': 'Preserve existing behavior and add proportionate verification.', 'dependencies': [], 'prompt': 'Complete the accepted improvement with useful verification and accurate documentation.', 'decision': 'accepted', 'reason': 'The orchestrator and both independent reviewers found a concrete benefit.'}
        proposals.append(proposal)
        review = {'session_id': 'review-session', 'revision': 'b' * 40, 'comparison_base': 'a' * 40, 'created_at': now, 'result': {'completed': True, 'summary': 'The full change set meets the objective without actionable findings.', 'findings': []}}
        put('task', identity, {'id': identity, 'cycle_id': 'cycle-1', 'proposal': proposal, 'status': status, 'route': config['tiers'][tier], 'config': task_config, 'source_revision': 'a' * 40, 'comparison_base': 'a' * 40, 'default_revision': 'a' * 40, 'branch': f'octomus/{identity}', 'workspace': f'/srv/project/.octomus/tasks/{identity}/workspace', 'execution_session': 'execution-session' if status != 'queued' else None, 'repair_session': None, 'sessions': [{'id': 'execution-session', 'role': 'executor', 'route': config['tiers'][tier], 'status': 'completed', 'started_at': now, 'summary': 'Implemented and verified the accepted scope.'}] if status != 'queued' else [], 'reviews': [review] if status == 'published' else [], 'verification': [{'command': 'cargo test', 'success': True, 'output': 'All tests passed.', 'revision': 'b' * 40, 'created_at': now}] if status == 'published' else [], 'output_commit': 'b' * 40 if status == 'published' else None, 'pr_number': 12 if status == 'published' else None, 'pr_url': 'https://github.com/fixture/project/pull/12' if status == 'published' else None, 'attempts': 0, 'error': 'Verification timed out. Workspace preserved for inspection.' if status == 'blocked' else None, 'created_at': now, 'updated_at': now})
    # Two positional reviewer slots, each with a completed session and a saved batch, so
    # the recorded run evidence has attributable verdicts. Clearly synthetic reasons.
    slots = ['adversary-a', 'adversary-b']
    reviewer_sessions = [{'id': f'{slot}-session', 'role': slot, 'route': config['roles']['proposal_reviewer'], 'status': 'completed', 'started_at': now, 'summary': f'Synthetic browser-test review recorded for {slot}.'} for slot in slots]
    batches = [{'assessments': [{'id': p['id'], 'decision': 'accepted', 'reason': f'Synthetic browser-test verdict recorded for {slot}: the saved scope is concrete and bounded.'} for p in proposals]} for slot in slots]
    put('cycle', 'cycle-1', {'id': 'cycle-1', 'number': 1, 'status': 'completed', 'started_at': now, 'completed_at': now, 'grounding': {'revision': 'a' * 40, 'prs': [], 'history': [], 'maintenance_due': False, 'maintenance_targets': []}, 'proposals': proposals, 'assessments': batches, 'sessions': reviewer_sessions, 'error': None})
    put('settings', 'prs', [{'number': 12, 'title': rows[1][1], 'branch': 'octomus/task-reviewed', 'head': 'b' * 40, 'base': 'main', 'url': 'https://github.com/fixture/project/pull/12', 'body': 'Synthetic browser test PR.', 'state': 'open', 'changed_lines': 42, 'created_at': now, 'owned': True}])
    db.commit()
    db.close()
    process = subprocess.Popen([str(binary), '--listen', '127.0.0.1:4299', '--data-dir', directory, '--assets', str(project / 'web/build')], env={**os.environ, 'OCTOMUS_TOKEN': 'browser-test-operator-token-32-characters'})
    try:
        process.wait()
    except KeyboardInterrupt:
        process.terminate()
        process.wait()
