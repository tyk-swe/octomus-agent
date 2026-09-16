#!/usr/bin/env python3
"""Mixed-runner behavior with synthetic peers, SQLite and real local Git only."""
import json
import os
from pathlib import Path
import shutil
import signal
import sqlite3
import tempfile

from e2e import Service, base_config, setup, usage_report, git


def route(backend, planning=False, provider='fixture', variant='high'):
    if backend == 'codex':
        return {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
    return {'backend': 'opencode', 'provider': provider, 'model': 'plain-model' if planning else 'fixture-model', 'effort': '', **({'variant': variant} if variant and not planning else {})}


def configuration(service, planning='opencode', executor='opencode', reviewer='opencode', repair='opencode'):
    c = base_config(service, ['test "$(cat feature.txt)" = fixed'], task_timeout_seconds=120)
    for role in ['orchestrator', 'discovery', 'proposal_reviewer']:
        c['roles'][role] = route(planning, planning=True)
    c['roles']['code_reviewer'] = route(reviewer)
    c['tiers'] = {tier: route(executor) for tier in c['tiers']}
    c['repair_route'] = route(repair, provider='alternate', variant=None)
    if {planning, executor, reviewer, repair} == {'opencode'}:
        c['codex_binary'] = '/codex-is-not-installed'
    service.request('/config', 'PUT', c)
    return c


def stop_peers(root):
    path = root / 'opencode-pids.jsonl'
    pids = [json.loads(row)['pid'] for row in path.read_text().splitlines()] if path.exists() else []
    child = root / 'opencode-child-pid'
    if child.exists():
        pids.append(int(child.read_text()))
    for pid in pids:
        try:
            args = Path(f'/proc/{pid}/cmdline').read_bytes()
            if str(root).encode() in args and os.getpgid(pid) == pid:
                os.killpg(pid, signal.SIGKILL)
        except (FileNotFoundError, ProcessLookupError):
            pass


def successful_workflow(mode):
    with tempfile.TemporaryDirectory(prefix='octomus-runners-') as directory:
        root = Path(directory)
        setup(root)
        service = Service(root)
        try:
            service.start()
            # Preview a draft executable path without saving configuration or admitting turns.
            preview = root / 'bin/opencode-preview'
            shutil.copy(root / 'bin/opencode', preview)
            models = service.request('/model-catalog', 'POST', {'backend': 'opencode', 'binary': str(preview)})
            assert any(m['available'] for m in models)
            assert not any(word in json.dumps(models) for word in ['fixture-credential', 'another-fixture-secret', 'PRIVATE_API_KEY'])
            assert service.request('/config')['opencode_binary'] == 'opencode'
            assert service.request('/state')['sessions_today'] == 0 and not (root / 'protocol.jsonl').exists()
            c = configuration(service, **({'planning': 'codex', 'reviewer': 'codex'} if mode == 'mixed' else {'executor': 'codex', 'repair': 'codex'} if mode == 'reverse-mixed' else {}))
            diagnostic = service.request('/doctor', 'POST')
            assert {d['backend'] for d in diagnostic['backends']} == ({'opencode'} if mode in ['opencode', 'recovery'] else {'codex', 'opencode'})
            assert not (root / 'protocol.jsonl').exists()
            if mode == 'recovery':
                (root / 'opencode-mode').write_text('hold')
            service.request('/control/cycle', 'POST')
            original = None
            if mode == 'recovery':
                service.wait(lambda: (root / 'opencode-child-pid').exists(), 'executor entered')
                row = service.request('/state')['tasks'][0]
                original = service.request(f'/tasks/{row["id"]}')
                service.stop(crash=True)
                stop_peers(root)  # Model the documented supervisor cgroup cleanup before restart.
                (root / 'opencode-mode').unlink()
                service.start()
            task = service.wait(service.terminal_task, f'{mode} delivery')
            assert task['status'] == 'published', task['error']
            assert task['route'] == c['tiers']['M']
            assert task['workspace'].endswith(f'tasks/{task["id"]}/workspace')
            assert len(task['reviews']) == 3 and len({r['session_id'] for r in task['reviews']}) == 3
            assert all(r['comparison_base'] == task['comparison_base'] for r in task['reviews'])
            repairs = [s for s in task['sessions'] if s['role'] == 'repair']
            assert len(repairs) == 1 and repairs[0]['route'] == c['repair_route']
            assert task['verification'][-1]['success'] and task['verification'][-1]['revision'] == task['output_commit']
            assert git('rev-parse', 'main', cwd=root / 'remote.git') == task['default_revision']
            assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
            calls = [json.loads(line) for line in (root / 'protocol.jsonl').read_text().splitlines()]
            assert len({p['thread'] for p in calls if p['prompt'].startswith('Repair actionable')}) == 1
            if original:
                assert task['execution_session'] == original['execution_session'] and task['workspace'] == original['workspace']
                assert task['attempts'] == 1
            report = usage_report(root)
            assert len(report['admissions']) == (20 if original else 19)
            assert any(a['route']['backend'] == 'opencode' for a in report['admissions'])
            assert report['tasks'][0]['repair_route'] == c['repair_route']
            print(f'PASS {mode}: exact routes, fresh reviews, persistent repairs and verified delivery')
        finally:
            service.stop()
            stop_peers(root)
            service.log.close()


def failed_review(mode):
    with tempfile.TemporaryDirectory(prefix='octomus-runner-failure-') as directory:
        root = Path(directory)
        setup(root)
        service = Service(root)
        try:
            service.start()
            c = configuration(service)
            c['tiers'] = {tier: route('opencode', planning=True) for tier in c['tiers']}
            service.request('/config', 'PUT', c)
            (root / 'opencode-mode').write_text(mode)
            service.request('/control/cycle', 'POST')
            task = service.wait(service.terminal_task, f'{mode} blocked review')
            assert task['status'] == 'blocked', task
            assert task['error'] and not task['reviews'] and not task['verification']
            assert Path(task['workspace']).is_dir() and not (root / 'publications.jsonl').exists()
            if mode == 'interactive':
                service.request('/control/pause', 'POST')
                service.wait(lambda: service.request('/state')['active_tasks'] == 0, 'paused task')
                new_config = service.request('/config')
                new_config['repair_route'] = route('codex')
                new_config['roles']['code_reviewer'] = route('codex')
                new_config['codex_binary'] = 'codex'
                service.request('/config', 'PUT', new_config)
                (root / 'opencode-mode').unlink()
                service.request(f'/tasks/{task["id"]}/retry', 'POST')
                service.request('/control/resume', 'POST')
                task = service.wait(service.terminal_task, 'retry keeps saved backend')
                assert task['status'] == 'published', task['error']
                assert task['config']['repair_route'] == c['repair_route']
                assert all(s['route']['backend'] == 'opencode' for s in task['sessions'])
            print(f'PASS OpenCode {mode}: failed review cannot authorize publication')
        finally:
            service.stop()
            stop_peers(root)
            service.log.close()


def audit():
    with tempfile.TemporaryDirectory(prefix='octomus-runner-audit-') as directory:
        root = Path(directory)
        setup(root)
        service = Service(root)
        try:
            service.start()
            c = configuration(service)
            c['verification_commands'] = []
            c['roles']['code_reviewer'] = route('codex')
            c['tiers'] = {tier: route('codex') for tier in c['tiers']}
            c['repair_route'] = route('codex')
            service.request('/config', 'PUT', c)
            assert service.request('/doctor?mode=audit', 'POST')['backends'][0]['backend'] == 'opencode'
            service.request('/control/audit', 'POST')
            state = service.wait(lambda: (state := service.request('/state'))['cycles'] and not state['cycle_active'] and state, 'OpenCode audit')
            assert state['cycles'][0]['status'] == 'completed' and state['control']['paused'] and not state['tasks']
            assert not (root / 'publications.jsonl').exists()
            assert len(usage_report(root)['admissions']) == 13
            print('PASS OpenCode audit: unavailable execution runners do not block planning')
        finally:
            service.stop()
            stop_peers(root)
            service.log.close()


def legacy_recovery():
    with tempfile.TemporaryDirectory(prefix='octomus-legacy-runner-') as directory:
        root = Path(directory)
        setup(root)
        (root / 'interactive').touch()
        service = Service(root)
        try:
            service.start()
            service.configure()
            task = service.wait(service.terminal_task, 'legacy interrupted executor')
            service.request('/control/pause', 'POST')
            service.wait(lambda: service.request('/state')['active_tasks'] == 0, 'paused legacy task')
            service.stop()
            old_path = root / '.octomus/tasks' / task['execution_session'] / 'workspace'
            Path(task['workspace']).parent.rename(old_path.parent)
            task['workspace'] = str(old_path)

            def legacy(value):
                if isinstance(value, dict):
                    return {k: legacy(v) for k, v in value.items() if k not in ['backend', 'provider', 'variant', 'opencode_binary']}
                if isinstance(value, list):
                    return [legacy(v) for v in value]
                return value

            with sqlite3.connect(root / '.octomus/state.db') as db:
                db.execute("UPDATE records SET data=? WHERE kind='task' AND id=?", (json.dumps(task), task['id']))
                for kind, identity, raw in db.execute('SELECT kind,id,data FROM records').fetchall():
                    db.execute('UPDATE records SET data=? WHERE kind=? AND id=?', (json.dumps(legacy(json.loads(raw))), kind, identity))
                for identity, raw in db.execute('SELECT id,data FROM admissions').fetchall():
                    db.execute('UPDATE admissions SET data=? WHERE id=?', (json.dumps(legacy(json.loads(raw))), identity))
            (root / 'interactive').unlink()
            service.start()
            service.request(f'/tasks/{task["id"]}/retry', 'POST')
            service.request('/control/resume', 'POST')
            recovered = service.wait(service.terminal_task, 'legacy task delivery')
            assert recovered['status'] == 'published', recovered['error']
            assert recovered['workspace'] == str(old_path) and recovered['execution_session'] == task['execution_session']
            assert all(a['route']['backend'] == 'codex' for a in usage_report(root)['admissions'])
            print('PASS legacy recovery: existing folders, sessions, snapshots and usage remain readable')
        finally:
            service.stop()
            service.log.close()


def task_deadline():
    with tempfile.TemporaryDirectory(prefix='octomus-task-deadline-') as directory:
        root = Path(directory)
        setup(root)
        service = Service(root)
        try:
            service.start()
            c = configuration(service)
            c.update(session_timeout_seconds=10, task_timeout_seconds=10)
            service.request('/config', 'PUT', c)
            (root / 'opencode-mode').write_text('detached-hold')
            service.request('/control/cycle', 'POST')
            service.wait(lambda: (root / 'opencode-child-pid').exists(), 'detached shell started')
            task = service.wait(service.terminal_task, 'task deadline cleanup', seconds=20)
            assert task['status'] == 'blocked' and task['error'] == 'Task time limit exceeded', task
            pid = int((root / 'opencode-child-pid').read_text())
            stat = Path(f'/proc/{pid}/stat')
            assert not stat.exists() or ') Z' in stat.read_text(), 'Detached shell survived the task deadline'
            assert (root / 'opencode-aborts.jsonl').exists()
            assert not (root / 'publications.jsonl').exists()
            print('PASS task deadline: abort finishes before server cleanup; no detached shell or publication')
        finally:
            service.stop()
            stop_peers(root)
            service.log.close()


if __name__ == '__main__':
    for mode in ['opencode', 'mixed', 'reverse-mixed', 'recovery']:
        successful_workflow(mode)
    for mode in ['wrong-model', 'wrong-variant', 'missing-structured', 'malformed-structured', 'incomplete', 'interactive']:
        failed_review(mode)
    audit()
    legacy_recovery()
    task_deadline()
