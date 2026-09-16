#!/usr/bin/env python3
import http.client
import json
import os
from pathlib import Path
import shutil
import signal
import sqlite3
import subprocess
import tempfile
import time

from e2e import TOKEN, Service, base_config, poll, setup, usage_report


def status(service, path, method='GET', value=None):
    return service.expect(path, method, value)


def save_config(service, empty_models=False, **overrides):
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
    def done():
        check = latest(service)['check']
        if not check or check['status'] not in statuses:
            return None
        return check if not cleaned or check['workspace_removed'] or check['cleanup_error'] else None
    return service.wait(done, f'baseline reaching {statuses}', seconds)


def descendants_gone(pattern, seconds=10):
    gone = poll(lambda: subprocess.run(['pgrep', '-f', pattern], capture_output=True).returncode != 0, seconds, interval=0.2)
    if not gone:
        raise AssertionError(f'descendant still running: {pattern}')


def scenario(mode):
    with tempfile.TemporaryDirectory(prefix=f'octomus-baseline-{mode}-') as tmp:
        root = Path(tmp)
        setup(root)
        service = Service(root)
        try:
            service.start()
            marker = root / 'baseline-entered'
            if mode == 'gates':
                config = save_config(service, verification_commands=[f'touch {marker}; sleep 31338 & sleep 60'], command_timeout_seconds=60)
                task = {'id': 'task-seed', 'cycle_id': 'cycle-seed', 'proposal': {'id': 'p', 'title': 'T', 'problem': 'P', 'benefit': 'B', 'scope': 'S', 'evidence': [], 'category': 'features', 'target': 'main', 'tier': 'M', 'dependencies': [], 'prompt': 'Do it', 'decision': 'accepted', 'reason': 'R', 'problem_key': '', 'relevant_paths': [], 'reconsiders': []}, 'status': 'blocked', 'blocked_reason': 'publication_uncertain', 'route': {'backend': 'codex', 'model': 'm', 'effort': 'low'}, 'config': config, 'source_revision': 's', 'comparison_base': 's', 'default_revision': 's', 'branch': 'octomus/seed', 'workspace': '', 'sessions': [], 'reviews': [], 'verification': [], 'output_commit': '0' * 40, 'attempts': 0, 'created_at': '2026-01-01T00:00:00Z', 'updated_at': '2026-01-01T00:00:00Z'}
                db = sqlite3.connect(root / '.octomus/state.db')
                db.execute("INSERT INTO records VALUES ('task',?,?)", (task['id'], json.dumps(task)))
                db.commit()
                db.close()
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202 and check['status'] == 'running', check
                check_id = check['id']
                service.wait(lambda: marker.exists(), 'command entry')
                assert status(service, '/baseline-checks', 'POST', {'expected_config': config})[0] == 409
                stale = dict(config, verification_commands=['false'])
                assert status(service, '/baseline-checks', 'POST', {'expected_config': stale})[0] == 409
                for path in ['/control/cycle', '/control/resume', '/control/audit', f'/tasks/{task["id"]}/reconcile']:
                    assert status(service, path, 'POST')[0] == 409, path
                assert status(service, '/config', 'PUT', config)[0] == 409
                assert status(service, '/control/pause', 'POST')[0] == 200
                assert status(service, '/tasks')[0] == 200
                state = service.request('/state')
                assert state['baseline_active'] and state['baseline']['id'] == check_id and 'commands' not in state['baseline'], state['baseline']
                assert 'config_matches' in state['baseline'] and 'revision_status' in state['baseline']
                code, ack = status(service, f'/baseline-checks/{check_id}/cancel', 'POST')
                assert code == 200 and ack['ok'], ack
                check = wait_check(service, ['cancelled'], cleaned=True)
                descendants_gone('sleep 31338')
                assert check['commands'] and check['commands'][0]['success'] is False, check['commands']
                assert check['workspace_removed']
                assert status(service, f'/baseline-checks/{check_id}/cancel', 'POST')[0] == 409
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
                time.sleep(2.5)
                assert latest(service)['check'] is None and not service.request('/state')['baseline_active']
                (root / 'checkout/README.md').write_text('dirty local edits\n')
                (root / 'checkout/untracked.txt').write_text('junk\n')
                (root / 'checkout/scratchpad.tmp').write_text('junk\n')
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202 and check['status'] == 'running', check
                check = wait_check(service, ['passed'], cleaned=True)
                assert len(check['commands']) == 4 and all(c['success'] for c in check['commands']), check
                assert check['revision'] and check['workspace_removed'] and check['completed_at'], check
                assert not (root / '.octomus/baselines' / check['id']).exists()
                assert not service.request('/state')['baseline_active']
                view = service.wait(lambda: latest(service) if latest(service)['revision_status'] == 'matches_last_observation' else None, 'default branch observation')
                assert view['config_matches'] and view['default_observation']['revision'] == check['revision'], view
                changed = save_config(service, empty_models=True, verification_commands=['echo extra'])
                view = latest(service)
                assert view['config_matches'] is False and view['check']['id'] == check['id'], view
                code, second = status(service, '/baseline-checks', 'POST', {'expected_config': changed})
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
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202, check
                check = wait_check(service, ['failed'], cleaned=True)
                assert len(check['commands']) == 3 and [c['success'] for c in check['commands']] == [True, False, True], check['commands']
                assert 'exit status: 1' in check['commands'][1]['output'], check['commands'][1]
                assert check['workspace_removed']
                print('PASS failure: ordinary nonzero commands are recorded and verification continues')
                return
            if mode == 'mutation':
                config = save_config(service, verification_commands=['echo external >> README.md', 'true'])
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202, check
                check = wait_check(service, ['failed'], cleaned=True)
                assert len(check['commands']) == 1 and 'changed' in check['commands'][0]['output'], check
                assert 'changed' in check['error'] and check['workspace_removed']
                config = save_config(service, verification_commands=['git -c user.name=External -c user.email=external@example.com commit --allow-empty -m moved', 'true'])
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202, check
                check = wait_check(service, ['failed'], cleaned=True)
                assert len(check['commands']) == 1 and 'changed' in check['commands'][0]['output'], check
                print('PASS mutation: worktree edits and HEAD movement both stop verification')
                return
            if mode == 'timeout':
                config = save_config(service, verification_commands=[f'touch {marker}; sleep 31337 & sleep 60'], command_timeout_seconds=10)
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202, check
                service.wait(lambda: marker.exists(), 'command entry')
                check = wait_check(service, ['timed_out'], cleaned=True)
                descendants_gone('sleep 31337')
                assert check['commands'] and check['commands'][0]['success'] is False
                assert check['workspace_removed'], check
                print('PASS timeout: command deadline ends the check and kills descendants')
                return
            if mode == 'timeout-overall':
                config = save_config(service, verification_commands=[f'touch {marker}; sleep 60'], session_timeout_seconds=10, task_timeout_seconds=10, command_timeout_seconds=60)
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202, check
                service.wait(lambda: marker.exists(), 'command entry')
                check = wait_check(service, ['timed_out'], cleaned=True)
                assert 'overall limit' in check['error'], check['error']
                assert check['workspace_removed']
                print('PASS timeout-overall: the task budget caps the whole check')
                return
            if mode == 'restart':
                pgid_file = root / 'baseline-pgid'
                config = save_config(service, verification_commands=[f'touch {marker}; echo $$ > {pgid_file}; sleep 31339 & sleep 60'], command_timeout_seconds=60)
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202, check
                service.wait(lambda: pgid_file.exists() and (root / '.octomus/baselines' / check['id'] / 'workspace').exists(), 'command entry and clone')
                pgid = int(pgid_file.read_text().strip())
                service.stop(crash=True)
                try:
                    os.killpg(pgid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                descendants_gone('sleep 31339')
                service.start()
                recovered = wait_check(service, ['interrupted'])
                check = service.wait(lambda: latest(service)['check'] if latest(service)['check']['workspace_removed'] else None, 'baseline cleanup retry')
                assert not (root / '.octomus/baselines' / recovered['id']).exists()
                print('PASS restart: a crashed check recovers interrupted and cleans its clone')
                return
            if mode == 'cancel-restart':
                config = save_config(service, verification_commands=[f'touch {marker}; sleep 60'], command_timeout_seconds=60)
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202, check
                service.wait(lambda: marker.exists(), 'command entry')
                assert status(service, f'/baseline-checks/{check["id"]}/cancel', 'POST')[0] == 200
                service.stop(crash=True)
                service.start()
                check = wait_check(service, ['cancelled'])
                print('PASS cancel-restart: durable cancel intent survives the crash')
                return
            if mode == 'shutdown':
                config = save_config(service, verification_commands=[f'touch {marker}; sleep 60'], command_timeout_seconds=60)
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202, check
                service.wait(lambda: marker.exists(), 'command entry')
                service.stop()
                db = sqlite3.connect(root / '.octomus/state.db')
                record = json.loads(db.execute("SELECT data FROM records WHERE kind='baseline' AND id=?", (check['id'],)).fetchone()[0])
                db.close()
                assert record['status'] == 'interrupted', record['status']
                service.start()
                check = service.wait(lambda: latest(service)['check'] if latest(service)['check']['workspace_removed'] else None, 'baseline cleanup retry')
                print('PASS shutdown: graceful stop drains the worker into an interrupted record')
                return
            if mode == 'symlink':
                config = save_config(service, verification_commands=[f'touch {marker}; sleep 60'], command_timeout_seconds=60)
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202, check
                service.wait(lambda: marker.exists(), 'command entry')
                service.stop(crash=True)
                outside = root / 'outside'
                outside.mkdir()
                (outside / 'keep.txt').write_text('preserve\n')
                target = root / '.octomus/baselines' / check['id']
                assert target.is_dir() and not target.is_symlink() and target.parent == root / '.octomus/baselines'
                shutil.rmtree(target)
                target.symlink_to(outside)
                service.start()
                recovered = wait_check(service, ['interrupted'])
                refused = service.wait(lambda: latest(service)['check'] if latest(service)['check']['cleanup_error'] else None, 'symlink refusal')
                assert not refused['workspace_removed'] and (outside / 'keep.txt').exists()
                assert recovered['id'] == refused['id']
                print('PASS symlink: cleanup refuses traversal and preserves the outside directory')
                return
            if mode == 'disconnect':
                config = save_config(service)
                connection = http.client.HTTPConnection('127.0.0.1', service.port)
                connection.request('POST', '/api/baseline-checks', body=json.dumps({'expected_config': config}), headers={'Authorization': f'Bearer {TOKEN}', 'Content-Type': 'application/json'})
                time.sleep(0.3)
                connection.close()
                check = wait_check(service, ['passed'])
                assert check['commands'][0]['success'], check
                print('PASS disconnect: an aborted request still leaves a completing worker')
                return
            if mode == 'storage':
                config = save_config(service, max_workspace_bytes=1_000_000)
                (root / '.octomus/junk.bin').write_bytes(b'0' * 2_000_000)
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202, check
                check = wait_check(service, ['failed'])
                assert 'storage' in check['error'].lower() or 'limit' in check['error'].lower(), check['error']
                assert not check['commands']
                print('PASS storage: the workspace limit fails the check before any work')
                return
            if mode == 'truncation':
                commands = ["yes '𐐀' | head -c 20001 || true", "yes 'x' | head -c 300000 || true"] + ["head -c 20000 /dev/zero | tr '\\0' y"] * 80
                config = save_config(service, verification_commands=commands)
                code, check = status(service, '/baseline-checks', 'POST', {'expected_config': config})
                assert code == 202, check
                check = wait_check(service, ['passed'], seconds=120)
                assert len(check['commands']) == len(commands), len(check['commands'])
                assert all(c['output_truncated'] for c in check['commands'][:2]), check['commands'][:2]
                assert all(len(c['output'].encode()) <= 16 * 1024 for c in check['commands'])
                total = sum(len(c['output'].encode()) for c in check['commands'])
                assert total <= 1024 * 1024, total
                print('PASS truncation: per-command UTF-8 bounds and the aggregate cap hold')
                return
            raise AssertionError(f'unknown baseline scenario {mode}')
        finally:
            service.stop()
            service.log.close()


if __name__ == '__main__':
    for mode in ['gates', 'pass', 'failure', 'mutation', 'timeout', 'timeout-overall', 'restart', 'cancel-restart', 'shutdown', 'symlink', 'disconnect', 'storage', 'truncation']:
        scenario(mode)
    print('All baseline scenarios passed')
