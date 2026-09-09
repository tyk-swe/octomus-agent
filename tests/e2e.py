#!/usr/bin/env python3
"""Runs the actual service, scheduler, SQLite, and Git against deterministic external peers.
No network writes, real Codex turns, credentials, or spending. Run after cargo build + web build.
"""
import contextlib
import http.server
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

PROJECT = Path(__file__).resolve().parents[1]
BINARY = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(PROJECT / 'target/debug/octomus-agent')))
TOKEN = 'fixture-operator-token-with-at-least-32-characters'
GUIDANCE = 'Fixture guidance: prefer minimal, well-verified changes.'


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
        # A local webhook receiver: every configured notification lands here as parsed JSON.
        self.notifications = []
        service = self

        class Receiver(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
                assert self.headers.get('Content-Type') == 'application/json' and TOKEN.encode() not in body
                service.notifications.append(json.loads(body))
                self.send_response(204)
                self.end_headers()

            def log_message(self, *args):
                pass

        self.receiver = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Receiver)
        threading.Thread(target=self.receiver.serve_forever, daemon=True).start()
        self.notification_url = f'http://127.0.0.1:{self.receiver.server_address[1]}/hook'

    def notification(self, event, task_id=None):
        return next((n for n in self.notifications if n['event'] == event and (task_id is None or n['detail']['task_id'] == task_id)), None)

    def close(self):
        self.stop()
        self.log.close()
        self.receiver.shutdown()
        self.receiver.server_close()

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
        config.update(repository=str(self.root / 'checkout'), github_repo='fixture/project', cycle_interval_seconds=3600, verification_commands=['for file in feature*.txt; do test "$(cat "$file")" = fixed || exit 1; done'], session_timeout_seconds=30, task_timeout_seconds=120, command_timeout_seconds=10, operator_guidance=GUIDANCE, notification_url=self.notification_url)
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
    pr = {'number': 42, 'title': 'An existing improvement', 'body': 'Existing context.\n<!-- octomus:task:earlier -->', 'head': {'ref': 'octomus/existing', 'sha': head, 'repo': {'full_name': 'fixture/project'}}, 'base': {'ref': 'main'}, 'html_url': 'https://github.com/fixture/project/pull/42', 'state': 'open', 'merged_at': None, 'additions': 2000, 'deletions': 0, 'created_at': '2026-08-01T00:00:00Z'}
    if (root / 'existing-pr-feedback').exists():
        # A maintainer asked for changes, one check is red and the branch conflicts with main.
        pr.update(mergeable=False, mergeable_state='dirty')
        (root / 'feedback.json').write_text(json.dumps({'42': {
            'reviews': [{'user': {'login': 'maintainer'}, 'state': 'COMMENTED'}, {'user': {'login': 'maintainer'}, 'state': 'CHANGES_REQUESTED'}],
            'review_comments': [{'user': {'login': 'maintainer'}, 'created_at': '2026-08-02T00:00:00Z', 'path': 'earlier.txt', 'body': 'Please also cover feature.txt. Token ghp_abcdefghijklmnop must not leak.'}],
            'issue_comments': [{'user': {'login': 'maintainer'}, 'created_at': '2026-08-03T00:00:00Z', 'body': 'CI is red on this branch.'}],
            'check_runs': [{'name': 'unit', 'status': 'completed', 'conclusion': 'failure'}, {'name': 'lint', 'status': 'completed', 'conclusion': 'success'}]}}))
    (root / 'prs.json').write_text(json.dumps([pr]))


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
        if mode != 'normal':
            (root / mode).touch()
        if mode in ['existing-pr', 'existing-pr-feedback', 'remote-conflict', 'dependencies']:
            existing_pr(root)
        if mode == 'closed-after-publication':
            (root / 'interrupt-publication').touch()
        service = Service(root)
        try:
            service.start()
            service.configure()
            if mode == 'failed-start':
                service.wait(lambda: (s := service.request('/state'))['cycles'] and s['cycles'][0]['status'] == 'failed', 'failed cycle start')
                failed = service.wait(lambda: service.notification('cycle_failed'), 'cycle failure notification')
                assert 'Fixture failed start' in failed['detail']['error'] and failed['repository'] == 'fixture/project'
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
            if mode == 'failed-executor-start':
                assert task['status'] == 'blocked' and task['execution_session'] is None, task
                assert 'Fixture failed start' in task['error'], task['error']
                report = usage_report(root)
                assert sum(a['role'] == 'executor' for a in report['admissions']) == 1
                assert not (root / 'publications.jsonl').exists()
                service.request('/control/pause', 'POST')
                service.wait(lambda: service.request('/state')['active_tasks'] == 0, 'failed executor stopped')
                (root / mode).unlink()
                service.request(f'/tasks/{task["id"]}/retry', 'POST')
                service.request('/control/resume', 'POST')
                task = service.wait(service.terminal_task, 'executor initialization retry')
                assert task['status'] == 'published', task['error']
                report = usage_report(root)
                assert sum(a['role'] == 'executor' for a in report['admissions']) == 2, report
                assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
                print('PASS failed-executor-start: retry initialization reserves exactly one new admission')
                return
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
            if mode in ['malformed-review', 'incomplete-review', 'remote-conflict', 'failed-verification', 'interactive', 'main-conflict']:
                assert task['status'] == 'blocked', task
                assert not (root / 'publications.jsonl').exists(), 'Unresolved work must not publish'
                if mode == 'main-conflict':
                    # The rebase conflicted: nothing rebased, the workspace is intact and main is untouched.
                    external = (root / 'external-revision').read_text()
                    assert 'conflicted' in task['error'] and external in task['error'], task['error']
                    assert task['output_commit'] is None and task['reconciliations'] == [] and task['reviews'] == []
                    assert git('rev-parse', 'HEAD', cwd=task['workspace']) != task['source_revision']
                    assert git('status', '--porcelain', cwd=task['workspace']) == '' and not (Path(task['workspace']) / '.git/rebase-merge').exists()
                    assert git('rev-parse', 'main', cwd=root / 'remote.git') == external
                    assert not any(p['prompt'].startswith('Perform a fresh code review') for p in map(json.loads, (root / 'protocol.jsonl').read_text().splitlines()))
                blocked = service.wait(lambda: service.notification('task_blocked', task['id']), 'blocked notification')
                assert blocked['detail']['error'] == task['error']
                assert service.notification('task_published') is None
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
            published = service.wait(lambda: service.notification('task_published', task['id']), 'publication notification')
            assert published['detail']['pr_url'] == task['pr_url'] and published['detail']['title'] == task['proposal']['title']
            assert {n['event'] for n in service.notifications} <= {'task_published', 'task_blocked'}
            reviews = 4 if mode == 'main-moved-late' else 3
            assert len(task['reviews']) == reviews, task['reviews']
            assert len({r['session_id'] for r in task['reviews']}) == reviews
            if mode in ['main-moved', 'main-moved-late']:
                external = (root / 'external-revision').read_text()
                stage = 'pre_review' if mode == 'main-moved' else 'pre_publication'
                assert [(r['stage'], r['to']) for r in task['reconciliations']] == [(stage, external)], task['reconciliations']
                assert task['default_revision'] == external and task['source_revision'] == external and task['comparison_base'] == external
                rebased = task['reviews'] if mode == 'main-moved' else task['reviews'][3:]
                assert all(r['comparison_base'] == external for r in rebased)
                assert all(r['comparison_base'] == task['reconciliations'][0]['from'] for r in task['reviews'][:0 if mode == 'main-moved' else 3])
                assert git('merge-base', '--is-ancestor', external, task['output_commit'], cwd=root / 'remote.git') == ''
                assert (Path(task['workspace']) / 'external.txt').read_text() == 'external\n'
                assert any(e['kind'] == 'reconciliation' for e in service.request(f'/events?entity={task["id"]}'))
            else:
                assert task['reconciliations'] == []
                assert all(r['comparison_base'] == task['default_revision'] for r in task['reviews'])
            repairs = [s for s in task['sessions'] if s['role'] == 'repair']
            assert len(repairs) == 1 and repairs[0]['route'] == task['config']['repair_route']
            assert task['workspace'].endswith(f'tasks/{task["execution_session"]}/workspace')
            assert task['verification'][-1]['success']
            assert task['verification'][-1]['revision'] == task['output_commit']
            assert len(json.loads((root / 'prs.json').read_text())) == (2 if mode == 'parallel' else 1)
            if mode in ['existing-pr', 'existing-pr-feedback', 'dependencies']:
                assert task['pr_number'] == 42 and task['branch'] == 'octomus/existing'
                assert (Path(task['workspace']) / 'earlier.txt').exists()
                assert json.loads((root / 'publications.jsonl').read_text().splitlines()[0])['action'] == 'edit'
                grounded = service.request('/state')['cycles'][-1]['grounding']
                pr = grounded['prs'][0]
                if mode == 'existing-pr-feedback':
                    assert (pr['review_decision'], pr['ci'], pr['failing_checks'], pr['mergeable']) == ('changes_requested', 'failure', ['unit'], 'conflicts'), pr
                    assert [c['author'] for c in pr['comments']] == ['maintainer', 'maintainer'] and pr['comments'][0]['path'] == 'earlier.txt'
                    assert 'ghp_abc' not in json.dumps(pr['comments']) and '[redacted]' in pr['comments'][0]['body']
                    assert grounded['feedback_targets'] == ['octomus/existing']
                    assert service.request('/state')['prs'][0]['review_decision'] == 'changes_requested'
                else:
                    assert (pr['review_decision'], pr['ci'], pr['mergeable'], pr['comments']) == ('none', 'none', 'unknown', [])
                    assert grounded['feedback_targets'] == []
                # Feedback is fetched for owned PRs only, once per grounding.
                requests = (root / 'feedback-requests.jsonl').read_text().splitlines()
                assert sum('/reviews' in r for r in requests) == 1 and sum('/check-runs' in r for r in requests) == 1
            assert len((root / 'publications.jsonl').read_text().splitlines()) == (2 if mode in ['parallel', 'dependencies'] else 1)
            assert git('rev-parse', 'main', cwd=root / 'remote.git') == task['default_revision'], 'Default branch must never be pushed'
            protocol = [json.loads(line) for line in (root / 'protocol.jsonl').read_text().splitlines()]
            assert len([p for p in protocol if p['prompt'].startswith('Discover worthwhile')]) == 9
            expected_feedback = '["octomus/existing"]' if mode == 'existing-pr-feedback' else '[]'
            assert all(f'Feedback targets (owned PRs with requested changes, failing checks or merge conflicts): {expected_feedback}.' in p['prompt'] for p in protocol if p['prompt'].startswith('Discover worthwhile'))
            assert f'resolves recorded feedback on {expected_feedback}' in next(p['prompt'] for p in protocol if p['prompt'].startswith('Act as final'))
            assert len([p for p in protocol if p['prompt'].startswith('Adversarial proposal')]) == 2
            assert len({p['thread'] for p in protocol if p['prompt'].startswith('Repair actionable')}) == (2 if mode in ['parallel', 'dependencies'] else 1)
            assert all(p['sandbox'] == {'type': 'dangerFullAccess'} and p['approval'] == 'never' for p in protocol)
            # Operator guidance reaches every planning role as authoritative policy, but never the workers directly.
            planning = [p['prompt'] for p in protocol if p['prompt'].startswith(('Ground this repository', 'Discover worthwhile', 'Adversarial proposal', 'Act as final orchestrator'))]
            assert len(planning) == 13 and all('Operator guidance (authoritative operator policy' in p and GUIDANCE in p for p in planning)
            assert not any(GUIDANCE in p['prompt'] for p in protocol if p['prompt'].startswith(('Implement this accepted task', 'Perform a fresh code review', 'Repair actionable')))
            report = usage_report(root)
            expected_tasks = 2 if mode in ['parallel', 'dependencies'] else 1
            extra_review = 1 if mode == 'main-moved-late' else 0
            assert len(report['admissions']) == 13 + 6 * expected_tasks + extra_review
            assert sum(a['role'] == 'repair' for a in report['admissions']) == 2 * expected_tasks
            assert report['cycles'][0]['planning_admissions'] == 13
            assert report['cycles'][0]['task_admissions'] == 6 * expected_tasks + extra_review
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
            service.close()


def missing_session_scenario(role):
    import sqlite3
    with tempfile.TemporaryDirectory(prefix=f'octomus-missing-{role}-') as tmp:
        root = Path(tmp)
        setup(root)
        marker = root / ('interactive' if role == 'executor' else 'interactive-repair')
        marker.touch()
        service = Service(root)
        try:
            service.start()
            service.configure()
            task = service.wait(service.terminal_task, f'{role} interrupted')
            assert task['status'] == 'blocked' and 'interactive input' in task['error'], task
            service.request('/control/pause', 'POST')
            service.wait(lambda: service.request('/state')['active_tasks'] == 0, 'paused task')
            service.stop()
            thread = task['execution_session' if role == 'executor' else 'repair_session']
            assert thread and any(s['id'] == thread for s in task['sessions'])
            task['sessions'] = [s for s in task['sessions'] if s['id'] != thread]
            # Corrupt only this stopped, temporary fixture's saved task snapshot.
            with sqlite3.connect(root / '.octomus/state.db') as db:
                db.execute("UPDATE records SET data=? WHERE kind='task' AND id=?", (json.dumps(task), task['id']))
            marker.unlink()
            service.start()
            service.request(f'/tasks/{task["id"]}/retry', 'POST')
            service.request('/control/resume', 'POST')
            task = service.wait(service.terminal_task, 'missing session blocked')
            assert task['status'] == 'blocked', task
            assert f'missing its {role} session record' in task['error'], task['error']
            assert thread in task['error'] and task['id'] in task['error']
            service.wait(lambda: service.request('/state')['active_tasks'] == 0, 'runtime task released')
            assert Path(task['workspace']).is_dir()
            assert not (root / 'publications.jsonl').exists()
            print(f'PASS missing-{role}-session: blocked with context, runtime released, no publication')
        finally:
            service.close()


def audit_scenario(mode):
    import sqlite3
    with tempfile.TemporaryDirectory(prefix='octomus-audit-') as tmp:
        root = Path(tmp)
        setup(root)
        service = Service(root)
        try:
            service.start()
            queued_before = []
            if mode == 'queued':
                service.configure()
                service.wait(service.terminal_task, 'initial fixture delivery')
                service.request('/control/pause', 'POST')
                service.stop()
                with sqlite3.connect(root / '.octomus/state.db') as db:
                    identity, raw = db.execute("SELECT id,data FROM records WHERE kind='task'").fetchone()
                    task = json.loads(raw)
                    task['status'] = 'queued'
                    task['proposal']['title'] = 'Earlier queued work'
                    db.execute("UPDATE records SET data=? WHERE kind='task' AND id=?", (json.dumps(task), identity))
                service.start()
                queued_before = service.request('/state')['tasks']
            c = service.request('/config')
            c.update(repository=str(root / 'checkout'), github_repo='fixture/project', verification_commands=[], command_timeout_seconds=10, session_timeout_seconds=30, task_timeout_seconds=120, notification_url=service.notification_url)
            for role in ['orchestrator', 'discovery', 'proposal_reviewer']:
                c['roles'][role] = {'model': 'gpt-6-astra', 'effort': 'medium'}
            c['roles']['code_reviewer'] = {'model': 'unavailable', 'effort': 'high'}
            c['repair_route'] = {'model': 'unavailable', 'effort': 'high'}
            if mode == 'budget':
                c['max_sessions_per_day'] = 2
            service.request('/config', 'PUT', c)
            assert service.request('/doctor?mode=audit', 'POST')['mode'] == 'audit'
            try:
                service.request('/doctor', 'POST')
                raise AssertionError('Execution doctor accepted missing verification')
            except urllib.error.HTTPError as e:
                assert e.code == 400
            marker = {'idle': 'idle', 'malformed': 'audit-malformed', 'failed': 'failed-start'}.get(mode, 'audit-decisions')
            (root / marker).touch()
            if mode not in ['failed']:
                (root / 'audit-hold').touch()
            publications = (root / 'publications.jsonl').read_bytes() if (root / 'publications.jsonl').exists() else b''
            baseline_revision = git('rev-parse', 'main', cwd=root / 'remote.git')
            baseline_refs = git('for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads', cwd=root / 'remote.git')
            service.request('/control/audit', 'POST')
            if mode != 'failed':
                service.wait(lambda: (root / 'audit-entered').exists(), 'audit started')
                state = service.request('/state')
                assert state['status'] == 'auditing' and state['control']['paused']
                for action in ['audit', 'resume', 'cycle']:
                    try:
                        service.request('/control/' + action, 'POST')
                        raise AssertionError('Conflicting control accepted')
                    except urllib.error.HTTPError as e:
                        assert e.code == 409
                if mode == 'interrupted':
                    service.stop(crash=True)
                (root / 'audit-hold').unlink()
                if mode == 'interrupted':
                    service.start()
            expected = 'interrupted' if mode == 'interrupted' else 'failed' if mode in ['budget', 'malformed', 'failed'] else 'idle' if mode == 'idle' else 'completed'
            def completed_audit():
                state = service.request('/state')
                return state if state['cycles'] and not state['cycle_active'] and state['cycles'][0]['status'] == expected else None
            state = service.wait(completed_audit, 'audit completion')
            cycle = state['cycles'][0]
            assert cycle['mode'] == 'audit' and state['control']['paused']
            assert state['tasks'] == queued_before
            if mode in ['accepted', 'queued']:
                assert {p['decision'] for p in cycle['proposals']} == {'accepted', 'rejected', 'deferred'}
                assert all(p['reason'] for p in cycle['proposals']) and len(cycle['assessments']) == 2
                completed = service.wait(lambda: service.notification('audit_completed'), 'audit notification')
                assert completed['detail']['cycle_id'] == cycle['id'] and (completed['detail']['accepted'], completed['detail']['rejected'], completed['detail']['deferred']) == (1, 1, 1)
            if mode in ['budget', 'malformed', 'failed']:
                assert service.wait(lambda: service.notification('audit_failed'), 'audit failure notification')['detail']['error'] == cycle['error']
            report = usage_report(root)
            row = next(c for c in report['cycles'] if c['id'] == cycle['id'])
            assert row['mode'] == 'audit' and row['task_admissions'] == 0
            if mode in ['accepted', 'idle', 'queued']:
                assert row['planning_admissions'] == 13
            service.stop()
            service.start()
            time.sleep(1.2)
            assert service.request('/state')['tasks'] == queued_before
            current_publications = (root / 'publications.jsonl').read_bytes() if (root / 'publications.jsonl').exists() else b''
            assert current_publications == publications
            assert git('rev-parse', 'main', cwd=root / 'remote.git') == baseline_revision
            assert git('for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads', cwd=root / 'remote.git') == baseline_refs
            if mode == 'accepted':
                service.stop()
                diagnostic = subprocess.run([str(BINARY), '--data-dir', str(root / '.octomus'), '--doctor', '--audit'], env=service.env, capture_output=True, text=True, check=True)
                assert json.loads(diagnostic.stdout)['mode'] == 'audit'
            print(f'PASS audit-{mode}: durable decisions, paused queue, no publication')
        finally:
            service.close()


if __name__ == '__main__':
    import sys
    modes = ['normal', 'custom-route', 'interactive', 'failed-start', 'failed-executor-start', 'parallel', 'existing-pr', 'existing-pr-feedback', 'dependencies', 'malformed-review', 'incomplete-review', 'failed-verification', 'remote-conflict', 'main-moved', 'main-moved-late', 'main-conflict', 'idle', 'interrupt-publication', 'closed-after-publication']
    # Optional focused run while developing: python3 tests/e2e.py normal failed-verification audit-accepted
    selected = sys.argv[1:]
    for mode in [m for m in modes if not selected or m in selected]:
        scenario(mode)

    for role in ['executor', 'repair']:
        if not selected or f'missing-{role}' in selected:
            missing_session_scenario(role)

    for mode in ['accepted', 'idle', 'malformed', 'budget', 'failed', 'interrupted', 'queued']:
        if not selected or f'audit-{mode}' in selected:
            audit_scenario(mode)
