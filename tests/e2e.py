#!/usr/bin/env python3
"""Runs the actual service, scheduler, SQLite, and Git against deterministic external peers.
No network writes, real Codex turns, credentials, or spending. Run after make build (dashboard + Go binary) or set OCTOMUS_TEST_BINARY.
Every e2e suite accepts scenario names (`python3 tests/e2e.py normal audit-idle`) to run only those;
an unknown name lists them all.
"""
import contextlib
import functools
import http.server
import io
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

PROJECT = Path(__file__).resolve().parents[1]
BINARY = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(PROJECT / 'bin/octomus-agent')))
TOKEN = 'fixture-operator-token-with-at-least-32-characters'
# The Go race detector's default exit status (GORACE exitcode). Only a
# race-instrumented build (make test-race-e2e) exits with it; the service
# itself uses 0, 1 and 2.
RACE_EXIT_STATUS = 66


def git(*args, cwd):
    return subprocess.check_output(['/usr/bin/git', *args], cwd=cwd, stderr=subprocess.DEVNULL, text=True).strip()


def poll(predicate, seconds, interval=0.1, tick=None):
    """Runs `predicate` until it returns a truthy value or `seconds` elapse.

    Returns that value, or None on timeout. A predicate that cannot connect is retried,
    because the service may still be starting; `tick` runs once per iteration and may
    raise to abort the wait early.
    """
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            result = predicate()
            if result:
                return result
        except (OSError, urllib.error.URLError):
            pass
        if tick:
            tick()
        time.sleep(interval)
    return None


def service_log(root, tail=None):
    """Returns root/service.log for failure messages, only its last `tail` lines when given."""
    try:
        text = (root / 'service.log').read_text(errors='replace')
    except OSError as error:
        return f'<service.log unavailable: {error!r}>'
    return text if tail is None else '\n'.join(text.splitlines()[-tail:])


def run_selected(suite, scenarios, names):
    """Runs `names`, or every scenario when it is empty, in registry order.

    `scenarios` lists (name, zero-argument callable) pairs; unknown names are
    refused before anything runs.
    """
    registry = dict(scenarios)
    assert len(registry) == len(scenarios), f'duplicate {suite} scenario names'
    unknown = [name for name in names if name not in registry]
    if unknown:
        raise SystemExit(f'unknown {suite} scenarios: {", ".join(unknown)}; available: {", ".join(registry)}')
    for name, run in registry.items():
        if not names or name in names:
            print(f'RUN {suite} {name}', flush=True)
            run()


def process_gone(pid):
    """Whether `pid` has exited: reaped, or a zombie its parent has not reaped yet.

    The state is the first field after the last ')', because the command name
    before it is arbitrary text and may itself contain ') Z'.
    """
    try:
        stat = Path(f'/proc/{pid}/stat').read_text()
    except (FileNotFoundError, ProcessLookupError):  # Reaped before or while reading.
        return True
    return stat.rpartition(')')[2].split()[0] in ['Z', 'X']


def base_config(service, commands, **overrides):
    """Loads the saved display configuration every scenario starts from.

    Callers add their own routes, flags and overrides, then PUT it themselves.
    """
    config = service.request('/config')['config']
    config.update(repository=str(service.root / 'checkout'), github_repo='fixture/project', verification_commands=commands, session_timeout_seconds=30, command_timeout_seconds=10, **overrides)
    return config


class Service:
    def __init__(self, root):
        self.root = root
        self.process = None
        self.race_reported = False
        self.log = (root / 'service.log').open('a')
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            self.port = sock.getsockname()[1]
        self.env = {key: value for key, value in os.environ.items() if key != 'OCTOMUS_NOTIFICATION_WEBHOOK_URL'}
        self.env.update({'OCTOMUS_TOKEN': TOKEN, 'OCTOMUS_FIXTURE': str(root), 'PATH': f'{root / "bin"}:{os.environ["PATH"]}'})

    def start(self):
        self.process = subprocess.Popen([str(BINARY), '--data-dir', str(self.root / '.octomus'), '--listen', f'127.0.0.1:{self.port}', '--assets', str(PROJECT / 'web/build')], env=self.env, stdout=self.log, stderr=self.log)
        self.wait(lambda: self.request('/healthz', api=False), 'service startup')

    def stop(self, crash=False):
        if self.process and self.process.poll() is None:
            self.process.kill() if crash else self.process.terminate()
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                # Never leave the service running; the raised error chains any
                # scenario failure already in flight.
                self.process.kill()
                self.process.wait(timeout=5)
                raise AssertionError(f'service did not stop within 15s of {"SIGKILL" if crash else "SIGTERM"}; service.log tail:\n{service_log(self.root, tail=100)}')
        # A race detected at any point, shutdown included, fails the scenario
        # once, with the report from the log.
        if self.process and self.process.returncode == RACE_EXIT_STATUS and not self.race_reported:
            self.race_reported = True
            raise AssertionError(f'service exited with status {RACE_EXIT_STATUS}: the race detector reported a data race; service.log tail:\n{service_log(self.root, tail=200)}')

    def _open(self, path, method, value, api, timeout):
        """Sends one authenticated request; `timeout` bounds each socket operation,
        so a response the service holds longer than that raises TimeoutError."""
        request = urllib.request.Request(f'http://127.0.0.1:{self.port}{"/api" if api else ""}{path}', method=method, headers={'Authorization': f'Bearer {TOKEN}', 'Content-Type': 'application/json'}, data=json.dumps(value or {}).encode() if method != 'GET' else None)
        return urllib.request.urlopen(request, timeout=timeout)

    def request(self, path, method='GET', value=None, api=True, timeout=5):
        """One request that must succeed: returns the JSON body, raises HTTPError otherwise."""
        with self._open(path, method, value, api, timeout) as response:
            return json.load(response)

    def expect(self, path, method='GET', value=None, timeout=5):
        """One API request that may fail: returns (status, body) instead of raising."""
        try:
            with self._open(path, method, value, True, timeout) as response:
                return response.status, json.load(response)
        except urllib.error.HTTPError as error:
            return error.code, json.loads(error.read() or b'{}')

    def save_config(self, config):
        """Replaces the given top-level fields under the current canonical
        revision and returns the fresh settings view.

        `config` is sent as the `config` patch: callers that mutate a loaded
        display view send every field back, while precise callers may pass a
        partial map. Scenarios are serialized, so the revision read here is the
        one the caller loaded.
        """
        revision = self.request('/config')['revision']
        return self.request('/config', 'PUT', {'expected_revision': revision, 'config': config})

    def wait(self, predicate, label, seconds=45):
        last_error = None

        def attempt():
            # poll() retries these; remember the latest for the timeout report.
            nonlocal last_error
            try:
                return predicate()
            except urllib.error.HTTPError as error:
                try:
                    body = error.read()[:2000].decode(errors='replace')
                except Exception as read_error:  # A cut-off body raises IncompleteRead, not OSError.
                    body = f'<body unavailable: {read_error!r}>'
                last_error = f'HTTP {error.code}: {body}'
                raise
            except (OSError, urllib.error.URLError) as error:
                last_error = repr(error)
                raise

        def exited():
            if self.process and self.process.poll() is not None:
                raise AssertionError(f'{label}: service exited\n{service_log(self.root)}')
        result = poll(attempt, seconds, tick=exited)
        if result:
            return result
        try:
            state = json.dumps(self.request('/state'), indent=2)
        except Exception as error:  # Any failure (even BadStatusLine) is reported, never raised.
            state = f'<state unavailable: {error!r}>'
        raise AssertionError(f'{label} timed out after {seconds}s; last error: {last_error}\nstate: {state}\nservice.log tail:\n{service_log(self.root, tail=100)}')

    def configure(self):
        commands = ['false'] if (self.root / 'failed-verification').exists() else ['for file in feature*.txt; do test "$(cat "$file")" = fixed || exit 1; done']
        config = base_config(self, commands, cycle_interval_seconds=3600, task_timeout_seconds=120)
        for role in config['roles']:
            config['roles'][role] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
        # Shipped tiers and repair carry effort but no model, so the fixture picks one.
        for tier in config['tiers']:
            config['tiers'][tier] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
        config['repair_route'] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
        if (self.root / 'custom-route').exists():
            config['repair_route'] = {'backend': 'codex', 'model': 'gpt-5.6-luna', 'effort': 'high'}
            config['tiers']['M'] = {'model': 'gpt-5.6-luna', 'effort': 'low'}
        if (self.root / 'cap1-interrupt').exists():
            config['max_open_prs'] = 1
        self.save_config(config)
        diagnostic = self.request('/doctor', 'POST')
        view = self.request('/config')
        # The check is attributed to the canonical revision; fixture values are
        # never display-transformed, so the checked canonical payload matches.
        assert diagnostic['checked_revision'] == view['revision']
        assert diagnostic['checked_config'] == view['config']
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
    for name in ['codex', 'opencode', 'gh', 'git']:
        dest = root / 'bin' / name
        shutil.copy(PROJECT / 'tests/fixtures' / f'{name}.py', dest)
        dest.chmod(0o755)
    shutil.copy(PROJECT / 'tests/fixtures/worker.py', root / 'bin/worker.py')
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
    report = json.loads(subprocess.check_output([str(BINARY), '--data-dir', str(root / '.octomus'), '--usage-report'], text=True, timeout=30))
    assert sum(d['admissions'] for d in report['daily']) == len(report['admissions'])
    assert all(d['unattributed_admissions'] == 0 for d in report['daily'])
    return report


def scenario(mode):
    with tempfile.TemporaryDirectory(prefix=f'octomus-{mode}-') as tmp:
        root = Path(tmp)
        setup(root)
        if mode in ['existing-pr', 'remote-conflict', 'dependencies']:
            existing_pr(root)
        if mode == 'external-context':
            (root / 'prs.json').write_text(json.dumps([{'number': 77, 'title': 'External contribution', 'body': 'External work.\n', 'head': {'ref': 'external-work', 'sha': 'e' * 40, 'repo': {'full_name': 'contributor/project'}}, 'base': {'ref': 'main', 'repo': {'full_name': 'fixture/project'}}, 'html_url': 'https://github.com/fixture/project/pull/77', 'state': 'open', 'merged_at': None, 'additions': 4, 'deletions': 1, 'created_at': '2026-08-02T00:00:00Z'}]))
        if mode != 'normal':
            (root / mode).touch()
        if mode in ['closed-after-publication', 'cap1-interrupt']:
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
            if mode == 'failed-discovery':
                def failed_cycle():
                    state = service.request('/state')
                    return state if state['cycles'] and not state['cycle_active'] and state['cycles'][0]['status'] == 'failed' else None
                state = service.wait(failed_cycle, 'failed discovery cycle')
                cycle = service.request('/cycles/' + state['cycles'][0]['id'])
                assert 'invalid JSON' in cycle['error'], cycle['error']
                # Every started role leaves terminal evidence, including the ones after the failure.
                assert sorted(s['role'] for s in cycle['sessions']) == sorted(['grounding'] + [f'discovery-{i}' for i in range(9)]), cycle['sessions']
                failed = [s for s in cycle['sessions'] if s['status'] == 'failed']
                assert [s['role'] for s in failed] == ['discovery-0'] and 'invalid JSON' in failed[0]['summary'], failed
                assert all(s['status'] == 'completed' and s['summary'] for s in cycle['sessions'] if s['role'] != 'discovery-0')
                assert not cycle['proposals'] and not state['tasks']
                report = usage_report(root)
                assert report['cycles'][0]['planning_admissions'] == 10
                assert report['cycles'][0]['recorded_completed_sessions'] == 9
                assert not (root / 'publications.jsonl').exists()
                print('PASS failed-discovery: partial planning failure records every role outcome and queues nothing')
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
            if mode in ['interrupt-publication', 'closed-after-publication', 'cap1-interrupt']:
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
            if mode in ['malformed-review', 'incomplete-review', 'remote-conflict', 'failed-verification', 'interactive']:
                assert task['status'] == 'blocked', task
                assert not (root / 'publications.jsonl').exists(), 'Unresolved work must not publish'
                if mode == 'interactive':
                    assert 'interactive input' in task['error']
                    # A retry retains the saved route despite an operator configuration change.
                    service.request('/control/pause', 'POST')
                    service.wait(lambda: service.request('/state')['active_tasks'] == 0, 'paused task')
                    config = service.request('/config')['config']
                    config['repair_route'] = {'model': 'gpt-5.6-luna', 'effort': 'low'}
                    service.save_config(config)
                    (root / 'interactive').unlink()
                    service.request(f'/tasks/{task["id"]}/retry', 'POST')
                    service.request('/control/resume', 'POST')
                    task = service.wait(service.terminal_task, 'retried delivery')
                    assert task['status'] == 'published', task['error']
                    assert task['config']['repair_route'] == {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
                    assert all(s['route'] == task['config']['repair_route'] for s in task['sessions'] if s['role'] == 'repair')
                    report = usage_report(root)
                    assert sum(a['role'] == 'executor' for a in report['admissions']) == 2
                    assert len(report['admissions']) == 20
                    print('PASS interactive: blocked promptly; retry retains routes and counts another admission')
                    return
                if mode == 'malformed-review':
                    assert 'invalid' in task['error'] and 'JSON' in task['error'], task['error']
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
            assert task['workspace'].endswith(f'tasks/{task["id"]}/workspace')
            assert task['verification'][-1]['success']
            assert task['verification'][-1]['revision'] == task['output_commit']
            assert len(json.loads((root / 'prs.json').read_text())) == (2 if mode in ['parallel', 'external-context'] else 1)
            if mode in ['existing-pr', 'dependencies']:
                assert task['pr_number'] == 42 and task['branch'] == 'octomus/existing'
                assert (Path(task['workspace']) / 'earlier.txt').exists()
                assert json.loads((root / 'publications.jsonl').read_text().splitlines()[0])['action'] == 'comment'
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
            if mode == 'external-context':
                cycle_id = service.request('/state')['cycles'][0]['id']
                grounding = service.request(f'/cycles/{cycle_id}')['grounding']
                coverage = grounding['pr_coverage']
                assert coverage['complete'] and coverage['total_external'] == 1 and coverage['included_external'] == 1, coverage
                external = grounding['external_prs']
                assert [p['number'] for p in external] == [77]
                assert external[0]['head_repository'] == 'contributor/project' and external[0]['head'] == 'e' * 40
                reviewers = [p['prompt'] for p in protocol if p['prompt'].startswith('Adversarial proposal')]
                assert len(reviewers) == 2
                assert all('pull/77' in p and 'contributor/project' in p for p in reviewers)
                ground_prompt = next(p['prompt'] for p in protocol if p['prompt'].startswith('Ground this repository'))
                assert 'pull/77' in ground_prompt
                api_calls = [json.loads(line)['route'] for line in (root / 'gh-api.jsonl').read_text().splitlines()]
                assert any('state=open' in call for call in api_calls)
                assert not any(call.endswith('/pulls/77') for call in api_calls), api_calls
                assert all(t['branch'] != 'external-work' for t in service.request('/state')['tasks'])
            if mode == 'custom-route':
                assert repairs[0]['route'] == {'backend': 'codex', 'model': 'gpt-5.6-luna', 'effort': 'high'}
                consolidation = next(p['prompt'] for p in protocol if p['prompt'].startswith('Act as final'))
                assert '"M":{"backend":"codex","model":"gpt-5.6-luna","effort":"low"}' in consolidation
                assert 'XS luna xhigh' not in consolidation
            if mode == 'normal':
                service.stop()
                (root / 'version').write_text('0.0.0-fixture')
                diagnostic = subprocess.run([str(BINARY), '--data-dir', str(root / '.octomus'), '--doctor'], env=service.env, capture_output=True, text=True, check=True, timeout=60)
                assert json.loads(diagnostic.stdout)['warnings']
                assert 'mismatch' in diagnostic.stderr
            print(f'PASS {mode}: complete reviewed delivery with no duplicate PRs')
        finally:
            service.stop()
            service.log.close()


def settings_scenario():
    """The settings contract keeps canonical state distinct from the display view.

    A secret-bearing command is served redacted with transform metadata, the
    canonical revision identifies the saved configuration, a partial update
    preserves every untouched field (including hidden ones), stale revisions
    conflict without persisting, and baseline admission validates the revision
    instead of an echoed display object.
    """
    with tempfile.TemporaryDirectory(prefix='octomus-settings-') as tmp:
        root = Path(tmp)
        setup(root)
        service = Service(root)
        try:
            service.start()
            view = service.request('/config')
            assert set(view) == {'config', 'revision', 'transformed_fields'}, view
            assert view['transformed_fields'] == [] and len(view['revision']) == 64, view

            config = base_config(service, [])
            for role in config['roles']:
                config['roles'][role] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
            for tier in config['tiers']:
                config['tiers'][tier] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
            config['repair_route'] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
            # The service token is a registered secret: a command embedding it is
            # served redacted, while the stored canonical value keeps the token.
            # Its side effect is the proof the canonical command, not the preview,
            # is what the service holds and runs.
            proof = root / 'canonical-proof'
            secret_command = f'echo {TOKEN} > {proof}'
            displayed = f'echo [redacted] > {proof}'
            config['verification_commands'] = [secret_command]
            saved = service.save_config(config)
            entry = next(t for t in saved['transformed_fields'] if t['field'] == 'verification_commands')
            assert entry['kinds'] == ['redacted'] and entry['paths'] == [['verification_commands', 0]], saved['transformed_fields']
            assert saved['config']['verification_commands'] == [displayed]
            assert saved['revision'] != view['revision'] and len(saved['revision']) == 64

            # A partial patch omits the hidden field; the canonical token survives.
            retries = saved['config']['max_retries'] + 1
            code, updated = service.expect('/config', 'PUT', {'expected_revision': saved['revision'], 'config': {'max_retries': retries}})
            assert code == 200, updated
            assert updated['config']['verification_commands'] == [displayed]
            assert updated['config']['max_retries'] == retries and updated['revision'] != saved['revision']
            assert next(t for t in updated['transformed_fields'] if t['field'] == 'verification_commands')
            # API responses redact even canonical echoes; the revision is the
            # authoritative identity of the checked configuration.
            diagnostic = service.request('/doctor', 'POST')
            assert diagnostic['checked_revision'] == updated['revision']
            assert diagnostic['checked_config']['verification_commands'] == [displayed]
            assert not proof.exists()

            # A stale revision rejects writes and baseline starts before any work exists.
            code, refusal = service.expect('/config', 'PUT', {'expected_revision': saved['revision'], 'config': {'max_retries': retries + 1}})
            assert code == 409 and 'changed' in refusal['error'], (code, refusal)
            code, refusal = service.expect('/baseline-checks', 'POST', {'expected_revision': saved['revision']})
            assert code == 409, (code, refusal)
            assert service.request('/baseline-checks/latest')['check'] is None
            assert not (root / '.octomus/baselines').exists()

            # The admitted check snapshots the canonical configuration, not the preview.
            code, check = service.expect('/baseline-checks', 'POST', {'expected_revision': updated['revision']})
            assert code == 202, (code, check)
            assert check['config']['verification_commands'] == [displayed]
            assert check['config_fingerprint'] == updated['revision']
            finished = service.wait(lambda: (c := service.request('/baseline-checks/latest')['check']) and c['status'] != 'running' and c, 'baseline completion')
            assert finished['status'] == 'passed' and finished['commands'][0]['command'] == displayed, finished
            assert proof.read_text().strip() == TOKEN, 'the canonical secret-bearing command ran, not its display preview'
            print('PASS settings-view: canonical revision gates saves and baselines; hidden values stay canonical')
        finally:
            service.stop()
            service.log.close()


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
            service.stop()
            service.log.close()


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
                    task.update(branch=task['config']['branch_prefix'] + 'audit-queued',
                                workspace='', execution_session=None, repair_session=None,
                                sessions=[], reviews=[], verification=[], output_commit=None,
                                pr_number=None, pr_url=None, attempts=0, error=None,
                                blocked_reason=None, review_baseline=0, run_id=None)
                    db.execute("UPDATE records SET data=? WHERE kind='task' AND id=?", (json.dumps(task), identity))
                service.start()
                queued_before = service.request('/state')['tasks']
            c = base_config(service, [], task_timeout_seconds=120)
            for role in ['orchestrator', 'discovery', 'proposal_reviewer']:
                c['roles'][role] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
            c['roles']['code_reviewer'] = {'model': 'unavailable', 'effort': 'high'}
            c['repair_route'] = {'model': 'unavailable', 'effort': 'high'}
            if mode == 'budget':
                c['max_sessions_per_day'] = 2
            service.save_config(c)
            diagnostic = service.request('/doctor?mode=audit', 'POST')
            assert diagnostic['mode'] == 'audit'
            assert diagnostic['checked_revision'] == service.request('/config')['revision']
            assert diagnostic['checked_config'] == service.request('/config')['config']
            code, body = service.expect('/doctor', 'POST')
            assert code == 400, ('Execution doctor accepted missing verification', code, body)
            assert body['checked_revision'] == service.request('/config')['revision']
            assert body['checked_config'] == service.request('/config')['config']
            marker = {'idle': 'idle', 'malformed': 'audit-malformed', 'failed': 'failed-start'}.get(mode, 'audit-decisions')
            (root / marker).touch()
            if mode not in ['failed']:
                (root / 'audit-hold').touch()
            publications = (root / 'publications.jsonl').read_bytes() if (root / 'publications.jsonl').exists() else b''
            baseline_revision = git('rev-parse', 'main', cwd=root / 'remote.git')
            baseline_refs = git('for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads', cwd=root / 'remote.git')
            if mode == 'budget':
                code, body = service.expect('/control/audit', 'POST')
                assert code == 409, ('Unaffordable audit was accepted', code, body)
                message = body['error']
                assert '13' in message and 'increase' in message, message
                state = service.request('/state')
                capacity = state['planning_capacity']
                assert capacity['status'] == 'limit_too_low', capacity
                assert capacity['required'] == 13 and capacity['limit'] == 2, capacity
                assert state['cycles'] == [] and state['tasks'] == queued_before
                assert state['control']['paused'] and state['control']['error'] is None
                report = usage_report(root)
                assert report['admissions'] == [] and report['cycles'] == [] and report['daily'] == []
                assert not (root / 'publications.jsonl').exists()
                assert git('rev-parse', 'main', cwd=root / 'remote.git') == baseline_revision
                assert git('for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads', cwd=root / 'remote.git') == baseline_refs
                print('PASS audit-budget: refused before any admission with an explicit capacity reason')
                return
            for attempt in range(3):
                code, response = service.expect('/control/audit', 'POST')
                if code == 200:
                    break
                assert mode == 'queued' and code == 400 and response.get('error') == 'Control state changed during planning preflight', (mode, code, response)
                state = service.request('/state')
                assert state['tasks'] == queued_before and not state['cycle_active'], state
                time.sleep(0.2)
            assert code == 200, (mode, code, response)
            if mode != 'failed':
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
            cycle = service.request('/cycles/' + state['cycles'][0]['id'])
            assert cycle['mode'] == 'audit' and state['control']['paused']
            assert state['tasks'] == queued_before
            if mode in ['accepted', 'queued']:
                assert {p['decision'] for p in cycle['proposals']} == {'accepted', 'rejected', 'deferred'}
                assert all(p['reason'] for p in cycle['proposals']) and len(cycle['assessments']) == 2
            report = usage_report(root)
            row = next(c for c in report['cycles'] if c['id'] == cycle['id'])
            assert row['mode'] == 'audit' and row['task_admissions'] == 0
            if mode in ['accepted', 'idle', 'queued']:
                assert row['planning_admissions'] == 13
            service.stop()
            service.start()
            # Negative checks sleep past a scheduler tick (schedulerInterval,
            # 1 s, in internal/engine/engine.go) so a restart could act first.
            time.sleep(1.2)
            assert service.request('/state')['tasks'] == queued_before
            current_publications = (root / 'publications.jsonl').read_bytes() if (root / 'publications.jsonl').exists() else b''
            assert current_publications == publications
            assert git('rev-parse', 'main', cwd=root / 'remote.git') == baseline_revision
            assert git('for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads', cwd=root / 'remote.git') == baseline_refs
            if mode == 'accepted':
                service.stop()
                diagnostic = subprocess.run([str(BINARY), '--data-dir', str(root / '.octomus'), '--doctor', '--audit'], env=service.env, capture_output=True, text=True, check=True, timeout=60)
                assert json.loads(diagnostic.stdout)['mode'] == 'audit'
            print(f'PASS audit-{mode}: durable decisions, paused queue, no publication')
        finally:
            service.stop()
            service.log.close()


def harness_scenario():
    """The harness itself, against a scripted HTTP peer and plain child
    processes (no service binary).

    Error responses, even ones whose body is cut short, are retried until the
    predicate succeeds. A timeout raises one labelled report with the last
    error, the /state outcome (even when unreadable) and the service.log tail.
    Service.stop reports a race-detector exit status once. process_gone tells
    a live process from a zombie or a reaped one. run_selected runs scenarios
    by name.
    """
    calls = {}

    class Peer(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            calls[self.path] = calls.get(self.path, 0) + 1
            if self.path == '/api/state':
                self.wfile.write(b'not an HTTP status line\r\n\r\n')
            elif self.path == '/api/recovers' and calls[self.path] < 3:
                # The body stops short of its Content-Length, so reading it raises IncompleteRead.
                self.reply(500, b'{"error":', length=64)
            elif self.path == '/api/recovers':
                self.reply(200, b'{"ok":true}')
            else:
                self.reply(404, b'{"error":"Unknown API route"}')

        def reply(self, status, body, length=None):
            self.send_response(status)
            self.send_header('Content-Length', str(length or len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    with tempfile.TemporaryDirectory(prefix='octomus-harness-') as tmp:
        root = Path(tmp)
        (root / 'service.log').write_text('earlier line\nlast service line\n')
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Peer)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        service = Service(root)
        service.port = server.server_address[1]
        try:
            assert service.wait(lambda: service.request('/recovers'), 'recovering request', seconds=10) == {'ok': True}
            assert calls['/api/recovers'] == 3, calls
            report = None
            try:
                service.wait(lambda: service.request('/missing'), 'missing route', seconds=1)
            except AssertionError as error:
                report = str(error)
            assert report and report.startswith('missing route timed out after 1s; last error: HTTP 404: {"error":"Unknown API route"}\nstate: <state unavailable: BadStatusLine('), report
            assert report.endswith('service.log tail:\nearlier line\nlast service line'), report

            # A race-instrumented service exits with the race detector's status
            # even from a graceful SIGTERM shutdown; stop() reports it once.
            service.process = subprocess.Popen([sys.executable, '-c', 'import signal, sys, time\nsignal.signal(signal.SIGTERM, lambda *_: sys.exit(66))\nprint("ready", flush=True)\ntime.sleep(30)'], stdout=subprocess.PIPE, text=True)
            with service.process.stdout:
                assert service.process.stdout.readline() == 'ready\n'
            report = None
            try:
                service.stop()
            except AssertionError as error:
                report = str(error)
            assert report and report.startswith('service exited with status 66: the race detector reported a data race; service.log tail:\n'), report
            service.stop()
            # A clean exit, or a crash stop's SIGKILL, is not a race report.
            service.race_reported = False
            for command in ['pass', 'import time; time.sleep(30)']:
                service.process = subprocess.Popen([sys.executable, '-c', command])
                service.stop(crash=True)
                assert service.process.returncode in [0, -9], service.process.returncode
        finally:
            server.shutdown()
            server.server_close()
            service.log.close()

    # A live process whose command name mimics a zombie's stat line.
    child = subprocess.Popen([sys.executable, '-c', "from pathlib import Path; import time; Path('/proc/self/comm').write_text('x) Z 0'); print('ready', flush=True); time.sleep(30)"], stdout=subprocess.PIPE, text=True)
    try:
        with child.stdout:
            assert child.stdout.readline() == 'ready\n'
        assert ') Z 0)' in Path(f'/proc/{child.pid}/stat').read_text()
        assert not process_gone(child.pid)
        child.kill()
        assert poll(lambda: process_gone(child.pid), 5), 'killed child never became a zombie'
        assert Path(f'/proc/{child.pid}').exists(), 'the zombie was reaped early'
    finally:
        child.kill()
        child.wait(timeout=5)
    assert process_gone(child.pid)

    # Selected scenarios run in registry order; an unknown name runs nothing.
    ran = []
    registry = [(name, functools.partial(ran.append, name)) for name in ['a', 'b', 'c']]
    output = io.StringIO()
    with contextlib.redirect_stdout(output):
        run_selected('selftest', registry, ['c', 'a'])
        run_selected('selftest', registry, [])
        try:
            run_selected('selftest', registry, ['b', 'nope'])
            raise AssertionError('an unknown scenario name was accepted')
        except SystemExit as error:
            refusal = str(error)
    assert ran == ['a', 'c', 'a', 'b', 'c'], ran
    assert output.getvalue() == ''.join(f'RUN selftest {name}\n' for name in ran), output.getvalue()
    assert refusal == 'unknown selftest scenarios: nope; available: a, b, c', refusal
    print('PASS harness: waits retry cut-off error responses; timeouts report the last error, state failure and log tail; race exits fail the stop; process_gone reads the state field; scenarios run by name')


if __name__ == '__main__':
    run_selected('e2e', [
        ('harness', harness_scenario),
        ('settings', settings_scenario),
        *[(mode, functools.partial(scenario, mode)) for mode in ['normal', 'custom-route', 'interactive', 'failed-start', 'failed-discovery', 'failed-executor-start', 'parallel', 'existing-pr', 'external-context', 'dependencies', 'malformed-review', 'incomplete-review', 'failed-verification', 'remote-conflict', 'idle', 'interrupt-publication', 'closed-after-publication', 'cap1-interrupt']],
        *[(f'missing-{role}', functools.partial(missing_session_scenario, role)) for role in ['executor', 'repair']],
        *[(f'audit-{mode}', functools.partial(audit_scenario, mode)) for mode in ['accepted', 'idle', 'malformed', 'budget', 'failed', 'interrupted', 'queued']],
    ], sys.argv[1:])
