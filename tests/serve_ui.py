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
binary = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(project / 'bin/octomus-agent'))).resolve()
with tempfile.TemporaryDirectory(prefix='octomus-browser-') as directory:
    data = Path(directory)
    config = json.loads(subprocess.check_output([str(binary), '--print-config']))
    # Shipped routes carry effort but no model, so the synthetic service picks one
    # and the browser fixture shows a configured project rather than a blank form.
    for role in config['roles']:
        config['roles'][role] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
    for tier, effort in [('XS', 'xhigh'), ('S', 'max'), ('M', 'low'), ('L', 'medium'), ('XL', 'high')]:
        config['tiers'][tier] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': effort}
    config['repair_route'] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
    # Tasks embed the configuration they ran under; a configured check makes the recorded
    # check evidence exercisable instead of reporting "no checks configured".
    task_config = {**config, 'verification_commands': ['go test ./...']}
    now = datetime.now(timezone.utc).isoformat()
    subprocess.run(['go', 'run', './tests/fixturedb', str(data / 'state.db')], cwd=project, check=True)
    db = sqlite3.connect(data / 'state.db')
    def put(kind, identity, value):
        db.execute('INSERT INTO records VALUES (?,?,?)', (kind, identity, json.dumps(value)))
    rows = [('task-active', 'Complete the repository setup flow', 'queued', 'features', 'M'), ('task-reviewed', 'Explain the local development workflow', 'published', 'documentation', 'S'), ('task-blocked', 'Handle interrupted verification commands', 'blocked', 'correctness', 'M')]
    proposals = []
    for identity, title, status, category, tier in rows:
        proposal = {'id': identity, 'title': title, 'problem': 'A project-specific improvement grounded in repository evidence.', 'evidence': ['cmd/octomus-agent/main.go: service lifecycle'], 'benefit': 'A clearer and more reliable project.', 'category': category, 'target': 'main', 'tier': tier, 'scope': 'Preserve existing behavior and add proportionate verification.', 'dependencies': [], 'prompt': 'Complete the accepted improvement with useful verification and accurate documentation.', 'decision': 'accepted', 'reason': 'The orchestrator and both independent reviewers found a concrete benefit.', 'problem_key': '', 'relevant_paths': [], 'reconsiders': []}
        proposals.append(proposal)
        review = {'session_id': 'review-session', 'revision': 'b' * 40, 'comparison_base': 'a' * 40, 'created_at': now, 'result': {'completed': True, 'summary': 'The full change set meets the objective without actionable findings.', 'findings': []}}
        put('task', identity, {'id': identity, 'cycle_id': 'cycle-1', 'proposal': proposal, 'status': status, 'route': config['tiers'][tier], 'config': task_config, 'source_revision': 'a' * 40, 'comparison_base': 'a' * 40, 'default_revision': 'a' * 40, 'branch': f'octomus/{identity}', 'workspace': f'/srv/project/.octomus/tasks/{identity}/workspace', 'execution_session': 'execution-session' if status != 'queued' else None, 'repair_session': None, 'sessions': [{'id': 'execution-session', 'role': 'executor', 'route': config['tiers'][tier], 'status': 'completed', 'started_at': now, 'summary': 'Implemented and verified the accepted scope.'}] if status != 'queued' else [], 'reviews': [review] if status == 'published' else [], 'verification': [{'command': 'go test ./...', 'success': True, 'output': 'All tests passed.', 'revision': 'b' * 40, 'created_at': now}] if status == 'published' else [], 'output_commit': 'b' * 40 if status == 'published' else None, 'pr_number': 12 if status == 'published' else None, 'pr_url': 'https://github.com/fixture/project/pull/12' if status == 'published' else None, 'attempts': 0, 'review_baseline': 0, 'superseded_by': [], 'supersedes': [], 'rediscovery_requested': False, 'lifecycle': {'archived_at': None, 'discarded_at': None}, 'error': 'Verification timed out. Workspace preserved for inspection.' if status == 'blocked' else None, 'created_at': now, 'updated_at': now})
    # Two positional reviewer slots, each with a completed session and a saved batch, so
    # the recorded run evidence has attributable verdicts. Clearly synthetic reasons.
    slots = ['adversary-a', 'adversary-b']
    reviewer_sessions = [{'id': f'{slot}-session', 'role': slot, 'route': config['roles']['proposal_reviewer'], 'status': 'completed', 'started_at': now, 'summary': f'Synthetic browser-test review recorded for {slot}.'} for slot in slots]
    batches = [{'assessments': [{'id': p['id'], 'decision': 'accepted', 'reason': f'Synthetic browser-test verdict recorded for {slot}: the saved scope is concrete and bounded.'} for p in proposals]} for slot in slots]
    put('cycle', 'cycle-1', {'id': 'cycle-1', 'number': 1, 'mode': 'execution', 'status': 'completed', 'started_at': now, 'completed_at': now, 'grounding': {'revision': 'a' * 40, 'prs': [], 'external_prs': [{'number': 31, 'url': 'https://github.com/fixture/project/pull/31', 'title': 'Adjust the retry backoff', 'body': 'Synthetic browser test context.', 'branch': 'contributor/backoff', 'head': 'c' * 40, 'base': 'main', 'head_repository': 'contributor/project', 'base_repository': 'fixture/project', 'title_truncated': False, 'body_truncated': False}], 'pr_coverage': {'observed_at': now, 'complete': True, 'total_open': 2, 'total_external': 1, 'included_external': 1, 'omitted_external': 0, 'max_external': 20, 'max_title_chars': 200, 'max_body_chars': 2000, 'max_context_bytes': 20000}, 'history': [], 'maintenance_due': False, 'maintenance_targets': []}, 'proposals': proposals, 'assessments': batches, 'sessions': reviewer_sessions, 'error': None})
    # An owned delivery at its delivered head, an external request and an owned delivery
    # whose head moved afterwards exercise every ownership and head-movement state. As in
    # store.RecordPrObservation, movement is only ever measured against a delivered head,
    # which an external request never has.
    def observation(number, title, branch, owned, head, delivered=None):
        pull = {'number': number, 'title': title, 'branch': branch, 'head': head, 'base': 'main', 'url': f'https://github.com/fixture/project/pull/{number}', 'body': 'Synthetic browser test pull request.', 'state': 'open', 'changed_lines': 42, 'created_at': now, 'owned': owned, 'head_repository': 'fixture/project' if owned else 'contributor/project', 'base_repository': 'fixture/project'}
        return {'repository': 'fixture/project', 'pr': pull, 'observed_at': now, 'delivered_head': delivered, 'external_head_movement': delivered is not None and delivered != head}
    put('pr', 'fixture/project:12', observation(12, rows[1][1], 'octomus/task-reviewed', True, 'b' * 40, delivered='b' * 40))
    put('pr', 'fixture/project:31', observation(31, 'Adjust the retry backoff', 'contributor/backoff', False, 'c' * 40))
    put('pr', 'fixture/project:7', observation(7, 'Record the first delivered change', 'octomus/first-delivery', True, 'd' * 40, delivered='e' * 40))
    db.commit()
    db.close()
    env = {key: value for key, value in os.environ.items() if key != 'OCTOMUS_NOTIFICATION_WEBHOOK_URL'}
    env['OCTOMUS_TOKEN'] = 'browser-test-operator-token-32-characters'
    process = subprocess.Popen([str(binary), '--listen', '127.0.0.1:4299', '--data-dir', directory, '--assets', str(project / 'web/build')], env=env)
    try:
        process.wait()
    except KeyboardInterrupt:
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
