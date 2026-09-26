#!/usr/bin/env python3
import functools
import http.server
import json
from pathlib import Path
import re
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time

from e2e import BINARY, Service, base_config, poll, run_selected, setup
from e2e_runners import configuration, stop_service_and_peers

ENV = 'OCTOMUS_NOTIFICATION_WEBHOOK_URL'
SECRET = 'synthetic-path-secret-9f27c1/query?key=synthetic-query-secret-4d80'


class Receiver:
    def __init__(self):
        self.requests = []
        self.lock = threading.Lock()

        class Handler(http.server.BaseHTTPRequestHandler):
            outer = self

            def do_POST(self):
                outer = self.outer
                length = int(self.headers.get('Content-Length', '0'))
                body = self.rfile.read(length)
                with outer.lock:
                    outer.requests.append({'path': self.path, 'headers': dict(self.headers), 'body': body})
                self.send_response(200)
                self.send_header('Content-Length', '0')
                self.end_headers()

            def log_message(self, *args):
                pass

        self.server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def url(self):
        return f'http://127.0.0.1:{self.port}/{SECRET}'

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
    log_text = (root / 'service.log').read_text()
    assert SECRET not in log_text, 'webhook URL leaked into service.log'
    state = service.request('/state')
    payloads = [state, service.request('/config'), service.request('/baseline-checks/latest')]
    for task in state['tasks']:
        payloads.append(service.request(f"/tasks/{task['id']}"))
    for cycle in state['cycles']:
        payloads.append(service.request(f"/cycles/{cycle['id']}"))
        payloads.append(service.request(f"/cycles/{cycle['id']}/evidence"))
    for payload in payloads:
        assert SECRET not in json.dumps(payload), 'webhook URL leaked into the API'
    for args in [['--usage-report']] + [['--export-run', cycle['id']] for cycle in state['cycles']]:
        result = subprocess.run([str(BINARY), '--data-dir', str(root / '.octomus'), *args], env=service.env, capture_output=True, text=True, check=True, timeout=30)
        assert SECRET not in result.stdout and SECRET not in result.stderr
    db = sqlite3.connect(root / '.octomus/state.db')
    leaked = []
    for table in ['records', 'events', 'notification_outbox', 'notification_policy']:
        for row in db.execute(f'SELECT * FROM {table}'):
            if any(SECRET in str(column) for column in row):
                leaked.append(table)
    destination = db.execute('SELECT destination_id FROM notification_policy WHERE id=1').fetchone()
    db.close()
    assert not leaked, f'webhook URL persisted in {leaked}'
    assert destination and re.fullmatch(r'[0-9a-f]{64}', destination[0]), 'policy must store only the URL fingerprint'


def scenario(mode):
    with tempfile.TemporaryDirectory(prefix=f'octomus-notify-{mode}-') as tmp:
        root = Path(tmp)
        setup(root)
        receiver = Receiver()
        service = Service(root)
        try:
            service.env[ENV] = receiver.url()
            if mode == 'deliver':
                (root / 'malformed-review').touch()
                service.start()
                service.configure()
                found = receiver.wait(lambda rows: [r for r in rows if attention(r['body'])['task_id']], 'attention delivery without dashboard polling')[0]
                event = attention(found['body'])
                task = service.request(f"/tasks/{event['task_id']}")
                assert task['status'] == 'blocked', task
                assert event['category'] == 'runner_unavailable' and event['action'] == 'inspect_task'
                assert event['repository'] == 'fixture/project' and event['cycle_id'] and event['run_id']
                assert found['path'] == f'/{SECRET}', found['path']
                assert {k.lower(): v for k, v in found['headers'].items()}.get('content-type') == 'application/json'
                health = service.request('/state')['notifications']
                assert health['state'] == 'enabled' and health['configured'], health
                assert_no_url_leak(root, service)
                print('PASS deliver: blocked task produced one minimal attention event')
                return
            if mode == 'restart':
                (root / 'malformed-review').touch()
                service.start()
                service.configure()
                task = service.wait(service.terminal_task, 'blocked task')
                receiver.wait(lambda rows: [r for r in rows if attention(r['body'])['task_id'] == task['id']], 'attention delivery')
                assert len(receiver.events()) == 1
                service.stop(crash=True)
                service.start()
                time.sleep(5)
                assert len(receiver.events()) == 1, f'restart must not re-notify a delivered episode: {receiver.events()}'
                print('PASS restart: a delivered episode is not repeated after a crash')
                return
            if mode == 'service-error':
                (root / 'failed-discovery').touch()
                service.start()
                service.configure()
                found = receiver.wait(lambda rows: [r for r in rows if attention(r['body'])['category'] == 'service_error_paused'], 'service pause event')[0]
                event = attention(found['body'])
                assert event['action'] == 'inspect_service' and event['task_id'] is None, event
                state = service.request('/state')
                assert state['control']['paused'] and state['control']['error'], state['control']
                print('PASS service-error: an error pause raises one inspect_service event')
                return
            if mode in ['env-strip', 'env-strip-opencode']:
                service.start()
                config = configuration(service) if mode == 'env-strip-opencode' else base_config(service, [f'test -z "${{{ENV}+x}}"', 'for file in feature*.txt; do test "$(cat "$file")" = fixed || exit 1; done'], cycle_interval_seconds=3600, task_timeout_seconds=120)
                if mode == 'env-strip':
                    codex = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
                    # Shipped tiers and repair carry effort but no model.
                    for role in config['roles']:
                        config['roles'][role] = dict(codex)
                    for tier in config['tiers']:
                        config['tiers'][tier] = dict(codex)
                    config['repair_route'] = dict(codex)
                service.save_config(config)
                service.request('/control/cycle', 'POST')
                task = service.wait(service.terminal_task, 'published task')
                assert task['status'] == 'published', task['error']
                assert SECRET not in (root / 'service.log').read_text()
                print('PASS env-strip: published work never saw the webhook environment')
                return
            raise AssertionError(f'unknown notifications scenario {mode}')
        finally:
            try:
                stop_service_and_peers(service, root)
            finally:
                receiver.close()


if __name__ == '__main__':
    run_selected('notifications', [(mode, functools.partial(scenario, mode)) for mode in ['deliver', 'restart', 'service-error', 'env-strip', 'env-strip-opencode']], sys.argv[1:])
    if not sys.argv[1:]:
        print('All notification scenarios passed')
