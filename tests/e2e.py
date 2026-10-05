#!/usr/bin/env python3
"""The service scenarios: the real service, scheduler, SQLite and Git against deterministic peers.

No network writes, real model turns, credentials or spending. Run after `make build` or set
OCTOMUS_TEST_BINARY. `python3 tests/e2e.py [SCENARIO...]` runs the named scenarios, or all of
them; an unknown name lists them. Up to four run in parallel; OCTOMUS_TEST_JOBS sets the limit.
"""
import contextlib
import fcntl
import functools
import http.server
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import threading

from harness import BINARY, FEATURE_CHECK, PROJECT, TOKEN, fixture_service, git, poll, routes, run_selected

WEBHOOK_ENV = 'OCTOMUS_NOTIFICATION_WEBHOOK_URL'
WEBHOOK_SECRET = 'synthetic-path-secret-9f27c1/query?key=synthetic-query-secret-4d80'
PUBLICATION_SECRET = 'ghp_fixturePublicationSecret0001'


def usage_report(root):
    report = json.loads(subprocess.check_output([str(BINARY), '--data-dir', str(root / '.octomus'), '--usage-report'], text=True, timeout=30))
    assert sum(d['admissions'] for d in report['daily']) == len(report['admissions'])
    assert all(d['unattributed_admissions'] == 0 for d in report['daily'])
    return report


def existing_pr(root):
    """Pushes an owned branch with an open PR the fixture's gh reports as #42, and targets proposals at it."""
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


def update_prs(root, change):
    """Lets `change(prs)` edit the gh fixture's saved PR list under the fixture's lock, replacing the file whole."""
    with (root / 'github.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        path = root / 'prs.json'
        prs = json.loads(path.read_text())
        change(prs)
        temporary = root / 'prs.json.tmp'
        temporary.write_text(json.dumps(prs))
        os.replace(temporary, path)


def normal(executor='codex', **roles):
    """Two independent proposals are planned, implemented, reviewed, repaired, verified and published.

    The first proposal carries secrets in its title and problem: the PR, the commit message and
    the API show them redacted while the saved record stays canonical. The service runs with the
    webhook URL set, and the verification commands prove that neither it nor the operator token
    reaches a child. The doctor is checked over HTTP before the cycle and from the CLI after it.
    """
    def prepare(root):
        (root / 'parallel').touch()
        (root / 'proposal-override.json').write_text(json.dumps({
            'title': f'Complete the fixture feature {PUBLICATION_SECRET}',
            'problem': f'Missing output; leaked environment value {TOKEN} and {PUBLICATION_SECRET}'}))

    selected = routes(executor, **roles)
    backends = {r['backend'] for r in [*selected['roles'].values(), *selected['tiers'].values(), selected['repair_route']]}
    commands = [f'test -z "${{{WEBHOOK_ENV}+x}}"', 'test -z "${OCTOMUS_TOKEN+x}"', FEATURE_CHECK]
    with fixture_service('octomus-normal-', prepare, env={WEBHOOK_ENV: f'http://127.0.0.1:9/{WEBHOOK_SECRET}'}) as (root, service):
        preview = root / 'bin/opencode-preview'
        shutil.copy(root / 'bin/opencode', preview)
        models = service.request('/model-catalog', 'POST', {'backend': 'opencode', 'binary': str(preview)})
        assert any(m['available'] for m in models)
        assert not any(word in json.dumps(models) for word in ['fixture-credential', 'another-fixture-secret', 'PRIVATE_API_KEY'])
        assert service.request('/config')['config']['opencode_binary'] == 'opencode'
        assert not usage_report(root)['admissions'] and not (root / 'protocol.jsonl').exists()

        saved = service.configure(selected, commands, start=False)
        diagnostic = service.request('/doctor', 'POST')
        assert diagnostic['checked_revision'] == saved['revision'] and diagnostic['checked_config'] == saved['config']
        assert {d['backend'] for d in diagnostic['backends']} == backends and diagnostic['warnings'] == []
        if 'codex' in backends:
            assert diagnostic['codex_version'] == 'codex-cli 0.153.4' and diagnostic['tested_codex_version'] == '0.153.4'
            (root / 'version').write_text('0.0.0-fixture')
            diagnostic = service.request('/doctor', 'POST')
            assert 'mismatch' in diagnostic['message'] and len(diagnostic['warnings']) == 1
            (root / 'version').unlink()
        assert not (root / 'protocol.jsonl').exists(), 'the doctor must not start model turns'
        service.request('/control/cycle', 'POST')

        service.wait(service.terminal_task, 'task completion')
        service.wait(lambda: len([t for t in service.request('/state')['tasks'] if t['status'] == 'published']) == 2, 'both tasks delivered')
        tasks = [service.request(f'/tasks/{t["id"]}') for t in service.request('/state')['tasks']]
        assert len({t['workspace'] for t in tasks}) == 2 and len({t['execution_session'] for t in tasks}) == 2
        for task in tasks:
            assert task['status'] == 'published', task['error']
            assert task['route'] == selected['tiers']['M'] and task['config']['repair_route'] == selected['repair_route']
            assert task['workspace'].endswith(f'tasks/{task["id"]}/workspace')
            assert len(task['reviews']) == 3 and len({r['session_id'] for r in task['reviews']}) == 3
            assert all(r['comparison_base'] == task['comparison_base'] == task['default_revision'] for r in task['reviews'])
            repairs = [s for s in task['sessions'] if s['role'] == 'repair']
            assert len(repairs) == 1 and repairs[0]['route'] == selected['repair_route']
            assert task['verification'][-1]['success'] and task['verification'][-1]['revision'] == task['output_commit']
        prs = json.loads((root / 'prs.json').read_text())
        assert len(prs) == 2 and len((root / 'publications.jsonl').read_text().splitlines()) == 2
        assert git('rev-parse', 'main', cwd=root / 'remote.git') == tasks[0]['default_revision'], 'Default branch must never be pushed'
        protocol = [json.loads(line) for line in (root / 'protocol.jsonl').read_text().splitlines()]
        assert len([p for p in protocol if p['prompt'].startswith('Discover worthwhile')]) == 9
        assert len([p for p in protocol if p['prompt'].startswith('Adversarial proposal')]) == 2
        assert len({p['thread'] for p in protocol if p['prompt'].startswith('Repair actionable')}) == 2
        assert all(p['sandbox'] == {'type': 'dangerFullAccess'} and p['approval'] == 'never' for p in protocol if p.get('backend') != 'opencode')
        report = usage_report(root)
        assert len(report['admissions']) == 25 and sum(a['role'] == 'repair' for a in report['admissions']) == 4
        assert report['cycles'][0]['planning_admissions'] == 13 and report['cycles'][0]['task_admissions'] == 12
        assert all(t['repair_route'] == selected['repair_route'] for t in report['tasks'])
        assert ('opencode' in backends) == any(a['route']['backend'] == 'opencode' for a in report['admissions'])

        secret_task = next(t for t in tasks if '[redacted]' in t['proposal']['title'])
        pr = next(p for p in prs if p['number'] == secret_task['pr_number'])
        sent = pr['title'] + '\n' + pr['body']
        assert '[redacted]' in pr['title'] and TOKEN not in sent and PUBLICATION_SECRET not in sent, sent
        assert f'<!-- octomus:task:{secret_task["id"]} -->' in sent and f'Reviewed commit: `{secret_task["output_commit"]}`' in sent, sent
        messages = git('log', '--format=%B', secret_task['branch'], cwd=root / 'remote.git')
        assert TOKEN not in messages and PUBLICATION_SECRET not in messages, messages
        assert 'Complete the fixture feature [redacted]' in messages, messages
        assert WEBHOOK_SECRET not in (root / 'service.log').read_text(), 'webhook URL leaked into service.log'

        service.stop()
        with sqlite3.connect(root / '.octomus/state.db') as db:
            canonical = json.loads(db.execute("SELECT data FROM records WHERE kind='task' AND id=?", (secret_task['id'],)).fetchone()[0])
        assert PUBLICATION_SECRET in canonical['proposal']['title'] and TOKEN in canonical['proposal']['problem'], canonical['proposal']
        if 'codex' in backends:
            (root / 'version').write_text('0.0.0-fixture')
            diagnostic = subprocess.run([str(BINARY), '--data-dir', str(root / '.octomus'), '--doctor'], env=service.env, capture_output=True, text=True, check=True, timeout=60)
            assert json.loads(diagnostic.stdout)['warnings'] and 'mismatch' in diagnostic.stderr


def interrupt_publication():
    """A crash right after the PR is created recovers to a published task without a duplicate PR."""
    with fixture_service('octomus-interrupt-', lambda root: (root / 'interrupt-publication').touch()) as (root, service):
        service.configure(routes())
        service.wait(lambda: (root / 'publication-created').exists(), 'publication side effect')
        service.stop(crash=True)
        service.start()
        task = service.wait(service.terminal_task, 'recovered publication')
        assert task['status'] == 'published', task['error']
        assert len(json.loads((root / 'prs.json').read_text())) == 1
        assert len((root / 'publications.jsonl').read_text().splitlines()) == 1


def audit():
    """An audit records decisions without queueing work, conflicts with other controls while it runs, and survives a restart unpublished."""
    with fixture_service('octomus-audit-') as (root, service):
        selected = routes()
        selected['roles']['code_reviewer'] = {'model': 'unavailable', 'effort': 'high'}
        selected['repair_route'] = {'model': 'unavailable', 'effort': 'high'}
        saved = service.configure(selected, [], start=False)
        diagnostic = service.request('/doctor?mode=audit', 'POST')
        assert diagnostic['mode'] == 'audit' and diagnostic['checked_revision'] == saved['revision'] and diagnostic['checked_config'] == saved['config']
        code, body = service.expect('/doctor', 'POST')
        assert code == 400, ('Execution doctor accepted missing verification', code, body)
        assert body['checked_revision'] == saved['revision'] and body['checked_config'] == saved['config']
        (root / 'audit-decisions').touch()
        (root / 'audit-hold').touch()
        baseline_refs = git('for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads', cwd=root / 'remote.git')
        code, response = service.expect('/control/audit', 'POST')
        assert code == 200, (code, response)
        service.wait(lambda: (root / 'audit-entered').exists(), 'audit started')
        state = service.request('/state')
        assert state['status'] == 'auditing' and state['control']['paused']
        for action in ['audit', 'resume', 'cycle']:
            code, body = service.expect('/control/' + action, 'POST')
            assert code == 409, ('Conflicting control accepted', action, code, body)
        (root / 'audit-hold').unlink()

        def completed_audit():
            state = service.request('/state')
            return state if state['cycles'] and not state['cycle_active'] and state['cycles'][0]['status'] == 'completed' else None
        state = service.wait(completed_audit, 'audit completion')
        cycle = service.request('/cycles/' + state['cycles'][0]['id'])
        assert cycle['mode'] == 'audit' and state['control']['paused'] and state['tasks'] == []
        assert {p['decision'] for p in cycle['proposals']} == {'accepted', 'rejected', 'deferred'}
        assert all(p['reason'] for p in cycle['proposals']) and len(cycle['assessments']) == 2
        row = next(c for c in usage_report(root)['cycles'] if c['id'] == cycle['id'])
        assert row['mode'] == 'audit' and row['task_admissions'] == 0 and row['planning_admissions'] == 13
        observed = state['pr_capacity']['observed_at']
        service.stop()
        service.start()
        service.wait(lambda: service.request('/state')['pr_capacity']['observed_at'] != observed, 'fresh PR observation after restart')
        assert service.request('/state')['tasks'] == []
        assert not (root / 'publications.jsonl').exists()
        assert git('for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads', cwd=root / 'remote.git') == baseline_refs
        service.stop()
        diagnostic = subprocess.run([str(BINARY), '--data-dir', str(root / '.octomus'), '--doctor', '--audit'], env=service.env, capture_output=True, text=True, check=True, timeout=60)
        assert json.loads(diagnostic.stdout)['mode'] == 'audit'


def chain():
    """Three dependent tasks land on one existing PR in order; archival, restarts and a repeat plan change nothing on the remote.

    After the chain is delivered, an unrelated commit on main and a new cycle re-propose the
    same work: planning fails because it duplicates recorded tasks, and nothing is published.
    """
    def prepare(root):
        existing_pr(root)
        (root / 'chain').touch()

    with fixture_service('octomus-chain-', prepare) as (root, service):
        service.configure(routes())
        service.wait(lambda: service.request('/state')['counts'].get('published') == 3, 'three branch tasks delivered')
        tasks = [service.request('/tasks/' + r['id']) for r in service.request('/state')['tasks']]
        for t in tasks:
            if t['proposal']['dependencies']:
                predecessor = next(x for x in tasks if x['id'] == t['proposal']['dependencies'][0])
                assert t['source_revision'] == predecessor['output_commit']
        first = next(t for t in tasks if not t['proposal']['dependencies'])
        service.request('/tasks/' + first['id'] + '/archive', 'POST')
        delivered = {p['pr']['number']: p['observed_at'] for p in service.request('/state')['prs']}
        service.stop()
        service.start()

        def refreshed_prs():
            prs = service.request('/state')['prs']
            return bool(prs) and all(p['observed_at'] != delivered.get(p['pr']['number']) for p in prs)
        service.wait(refreshed_prs, 'refreshed observation of the archived predecessor')
        assert not service.request('/state')['prs'][0]['external_head_movement'], 'Archival must not replace the latest known delivery head'
        service.wait(lambda: service.request('/state')['control']['paused'], 'one-shot completion')
        cycles = len(service.request('/state')['cycles'])
        observed = service.request('/state')['pr_capacity']['observed_at']
        service.stop()
        service.start()
        service.wait(lambda: service.request('/state')['pr_capacity']['observed_at'] != observed, 'fresh PR observation after restart')
        assert service.request('/state')['control']['mode'] == 'paused'
        assert len(service.request('/state')['cycles']) == cycles

        publications = (root / 'publications.jsonl').read_text()
        checkout = root / 'checkout'
        (checkout / 'unrelated.txt').write_text('Unrelated change on main\n')
        git('add', '.', cwd=checkout)
        git('commit', '-m', 'Unrelated main change', cwd=checkout)
        git('push', 'origin', 'main', cwd=checkout)
        service.request('/control/cycle', 'POST')
        state = service.wait(lambda: (s := service.request('/state'))['cycles'][0]['id'] != first['cycle_id'] and not s['cycle_active'] and s, 'duplicate planning completes')
        cycle = service.request('/cycles/' + state['cycles'][0]['id'])
        assert cycle['grounding']['revision'] == git('rev-parse', 'main', cwd=root / 'remote.git')
        assert any(pr['number'] == 42 and pr['state'] == 'open' for pr in cycle['grounding']['prs'])
        assert cycle['status'] == 'failed' and 'duplicates recorded work' in cycle['error'], cycle
        assert len(state['tasks']) == 3 and (root / 'publications.jsonl').read_text() == publications


def pr_outcome():
    """Remote observation while paused: external head movement, context changes and a merged PR, each across a restart."""
    with fixture_service('octomus-pr-outcome-') as (root, service):
        service.configure(routes())
        task = service.wait(service.terminal_task, 'published task')
        assert task['status'] == 'published', task['error']
        service.wait(lambda: service.request('/state')['control']['paused'], 'one-shot paused')
        assert service.request('/state')['prs'][0]['pr']['head'] == task['output_commit']
        service.stop()
        service.start()
        service.wait(lambda: service.request('/state')['control']['context_fingerprint'], 'initial repository observation')
        fingerprint = service.request('/state')['control']['context_fingerprint']
        deadline = service.request('/state')['control']['next_cycle_at']
        service.stop()
        remote = str(root / 'remote.git')
        tree = git('--git-dir', remote, 'rev-parse', task['output_commit'] + '^{tree}', cwd=root)
        advanced = git('--git-dir', remote, '-c', 'user.name=External', '-c', 'user.email=fixture@example.com', 'commit-tree', tree, '-p', task['output_commit'], '-m', 'External follow-up', cwd=root)
        git('--git-dir', remote, 'update-ref', 'refs/heads/' + task['branch'], advanced, cwd=root)
        service.start()
        service.wait(lambda: service.request('/state')['prs'][0]['external_head_movement'], 'external head observed while paused')
        service.wait(lambda: service.request('/state')['control']['context_fingerprint'] != fingerprint, 'changed context observed')
        assert service.request('/state')['control']['next_cycle_at'] == deadline, 'Observations must preserve ordinary cadence'
        service.stop()
        update_prs(root, lambda prs: prs[0].update(state='closed', merged_at='2026-09-10T00:00:00Z'))
        service.start()
        service.wait(lambda: service.request('/state')['merged_prs'] == 1, 'merge outcome reconciled from old open record')
        assert service.request('/tasks/' + task['id'])['status'] == 'published'
        assert service.request('/state')['control']['paused']


def baseline():
    """Clean-baseline checks: a pass without model routes, a failure that keeps going, and a held check that gates every other control until cancelled."""
    def no_routes():
        empty = {'backend': 'codex', 'model': '', 'effort': ''}
        return {'roles': {role: dict(empty) for role in ['orchestrator', 'discovery', 'proposal_reviewer', 'code_reviewer']},
                'tiers': {tier: {'model': '', 'effort': ''} for tier in ['XS', 'S', 'M', 'L', 'XL']}, 'repair_route': dict(empty)}

    def latest(service):
        return service.request('/baseline-checks/latest')

    def start_check(service, revision, expected=202):
        code, check = service.expect('/baseline-checks', 'POST', {'expected_revision': revision})
        assert code == expected, (code, check)
        return check

    def wait_check(service, statuses, seconds=60):
        """The latest check in one of `statuses`, its clone removed (or the failure recorded) and the baseline slot free."""
        def done():
            check = latest(service)['check']
            if not check or check['status'] not in statuses or not (check['workspace_removed'] or check['cleanup_error']):
                return None
            return None if service.request('/state')['baseline_active'] else check
        return service.wait(done, f'baseline reaching {statuses}', seconds)

    with fixture_service('octomus-baseline-') as (root, service):
        proof = root / 'canonical-proof'
        secret_command = f'echo {TOKEN} > {proof}'
        displayed = f'echo [redacted] > {proof}'
        saved = service.configure(no_routes(), ["test \"$(cat README.md)\" = 'Feature contract: feature.txt must contain fixed.'", 'test ! -e untracked.txt', 'test ! -e scratchpad.tmp', secret_command], start=False, command_timeout_seconds=60)
        view = latest(service)
        assert view['check'] is None and view['eligible'] and not service.request('/state')['baseline_active'], view
        entry = next(t for t in saved['transformed_fields'] if t['field'] == 'verification_commands')
        assert entry['kinds'] == ['redacted'] and entry['paths'] == [['verification_commands', 3]], saved['transformed_fields']
        assert saved['config']['verification_commands'][3] == displayed and len(saved['revision']) == 64
        for path in ['README.md', 'untracked.txt', 'scratchpad.tmp']:
            (root / 'checkout' / path).write_text('dirty local edits\n')
        assert start_check(service, '0' * 64, expected=409)['error'], 'a stale revision must conflict before any clone is made'
        assert start_check(service, saved['revision'])['status'] == 'running'
        check = wait_check(service, ['passed'])
        assert len(check['commands']) == 4 and all(c['success'] for c in check['commands']), check
        assert check['commands'][3]['command'] == displayed and check['config']['verification_commands'][3] == displayed, check
        assert proof.read_text().strip() == TOKEN, 'the canonical secret-bearing command ran, not its display preview'
        assert check['revision'] and check['completed_at'] and not (root / '.octomus/baselines' / check['id']).exists()
        view = service.wait(lambda: latest(service) if latest(service)['revision_status'] == 'matches_last_observation' else None, 'default branch observation')
        assert view['config_matches'] and view['default_observation']['revision'] == check['revision'], view
        assert view['config_revision'] == check['config_fingerprint'] == saved['revision'], view

        changed = service.configure(routes(), ['true', 'false', 'echo third'], start=False)
        view = latest(service)
        assert view['config_matches'] is False and view['check']['id'] == check['id'], view
        start_check(service, changed['revision'])
        failed = wait_check(service, ['failed'])
        assert failed['id'] != check['id'] and [c['success'] for c in failed['commands']] == [True, False, True], failed['commands']
        assert 'exit status: 1' in failed['commands'][1]['output'] and failed['workspace_removed'], failed['commands'][1]

        marker = root / 'baseline-entered'
        held = service.configure(routes(), [f'touch {marker}; sleep 31338 & sleep 60'], start=False, command_timeout_seconds=60)
        check = start_check(service, held['revision'])
        service.wait(lambda: marker.exists(), 'command entry')
        start_check(service, held['revision'], expected=409)
        for path in ['/control/cycle', '/control/resume', '/control/audit']:
            assert service.expect(path, 'POST')[0] == 409, path
        assert service.expect('/config', 'PUT', {'expected_revision': held['revision'], 'config': {}})[0] == 409
        assert service.expect('/control/pause', 'POST')[0] == 200 and service.expect('/tasks')[0] == 200
        state = service.request('/state')
        assert state['baseline_active'] and state['baseline']['id'] == check['id'] and 'commands' not in state['baseline'], state['baseline']
        assert state['baseline']['config_revision'] == held['revision'] and {'config_matches', 'revision_status'} <= set(state['baseline']), state['baseline']
        code, ack = service.expect(f'/baseline-checks/{check["id"]}/cancel', 'POST')
        assert code == 200 and ack['ok'], ack
        cancelled = wait_check(service, ['cancelled'])
        assert poll(lambda: subprocess.run(['pgrep', '-f', 'sleep 31338'], capture_output=True).returncode != 0, 10, interval=0.2), 'descendant still running'
        assert cancelled['commands'] and cancelled['commands'][0]['success'] is False and cancelled['workspace_removed'], cancelled
        assert service.expect(f'/baseline-checks/{check["id"]}/cancel', 'POST')[0] == 409
        report = usage_report(root)
        assert report['admissions'] == [] and report['cycles'] == [] and report['tasks'] == [], report
        assert not (root / 'publications.jsonl').exists()


class Receiver:
    """A local webhook receiver recording every POST."""

    def __init__(self):
        self.requests = []
        self.lock = threading.Lock()
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                with outer.lock:
                    outer.requests.append({'path': self.path, 'headers': dict(self.headers), 'body': body})
                self.send_response(200)
                self.send_header('Content-Length', '0')
                self.end_headers()

            def log_message(self, *args):
                pass

        self.server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
        self.url = f'http://127.0.0.1:{self.server.server_address[1]}/{WEBHOOK_SECRET}'
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def events(self):
        with self.lock:
            return list(self.requests)

    def wait(self, predicate, label, seconds=30):
        found = poll(lambda: predicate(self.events()), seconds)
        if found:
            return found
        raise AssertionError(f'{label} timed out: {self.events()}')

    def close(self):
        self.server.shutdown()
        self.server.server_close()


def attention(body):
    event = json.loads(body)
    assert set(event) == {'schema_version', 'event_id', 'occurred_at', 'repository', 'cycle_id', 'run_id', 'task_id', 'category', 'action'}, event
    assert event['schema_version'] == 1 and re.fullmatch(r'[0-9a-f]{32}', event['event_id']), event
    return event


def assert_no_url_leak(root, service):
    assert WEBHOOK_SECRET not in (root / 'service.log').read_text(), 'webhook URL leaked into service.log'
    state = service.request('/state')
    payloads = [state, service.request('/config'), service.request('/baseline-checks/latest')]
    payloads += [service.request(f"/tasks/{task['id']}") for task in state['tasks']]
    for cycle in state['cycles']:
        payloads += [service.request(f"/cycles/{cycle['id']}"), service.request(f"/cycles/{cycle['id']}/evidence")]
    assert not any(WEBHOOK_SECRET in json.dumps(payload) for payload in payloads), 'webhook URL leaked into the API'
    for args in [['--usage-report']] + [['--export-run', cycle['id']] for cycle in state['cycles']]:
        result = subprocess.run([str(BINARY), '--data-dir', str(root / '.octomus'), *args], env=service.env, capture_output=True, text=True, check=True, timeout=30)
        assert WEBHOOK_SECRET not in result.stdout and WEBHOOK_SECRET not in result.stderr
    db = sqlite3.connect(root / '.octomus/state.db')
    leaked = [table for table in ['records', 'events', 'notification_outbox', 'notification_policy']
              if any(WEBHOOK_SECRET in str(column) for row in db.execute(f'SELECT * FROM {table}') for column in row)]
    destination = db.execute('SELECT destination_id FROM notification_policy WHERE id=1').fetchone()
    db.close()
    assert not leaked, f'webhook URL persisted in {leaked}'
    assert destination and re.fullmatch(r'[0-9a-f]{64}', destination[0]), 'policy must store only the URL fingerprint'


def notify():
    """A blocked task produces one minimal attention event at the configured webhook, and the URL never leaks anywhere."""
    with contextlib.closing(Receiver()) as receiver, fixture_service('octomus-notify-', lambda root: (root / 'malformed-review').touch(), env={WEBHOOK_ENV: receiver.url}) as (root, service):
        service.configure(routes())
        found = receiver.wait(lambda rows: [r for r in rows if attention(r['body'])['task_id']], 'attention delivery without dashboard polling')[0]
        event = attention(found['body'])
        task = service.request(f"/tasks/{event['task_id']}")
        assert task['status'] == 'blocked', task
        assert event['category'] == 'runner_unavailable' and event['action'] == 'inspect_task'
        assert event['repository'] == 'fixture/project' and event['cycle_id'] and event['run_id']
        assert found['path'] == f'/{WEBHOOK_SECRET}', found['path']
        assert {k.lower(): v for k, v in found['headers'].items()}.get('content-type') == 'application/json'
        health = service.request('/state')['notifications']
        assert health['state'] == 'enabled' and health['configured'], health
        assert_no_url_leak(root, service)


def upgrade():
    """A v0.1.0 state database opens on this binary and serves every saved record back.

    The golden's configuration points at a repository path from the generating fixture,
    which no longer exists; the service must still serve and stop cleanly.
    """
    def prepare(root):
        state = root / '.octomus'
        state.mkdir(mode=0o700)
        shutil.copyfile(PROJECT / 'internal/store/testdata/state-v0.1.0.db', state / 'state.db')
        (state / 'state.db').chmod(0o600)

    with fixture_service('octomus-upgrade-', prepare) as (root, service):
        state = service.request('/state')
        assert state['status'] == 'paused' and not state['cycle_active'], state['status']
        assert len(state['tasks']) == 3 and len(state['cycles']) == 2 and len(state['prs']) == 1
        tasks = [service.request(f'/tasks/{t["id"]}') for t in state['tasks']]
        assert all(t['status'] == 'published' and t['pr_number'] == 42 for t in tasks)
        assert [t['lifecycle']['archived_at'] is not None for t in tasks].count(True) == 1
        evidence = {}
        for row in state['cycles']:
            detail = service.request(f'/cycles/{row["id"]}')
            assert detail['id'] == row['id'] and detail['mode'] == 'execution' and len(detail['proposals']) == 3
            evidence[row['id']] = service.request(f'/cycles/{row["id"]}/evidence')
        service.stop()
        for cycle_id, via_http in evidence.items():
            exported = json.loads(subprocess.check_output([str(BINARY), '--data-dir', str(root / '.octomus'), '--export-run', cycle_id], text=True, timeout=30))
            assert exported.pop('generated_at') and via_http.pop('generated_at')
            assert exported == via_http, cycle_id
        with sqlite3.connect(root / '.octomus/state.db') as db:
            user_version = db.execute('PRAGMA user_version').fetchone()[0]
        backups = list((root / '.octomus').glob('state.db.v*-backup-*'))
        if user_version == 7:
            assert not backups, f'upgrade ran without a migration: {backups}'
        else:
            assert len(backups) == 1, f'expected one pre-upgrade backup: {backups}'


SCENARIOS = [
    ('normal', normal),
    ('normal-opencode', functools.partial(normal, 'opencode')),
    ('normal-mixed', functools.partial(normal, 'opencode', planning='codex', reviewer='codex')),
    ('interrupt-publication', interrupt_publication),
    ('audit', audit),
    ('chain', chain),
    ('pr-outcome', pr_outcome),
    ('baseline', baseline),
    ('notify', notify),
    ('upgrade', upgrade),
]

if __name__ == '__main__':
    run_selected('e2e', SCENARIOS, sys.argv[1:])
