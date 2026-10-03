#!/usr/bin/env python3
"""Runs the actual service, scheduler, SQLite, and Git against deterministic external peers.
No network writes, real Codex turns, credentials, or spending. Run after make build (dashboard + Go binary) or set OCTOMUS_TEST_BINARY.
Every e2e suite accepts scenario names (`python3 tests/e2e.py normal audit-accepted`) to run only those;
an unknown name lists them all. Scenarios run with up to four workers by default;
OCTOMUS_TEST_JOBS sets the limit (1 runs serially). The shared harness is tests/harness.py.
"""
import functools
import json
import subprocess
import sys

from harness import BINARY, CODEX_ROUTE, TOKEN, base_config, fixture_service, git, run_selected, usage_report, use_codex_routes


def scenario(mode):
    def prepare(root):
        if mode != 'normal':
            (root / mode).touch()

    with fixture_service(f'octomus-{mode}-', prepare) as (root, service):
        service.configure()
        if mode == 'interrupt-publication':
            service.wait(lambda: (root / 'publication-created').exists(), 'publication side effect')
            service.stop(crash=True)
            service.start()
            task = service.wait(service.terminal_task, 'recovered publication')
            assert task['status'] == 'published', task['error']
        task = service.wait(service.terminal_task, 'task completion')
        if mode == 'parallel':
            service.wait(lambda: len([t for t in service.request('/state')['tasks'] if t['status'] == 'published']) == 2, 'both tasks delivered')
            all_tasks = [service.request(f'/tasks/{t["id"]}') for t in service.request('/state')['tasks']]
            assert len({t['workspace'] for t in all_tasks}) == 2
            assert len({t['execution_session'] for t in all_tasks}) == 2
        if mode == 'failed-verification':
            assert task['status'] == 'blocked', task
            assert not (root / 'publications.jsonl').exists(), 'Unresolved work must not publish'
            print(f'PASS {mode}: blocked, never published, workspace retained')
            return
        assert task['status'] == 'published', task['error']
        assert len(task['reviews']) == 3, task['reviews']
        assert len({r['session_id'] for r in task['reviews']}) == 3
        assert all(r['comparison_base'] == task['default_revision'] for r in task['reviews'])
        repairs = [s for s in task['sessions'] if s['role'] == 'repair']
        assert len(repairs) == 1 and repairs[0]['route'] == task['config']['repair_route']
        assert task['workspace'].endswith(f'tasks/{task["id"]}/workspace')
        assert task['verification'][-1]['success']
        assert task['verification'][-1]['revision'] == task['output_commit']
        expected_tasks = 2 if mode == 'parallel' else 1
        assert len(json.loads((root / 'prs.json').read_text())) == expected_tasks
        assert len((root / 'publications.jsonl').read_text().splitlines()) == expected_tasks
        assert git('rev-parse', 'main', cwd=root / 'remote.git') == task['default_revision'], 'Default branch must never be pushed'
        protocol = [json.loads(line) for line in (root / 'protocol.jsonl').read_text().splitlines()]
        assert len([p for p in protocol if p['prompt'].startswith('Discover worthwhile')]) == 9
        assert len([p for p in protocol if p['prompt'].startswith('Adversarial proposal')]) == 2
        assert len({p['thread'] for p in protocol if p['prompt'].startswith('Repair actionable')}) == expected_tasks
        assert all(p['sandbox'] == {'type': 'dangerFullAccess'} and p['approval'] == 'never' for p in protocol)
        report = usage_report(root)
        assert len(report['admissions']) == 13 + 6 * expected_tasks
        assert sum(a['role'] == 'repair' for a in report['admissions']) == 2 * expected_tasks
        assert report['cycles'][0]['planning_admissions'] == 13
        assert report['cycles'][0]['task_admissions'] == 6 * expected_tasks
        if mode == 'normal':
            service.stop()
            (root / 'version').write_text('0.0.0-fixture')
            diagnostic = subprocess.run([str(BINARY), '--data-dir', str(root / '.octomus'), '--doctor'], env=service.env, capture_output=True, text=True, check=True, timeout=60)
            assert json.loads(diagnostic.stdout)['warnings']
            assert 'mismatch' in diagnostic.stderr
        print(f'PASS {mode}: complete reviewed delivery with no duplicate PRs')


def settings_scenario():
    """The settings contract keeps canonical state distinct from the display view.

    A secret-bearing command is served redacted with transform metadata, the
    canonical revision identifies the saved configuration, a partial update
    preserves every untouched field (including hidden ones), stale revisions
    conflict without persisting, and baseline admission validates the revision
    instead of an echoed display object.
    """
    with fixture_service('octomus-settings-') as (root, service):
        view = service.request('/config')
        assert set(view) == {'config', 'revision', 'transformed_fields'}, view
        assert view['transformed_fields'] == [] and len(view['revision']) == 64, view

        config = base_config(service, [])
        use_codex_routes(config)
        proof = root / 'canonical-proof'
        secret_command = f'echo {TOKEN} > {proof}'
        displayed = f'echo [redacted] > {proof}'
        config['verification_commands'] = [secret_command]
        saved = service.save_config(config)
        for patch, message in [({'default_branch': 'refs/heads/main'}, 'Default branch'),
                               ({'default_branch': 'aB09' * 10}, 'Default branch'),
                               ({'branch_prefix': 'refs/tasks/'}, 'Owned branch prefix')]:
            code, refusal = service.expect('/config', 'PUT', {'expected_revision': saved['revision'], 'config': patch})
            assert code == 400 and message in refusal['error'], (code, refusal)
            assert service.request('/config') == saved
        entry = next(t for t in saved['transformed_fields'] if t['field'] == 'verification_commands')
        assert entry['kinds'] == ['redacted'] and entry['paths'] == [['verification_commands', 0]], saved['transformed_fields']
        assert saved['config']['verification_commands'] == [displayed]
        assert saved['revision'] != view['revision'] and len(saved['revision']) == 64

        retries = saved['config']['max_retries'] + 1
        code, updated = service.expect('/config', 'PUT', {'expected_revision': saved['revision'], 'config': {'max_retries': retries}})
        assert code == 200, updated
        assert updated['config']['verification_commands'] == [displayed]
        assert updated['config']['max_retries'] == retries and updated['revision'] != saved['revision']
        assert next(t for t in updated['transformed_fields'] if t['field'] == 'verification_commands')
        diagnostic = service.request('/doctor', 'POST')
        assert diagnostic['checked_revision'] == updated['revision']
        assert diagnostic['checked_config']['verification_commands'] == [displayed]
        assert not proof.exists()

        code, refusal = service.expect('/config', 'PUT', {'expected_revision': saved['revision'], 'config': {'max_retries': retries + 1}})
        assert code == 409 and 'changed' in refusal['error'], (code, refusal)
        code, refusal = service.expect('/baseline-checks', 'POST', {'expected_revision': saved['revision']})
        assert code == 409, (code, refusal)
        assert service.request('/baseline-checks/latest')['check'] is None
        assert not (root / '.octomus/baselines').exists()

        code, check = service.expect('/baseline-checks', 'POST', {'expected_revision': updated['revision']})
        assert code == 202, (code, check)
        assert check['config']['verification_commands'] == [displayed]
        assert check['config_fingerprint'] == updated['revision']
        finished = service.wait(lambda: (c := service.request('/baseline-checks/latest')['check']) and c['status'] != 'running' and c, 'baseline completion')
        assert finished['status'] == 'passed' and finished['commands'][0]['command'] == displayed, finished
        assert proof.read_text().strip() == TOKEN, 'the canonical secret-bearing command ran, not its display preview'
        print('PASS settings-view: canonical revision gates saves and baselines; hidden values stay canonical')


def audit_scenario():
    with fixture_service('octomus-audit-') as (root, service):
        c = base_config(service, [], task_timeout_seconds=120)
        for role in ['orchestrator', 'discovery', 'proposal_reviewer']:
            c['roles'][role] = dict(CODEX_ROUTE)
        c['roles']['code_reviewer'] = {'model': 'unavailable', 'effort': 'high'}
        c['repair_route'] = {'model': 'unavailable', 'effort': 'high'}
        service.save_config(c)
        diagnostic = service.request('/doctor?mode=audit', 'POST')
        assert diagnostic['mode'] == 'audit'
        assert diagnostic['checked_revision'] == service.request('/config')['revision']
        assert diagnostic['checked_config'] == service.request('/config')['config']
        code, body = service.expect('/doctor', 'POST')
        assert code == 400, ('Execution doctor accepted missing verification', code, body)
        assert body['checked_revision'] == service.request('/config')['revision']
        assert body['checked_config'] == service.request('/config')['config']
        (root / 'audit-decisions').touch()
        (root / 'audit-hold').touch()
        baseline_revision = git('rev-parse', 'main', cwd=root / 'remote.git')
        baseline_refs = git('for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads', cwd=root / 'remote.git')
        code, response = service.expect('/control/audit', 'POST')
        assert code == 200, (code, response)
        service.wait(lambda: (root / 'audit-entered').exists(), 'audit started')
        state = service.request('/state')
        assert state['status'] == 'auditing' and state['control']['paused']
        for action in ['audit', 'resume', 'cycle']:
            code, body = service.expect('/control/' + action, 'POST')
            assert code == 409, ('Conflicting control accepted', action, code, body)
            message = body['error']
            explanation = {
                'audit': 'Audits require paused operation with no active work',
                'cycle': 'Run once requires paused operation with no active work',
                'resume': 'Wait for the audit to finish before starting continuous operation',
            }[action]
            assert message.startswith(explanation), message
        (root / 'audit-hold').unlink()

        def completed_audit():
            state = service.request('/state')
            return state if state['cycles'] and not state['cycle_active'] and state['cycles'][0]['status'] == 'completed' else None
        state = service.wait(completed_audit, 'audit completion')
        cycle = service.request('/cycles/' + state['cycles'][0]['id'])
        assert cycle['mode'] == 'audit' and state['control']['paused']
        assert state['tasks'] == []
        assert {p['decision'] for p in cycle['proposals']} == {'accepted', 'rejected', 'deferred'}
        assert all(p['reason'] for p in cycle['proposals']) and len(cycle['assessments']) == 2
        report = usage_report(root)
        row = next(c for c in report['cycles'] if c['id'] == cycle['id'])
        assert row['mode'] == 'audit' and row['task_admissions'] == 0
        assert row['planning_admissions'] == 13
        observed = service.request('/state')['pr_capacity']['observed_at']
        service.stop()
        service.start()
        service.wait(lambda: service.request('/state')['pr_capacity']['observed_at'] != observed, 'fresh PR observation after restart')
        assert service.request('/state')['tasks'] == []
        assert not (root / 'publications.jsonl').exists()
        assert git('rev-parse', 'main', cwd=root / 'remote.git') == baseline_revision
        assert git('for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads', cwd=root / 'remote.git') == baseline_refs
        service.stop()
        diagnostic = subprocess.run([str(BINARY), '--data-dir', str(root / '.octomus'), '--doctor', '--audit'], env=service.env, capture_output=True, text=True, check=True, timeout=60)
        assert json.loads(diagnostic.stdout)['mode'] == 'audit'
        print('PASS audit-accepted: durable decisions, paused queue, no publication')


SCENARIOS = [
    ('settings', settings_scenario),
    *[(mode, functools.partial(scenario, mode)) for mode in ['normal', 'parallel', 'failed-verification', 'interrupt-publication']],
    ('audit-accepted', audit_scenario),
]


if __name__ == '__main__':
    run_selected('e2e', SCENARIOS, sys.argv[1:])
