#!/usr/bin/env python3
"""Runs the actual service, scheduler, SQLite, and Git against deterministic external peers.
No network writes, real Codex turns, credentials, or spending. Run after cargo build + web build.
"""
import contextlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

PROJECT = Path(__file__).resolve().parents[1]
BINARY = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(PROJECT / 'target/debug/octomus-agent')))
TOKEN = 'fixture-operator-token-with-at-least-32-characters'


def git(*args, cwd):
    return subprocess.check_output(['/usr/bin/git', *args], cwd=cwd, stderr=subprocess.DEVNULL, text=True).strip()


class Service:
    def __init__(self, root):
        self.root = root
        self.process = None
        self.log = (root / 'service.log').open('a')
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            self.port = sock.getsockname()[1]
        self.env = {**os.environ, 'OCTOMUS_TOKEN': TOKEN, 'OCTOMUS_FIXTURE': str(root), 'PATH': f'{root / "bin"}:{os.environ["PATH"]}'}

    def start(self):
        self.process = subprocess.Popen([str(BINARY), '--data-dir', str(self.root / '.octomus'), '--listen', f'127.0.0.1:{self.port}', '--assets', str(PROJECT / 'web/build')], env=self.env, stdout=self.log, stderr=self.log)
        self.wait(lambda: self.request('/healthz', api=False), 'service startup')

    def stop(self, crash=False):
        if self.process and self.process.poll() is None:
            self.process.kill() if crash else self.process.terminate()
            self.process.wait(timeout=15)

    def request(self, path, method='GET', value=None, api=True):
        request = urllib.request.Request(f'http://127.0.0.1:{self.port}{"/api" if api else ""}{path}', method=method, headers={'Authorization': f'Bearer {TOKEN}', 'Content-Type': 'application/json'}, data=json.dumps(value or {}).encode() if method != 'GET' else None)
        with urllib.request.urlopen(request, timeout=5) as response:
            return json.load(response)

    def wait(self, predicate, label, seconds=45):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            try:
                result = predicate()
                if result:
                    return result
            except (OSError, urllib.error.URLError):
                pass
            if self.process and self.process.poll() is not None:
                raise AssertionError(f'{label}: service exited\n{(self.root / "service.log").read_text()}')
            time.sleep(0.1)
        state = self.request('/state')
        raise AssertionError(f'{label} timed out: {json.dumps(state, indent=2)}')

    def configure(self):
        config = self.request('/config')
        config.update(repository=str(self.root / 'checkout'), github_repo='fixture/project', cycle_interval_seconds=3600, verification_commands=['for file in feature*.txt; do test "$(cat "$file")" = fixed || exit 1; done'], session_timeout_seconds=30, task_timeout_seconds=120, command_timeout_seconds=10)
        if (self.root / 'failed-verification').exists():
            config['verification_commands'] = ['false']
        for role in config['roles']:
            config['roles'][role] = {'model': 'gpt-6-astra', 'effort': 'medium'}
        if (self.root / 'custom-route').exists():
            config['repair_route'] = {'model': 'gpt-5.6-luna', 'effort': 'high'}
            config['tiers']['M'] = {'model': 'gpt-5.6-luna', 'effort': 'low'}
        self.request('/config', 'PUT', config)
        diagnostic = self.request('/doctor', 'POST')
        assert diagnostic['codex_version'] == 'codex-cli 0.153.4'
        assert diagnostic['tested_codex_version'] == '0.153.4' and diagnostic['warnings'] == []
        (self.root / 'version').write_text('0.0.0-fixture')
        diagnostic = self.request('/doctor', 'POST')
        assert 'mismatch' in diagnostic['message'] and len(diagnostic['warnings']) == 1
        (self.root / 'version').unlink()
        self.request('/control/cycle', 'POST')

    def terminal_task(self):
        state = self.request('/state')
        assert not state['control']['error'], state['control']['error']
        tasks = state['tasks']
        return self.request(f'/tasks/{tasks[0]["id"]}') if tasks and tasks[0]['status'] in ['published', 'blocked', 'failed'] else None


def setup(root):
    (root / 'bin').mkdir()
    for name in ['codex', 'gh', 'git']:
        dest = root / 'bin' / name
        shutil.copy(PROJECT / 'tests/fixtures' / f'{name}.py', dest)
        dest.chmod(0o755)
    (root / 'checkout').mkdir()
    git('init', '--bare', str(root / 'remote.git'), cwd=root)
    git('init', '-b', 'main', cwd=root / 'checkout')
    git('config', 'user.name', 'Fixture', cwd=root / 'checkout')
    git('config', 'user.email', 'fixture@example.com', cwd=root / 'checkout')
    (root / 'checkout/README.md').write_text('Feature contract: feature.txt must contain fixed.\n')
    git('add', '.', cwd=root / 'checkout')
    git('commit', '-m', 'Initial fixture', cwd=root / 'checkout')
    git('remote', 'add', 'origin', str(root / 'remote.git'), cwd=root / 'checkout')
    git('push', '-u', 'origin', 'main', cwd=root / 'checkout')
    git('symbolic-ref', 'HEAD', 'refs/heads/main', cwd=root / 'remote.git')


def existing_pr(root):
    checkout = root / 'checkout'
    git('checkout', '-b', 'octomus/existing', cwd=checkout)
    (checkout / 'earlier.txt').write_text('Preserve the earlier improvement.\n')
    git('add', '.', cwd=checkout)
    git('commit', '-m', 'Earlier Octomus work', cwd=checkout)
    git('push', 'origin', 'octomus/existing', cwd=checkout)
    head = git('rev-parse', 'HEAD', cwd=checkout)
    git('checkout', 'main', cwd=checkout)
    (root / 'target').write_text('octomus/existing')
    (root / 'prs.json').write_text(json.dumps([{'number': 42, 'title': 'An existing improvement', 'body': 'Existing context.\n<!-- octomus:task:earlier -->', 'head': {'ref': 'octomus/existing', 'sha': head, 'repo': {'full_name': 'fixture/project'}}, 'base': {'ref': 'main'}, 'html_url': 'https://github.com/fixture/project/pull/42', 'state': 'open', 'merged_at': None, 'additions': 2000, 'deletions': 0, 'created_at': '2026-08-01T00:00:00Z'}]))


def usage_report(root):
    # Runs concurrently with the service lock, with no token or dashboard assets.
    report = json.loads(subprocess.check_output([str(BINARY), '--data-dir', str(root / '.octomus'), '--usage-report'], text=True))
    assert sum(d['admissions'] for d in report['daily']) == len(report['admissions'])
    assert all(d['unattributed_admissions'] == 0 for d in report['daily'])
    return report


def scenario(mode):
    with tempfile.TemporaryDirectory(prefix=f'octomus-{mode}-') as tmp:
        root = Path(tmp)
        setup(root)
        if mode in ['existing-pr', 'remote-conflict', 'dependencies']:
            existing_pr(root)
        if mode != 'normal':
            (root / mode).touch()
        if mode == 'closed-after-publication':
            (root / 'interrupt-publication').touch()
        service = Service(root)
        try:
            service.start()
            service.configure()
            if mode == 'failed-start':
                service.wait(lambda: (s := service.request('/state'))['cycles'] and s['cycles'][0]['status'] == 'failed', 'failed cycle start')
                report = usage_report(root)
                assert len(report['admissions']) == 1
                assert report['cycles'][0]['planning_admissions'] == 1
                assert report['cycles'][0]['recorded_completed_sessions'] == 0
                assert not (root / 'publications.jsonl').exists()
                print('PASS failed-start: admission retained without completed session')
                return
            if mode == 'idle':
                service.wait(lambda: (s := service.request('/state'))['cycles'] and s['cycles'][0]['status'] == 'idle', 'idle cycle')
                assert not service.request('/state')['tasks']
                report = usage_report(root)
                assert len(report['admissions']) == 13
                assert report['cycles'][0]['planning_admissions'] == 13
                assert report['tasks'] == []
                print('PASS idle: all discovery/review roles complete without creating work')
                return
            if mode in ['interrupt-publication', 'closed-after-publication']:
                service.wait(lambda: (root / 'publication-created').exists(), 'publication side effect')
                service.stop(crash=True)
                if mode == 'closed-after-publication':
                    prs = json.loads((root / 'prs.json').read_text())
                    prs[0]['state'] = 'closed'
                    (root / 'prs.json').write_text(json.dumps(prs))
                service.start()
                # The durable publishing checkpoint is recovered autonomously.
                # No Codex turn or operator retry should be needed.
                task = service.wait(service.terminal_task, 'recovered publication')
                assert task['status'] == 'published', task['error']
            task = service.wait(service.terminal_task, 'task completion')
            if mode in ['parallel', 'dependencies']:
                service.wait(lambda: len([t for t in service.request('/state')['tasks'] if t['status'] == 'published']) == 2, 'both tasks delivered')
                all_tasks = [service.request(f'/tasks/{t["id"]}') for t in service.request('/state')['tasks']]
                assert len({t['workspace'] for t in all_tasks}) == 2
                assert len({t['execution_session'] for t in all_tasks}) == 2
                if mode == 'dependencies':
                    followup = next(t for t in all_tasks if t['proposal']['dependencies'])
                    prerequisite = next(t for t in all_tasks if not t['proposal']['dependencies'])
                    assert followup['source_revision'] == prerequisite['output_commit']
                    assert (Path(followup['workspace']) / 'feature.txt').read_text().strip() == 'fixed'
            if mode in ['malformed-review', 'incomplete-review', 'remote-conflict', 'failed-verification', 'interactive']:
                assert task['status'] == 'blocked', task
                assert not (root / 'publications.jsonl').exists(), 'Unresolved work must not publish'
                if mode == 'interactive':
                    assert 'interactive input' in task['error']
                    # A retry retains the saved route despite an operator configuration change.
                    service.request('/control/pause', 'POST')
                    service.wait(lambda: service.request('/state')['active_tasks'] == 0, 'paused task')
                    config = service.request('/config')
                    config['repair_route'] = {'model': 'gpt-5.6-luna', 'effort': 'low'}
                    service.request('/config', 'PUT', config)
                    (root / 'interactive').unlink()
                    service.request(f'/tasks/{task["id"]}/retry', 'POST')
                    service.request('/control/resume', 'POST')
                    task = service.wait(service.terminal_task, 'retried delivery')
                    assert task['status'] == 'published', task['error']
                    assert task['config']['repair_route'] == {'model': 'gpt-6-astra', 'effort': 'medium'}
                    assert all(s['route'] == task['config']['repair_route'] for s in task['sessions'] if s['role'] == 'repair')
                    report = usage_report(root)
                    assert sum(a['role'] == 'executor' for a in report['admissions']) == 2
                    assert len(report['admissions']) == 20
                    print('PASS interactive: blocked promptly; retry retains routes and counts another admission')
                    return
                if mode == 'malformed-review':
                    assert 'Unparseable review' in task['error'], task['error']
                if mode == 'remote-conflict':
                    assert git('rev-parse', 'octomus/existing', cwd=root / 'remote.git') == (root / 'external-revision').read_text()
                print(f'PASS {mode}: blocked, never published, workspace retained')
                return
            assert task['status'] == 'published', task['error']
            assert len(task['reviews']) == 3, task['reviews']
            assert len({r['session_id'] for r in task['reviews']}) == 3
            assert all(r['comparison_base'] == task['default_revision'] for r in task['reviews'])
            repairs = [s for s in task['sessions'] if s['role'] == 'repair']
            assert len(repairs) == 1 and repairs[0]['route'] == task['config']['repair_route']
            assert task['workspace'].endswith(f'tasks/{task["execution_session"]}/workspace')
            assert task['verification'][-1]['success']
            assert task['verification'][-1]['revision'] == task['output_commit']
            assert len(json.loads((root / 'prs.json').read_text())) == (2 if mode == 'parallel' else 1)
            if mode in ['existing-pr', 'dependencies']:
                assert task['pr_number'] == 42 and task['branch'] == 'octomus/existing'
                assert (Path(task['workspace']) / 'earlier.txt').exists()
                assert json.loads((root / 'publications.jsonl').read_text().splitlines()[0])['action'] == 'edit'
            assert len((root / 'publications.jsonl').read_text().splitlines()) == (2 if mode in ['parallel', 'dependencies'] else 1)
            assert git('rev-parse', 'main', cwd=root / 'remote.git') == task['default_revision'], 'Default branch must never be pushed'
            protocol = [json.loads(line) for line in (root / 'protocol.jsonl').read_text().splitlines()]
            assert len([p for p in protocol if p['prompt'].startswith('Discover worthwhile')]) == 9
            assert len([p for p in protocol if p['prompt'].startswith('Adversarial proposal')]) == 2
            assert len({p['thread'] for p in protocol if p['prompt'].startswith('Repair actionable')}) == (2 if mode in ['parallel', 'dependencies'] else 1)
            assert all(p['sandbox'] == {'type': 'dangerFullAccess'} and p['approval'] == 'never' for p in protocol)
            report = usage_report(root)
            expected_tasks = 2 if mode in ['parallel', 'dependencies'] else 1
            assert len(report['admissions']) == 13 + 6 * expected_tasks
            assert sum(a['role'] == 'repair' for a in report['admissions']) == 2 * expected_tasks
            assert report['cycles'][0]['planning_admissions'] == 13
            assert report['cycles'][0]['task_admissions'] == 6 * expected_tasks
            if mode == 'custom-route':
                assert repairs[0]['route'] == {'model': 'gpt-5.6-luna', 'effort': 'high'}
                consolidation = next(p['prompt'] for p in protocol if p['prompt'].startswith('Act as final'))
                assert '"M":{"model":"gpt-5.6-luna","effort":"low"}' in consolidation
                assert 'XS luna xhigh' not in consolidation
            if mode == 'normal':
                service.stop()
                (root / 'version').write_text('0.0.0-fixture')
                diagnostic = subprocess.run([str(BINARY), '--data-dir', str(root / '.octomus'), '--doctor'], env=service.env, capture_output=True, text=True, check=True)
                assert json.loads(diagnostic.stdout)['warnings']
                assert 'mismatch' in diagnostic.stderr
            print(f'PASS {mode}: complete reviewed delivery with no duplicate PRs')
        finally:
            service.stop()
            service.log.close()


if __name__ == '__main__':
    for mode in ['normal', 'custom-route', 'interactive', 'failed-start', 'parallel', 'existing-pr', 'dependencies', 'malformed-review', 'incomplete-review', 'failed-verification', 'remote-conflict', 'idle', 'interrupt-publication', 'closed-after-publication']:
        scenario(mode)
