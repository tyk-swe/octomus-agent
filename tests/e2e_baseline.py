#!/usr/bin/env python3
import functools
import json
import sqlite3
import subprocess
import sys

from harness import base_config, fixture_service, poll, run_selected, usage_report


def save_config(service, empty_models=False, **overrides):
    """Saves a fresh configuration and returns the settings view.

    The view's `revision` is the canonical fingerprint baseline admission and
    later saves must echo back; `config` holds the display-safe values.
    """
    config = base_config(service, ['true'], cycle_interval_seconds=3600, task_timeout_seconds=60)
    if empty_models:
        for role in config['roles']:
            config['roles'][role] = {'backend': 'codex', 'model': '', 'effort': ''}
        for tier in config['tiers']:
            config['tiers'][tier] = {'model': '', 'effort': ''}
        config['repair_route'] = {'backend': 'codex', 'model': '', 'effort': ''}
    config.update(overrides)
    return service.save_config(config)


def latest(service):
    return service.request('/baseline-checks/latest')


def wait_check(service, statuses, seconds=60, cleaned=False):
    """Waits for the latest check to reach one of `statuses` and returns it.

    With `cleaned`, it also waits for the clone's removal (or its recorded
    failure) and for the baseline slot to be free: the worker saves the
    cleaned record first and releases the slot as it exits, so a scenario
    that saves settings or starts another check right after must wait for
    baseline_active to clear.
    """
    def done():
        check = latest(service)['check']
        if not check or check['status'] not in statuses:
            return None
        if cleaned and not (check['workspace_removed'] or check['cleanup_error']):
            return None
        if cleaned and service.request('/state')['baseline_active']:
            return None
        return check
    return service.wait(done, f'baseline reaching {statuses}', seconds)


def descendants_gone(pattern, seconds=10):
    gone = poll(lambda: subprocess.run(['pgrep', '-f', pattern], capture_output=True).returncode != 0, seconds, interval=0.2)
    if not gone:
        raise AssertionError(f'descendant still running: {pattern}')


def scenario(mode):
    with fixture_service(f'octomus-baseline-{mode}-') as (root, service):
        marker = root / 'baseline-entered'
        if mode == 'gates':
            config = save_config(service, verification_commands=[f'touch {marker}; sleep 31338 & sleep 60'], command_timeout_seconds=60)
            task = {'id': 'task-seed', 'cycle_id': 'cycle-seed', 'proposal': {'id': 'p', 'title': 'T', 'problem': 'P', 'benefit': 'B', 'scope': 'S', 'evidence': [], 'category': 'features', 'target': 'main', 'tier': 'M', 'dependencies': [], 'prompt': 'Do it', 'decision': 'accepted', 'reason': 'R', 'problem_key': '', 'relevant_paths': [], 'reconsiders': []}, 'status': 'blocked', 'blocked_reason': 'publication_uncertain', 'route': {'backend': 'codex', 'model': 'm', 'effort': 'low'}, 'config': config['config'], 'source_revision': 's', 'comparison_base': 's', 'default_revision': 's', 'branch': 'octomus/seed', 'workspace': '', 'sessions': [], 'reviews': [], 'verification': [], 'output_commit': '0' * 40, 'attempts': 0, 'created_at': '2026-01-01T00:00:00Z', 'updated_at': '2026-01-01T00:00:00Z'}
            task.update(review_baseline=0, superseded_by=[], supersedes=[],
                        rediscovery_requested=False,
                        lifecycle={'archived_at': None, 'discarded_at': None})
            db = sqlite3.connect(root / '.octomus/state.db')
            db.execute("INSERT INTO records VALUES ('task',?,?)", (task['id'], json.dumps(task)))
            db.commit()
            db.close()
            code, check = service.expect('/baseline-checks', 'POST', {'expected_revision': config['revision']})
            assert code == 202 and check['status'] == 'running', check
            check_id = check['id']
            service.wait(lambda: marker.exists(), 'command entry')
            assert service.expect('/baseline-checks', 'POST', {'expected_revision': config['revision']})[0] == 409
            stale = '0' * 64
            assert service.expect('/baseline-checks', 'POST', {'expected_revision': stale})[0] == 409
            for path in ['/control/cycle', '/control/resume', '/control/audit', f'/tasks/{task["id"]}/reconcile']:
                assert service.expect(path, 'POST')[0] == 409, path
            assert service.expect('/config', 'PUT', {'expected_revision': config['revision'], 'config': {}})[0] == 409
            assert service.expect('/control/pause', 'POST')[0] == 200
            assert service.expect('/tasks')[0] == 200
            state = service.request('/state')
            assert state['baseline_active'] and state['baseline']['id'] == check_id and 'commands' not in state['baseline'], state['baseline']
            assert 'config_matches' in state['baseline'] and 'revision_status' in state['baseline']
            assert state['baseline']['config_revision'] == config['revision'], state['baseline']
            code, ack = service.expect(f'/baseline-checks/{check_id}/cancel', 'POST')
            assert code == 200 and ack['ok'], ack
            check = wait_check(service, ['cancelled'], cleaned=True)
            descendants_gone('sleep 31338')
            assert check['commands'] and check['commands'][0]['success'] is False, check['commands']
            assert check['workspace_removed']
            assert service.expect(f'/baseline-checks/{check_id}/cancel', 'POST')[0] == 409
            print('PASS gates: deterministic hold, every execution gate conflicts, cancel kills descendants')
            return
        if mode == 'pass':
            config = save_config(service, empty_models=True, verification_commands=[
                "test \"$(cat README.md)\" = 'Feature contract: feature.txt must contain fixed.'",
                'test ! -e untracked.txt',
                'test ! -e scratchpad.tmp',
                'echo done'])
            view = latest(service)
            assert view['check'] is None and view['eligible'], view
            assert latest(service)['check'] is None and not service.request('/state')['baseline_active']
            (root / 'checkout/README.md').write_text('dirty local edits\n')
            (root / 'checkout/untracked.txt').write_text('junk\n')
            (root / 'checkout/scratchpad.tmp').write_text('junk\n')
            code, check = service.expect('/baseline-checks', 'POST', {'expected_revision': config['revision']})
            assert code == 202 and check['status'] == 'running', check
            check = wait_check(service, ['passed'], cleaned=True)
            assert len(check['commands']) == 4 and all(c['success'] for c in check['commands']), check
            assert check['revision'] and check['workspace_removed'] and check['completed_at'], check
            assert not (root / '.octomus/baselines' / check['id']).exists()
            assert not service.request('/state')['baseline_active']
            view = service.wait(lambda: latest(service) if latest(service)['revision_status'] == 'matches_last_observation' else None, 'default branch observation')
            assert view['config_matches'] and view['default_observation']['revision'] == check['revision'], view
            assert view['config_revision'] == check['config_fingerprint'] == config['revision'], view
            changed = save_config(service, empty_models=True, verification_commands=['echo extra'])
            view = latest(service)
            assert view['config_matches'] is False and view['check']['id'] == check['id'], view
            code, second = service.expect('/baseline-checks', 'POST', {'expected_revision': changed['revision']})
            assert code == 202, second
            second = wait_check(service, ['passed'])
            assert second['id'] != check['id'] and second['commands'][0]['success']
            report = usage_report(root)
            assert report['admissions'] == [] and report['cycles'] == [], report
            assert report['tasks'] == []
            assert not (root / 'publications.jsonl').exists()
            print('PASS pass: empty model routes, remote-clone content asserted, stale config detected')
            return
        if mode == 'failure':
            config = save_config(service, verification_commands=['true', 'false', 'echo third'])
            code, check = service.expect('/baseline-checks', 'POST', {'expected_revision': config['revision']})
            assert code == 202, check
            check = wait_check(service, ['failed'], cleaned=True)
            assert len(check['commands']) == 3 and [c['success'] for c in check['commands']] == [True, False, True], check['commands']
            assert 'exit status: 1' in check['commands'][1]['output'], check['commands'][1]
            assert check['workspace_removed']
            print('PASS failure: ordinary nonzero commands are recorded and verification continues')
            return
        raise AssertionError(f'unknown baseline scenario {mode}')


SCENARIOS = [(mode, functools.partial(scenario, mode)) for mode in ['gates', 'pass', 'failure']]


if __name__ == '__main__':
    run_selected('baseline', SCENARIOS, sys.argv[1:])
    if not sys.argv[1:]:
        print('All baseline scenarios passed')
