#!/usr/bin/env python3
"""Temporary, clearly synthetic data for browser tests; never used by the shipped app."""
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import socketserver
import sqlite3
import subprocess
import tempfile
import threading
import time
from datetime import datetime, timezone

from harness import wait_service_ready

project = Path(__file__).resolve().parents[1]
binary = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(project / 'bin/octomus-agent'))).resolve()
with tempfile.TemporaryDirectory(prefix='octomus-browser-') as directory:
    data = Path(directory)
    config = json.loads(subprocess.check_output([str(binary), '--print-config'], timeout=15))
    for role in config['roles']:
        config['roles'][role] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
    for tier, effort in [('XS', 'xhigh'), ('S', 'max'), ('M', 'low'), ('L', 'medium'), ('XL', 'high')]:
        config['tiers'][tier] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': effort}
    config['repair_route'] = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
    task_config = {**config, 'verification_commands': ['go test ./...']}
    now = datetime.now(timezone.utc).isoformat()
    env = {key: value for key, value in os.environ.items() if key != 'OCTOMUS_NOTIFICATION_WEBHOOK_URL'}
    env['OCTOMUS_TOKEN'] = 'browser-test-operator-token-32-characters'
    env['OCTOMUS_SANDBOX'] = 'off'
    # The doctor opens (and so creates) the state database before it fails on the unrouted default configuration.
    subprocess.run([str(binary), '--data-dir', directory, '--doctor'], env=env, capture_output=True, timeout=60)
    assert (data / 'state.db').exists(), 'the doctor run did not create the state database'
    db = sqlite3.connect(data / 'state.db')
    def put(kind, identity, value):
        db.execute('INSERT OR REPLACE INTO records VALUES (?,?,?)', (kind, identity, json.dumps(value)))
    rows = [('task-active', 'Complete the repository setup flow', 'queued', 'features', 'M'), ('task-reviewed', 'Explain the local development workflow', 'published', 'documentation', 'S'), ('task-blocked', 'Handle interrupted verification commands', 'blocked', 'correctness', 'M')]
    proposals = []
    for identity, title, status, category, tier in rows:
        proposal = {'id': identity, 'title': title, 'problem': 'A project-specific improvement grounded in repository evidence.', 'evidence': ['cmd/octomus-agent/main.go: service lifecycle'], 'benefit': 'A clearer and more reliable project.', 'category': category, 'target': 'main', 'tier': tier, 'scope': 'Preserve existing behavior and add proportionate verification.', 'dependencies': [], 'prompt': 'Complete the accepted improvement with useful verification and accurate documentation.', 'decision': 'accepted', 'reason': 'The orchestrator and both independent reviewers found a concrete benefit.', 'problem_key': '', 'relevant_paths': [], 'reconsiders': []}
        proposals.append(proposal)
        sandboxed = {'image_id': 'sha256:' + 'f' * 64, 'runtime': '', 'runs': 1, 'oom': False, 'egress': {'allowed': {'api.openai.com:443': 14, 'proxy.golang.org:443': 3}, 'denied': {'example.com:443': 2}, 'failed': {'registry.npmjs.org:443': 1}}}
        review = {'session_id': 'review-session', 'revision': 'b' * 40, 'comparison_base': 'a' * 40, 'created_at': now, 'result': {'completed': True, 'summary': 'The full change set meets the objective without actionable findings.', 'findings': []}}
        put('task', identity, {'id': identity, 'cycle_id': 'cycle-1', 'proposal': proposal, 'status': status, 'route': config['tiers'][tier], 'config': task_config, 'source_revision': 'a' * 40, 'comparison_base': 'a' * 40, 'default_revision': 'a' * 40, 'branch': f'octomus/{identity}', 'workspace': f'/srv/project/.octomus/tasks/{identity}/workspace', 'execution_session': 'execution-session' if status != 'queued' else None, 'repair_session': None, 'sessions': [{'id': 'execution-session', 'role': 'executor', 'route': config['tiers'][tier], 'status': 'completed', 'started_at': now, 'summary': 'Implemented and verified the accepted scope.', 'sandbox': sandboxed if status == 'published' else None}] if status != 'queued' else [], 'reviews': [review] if status == 'published' else [], 'verification': [{'command': 'go test ./...', 'success': True, 'output': 'All tests passed.', 'revision': 'b' * 40, 'created_at': now, 'sandbox': {**sandboxed, 'oom': True, 'incomplete': True, 'egress': {'allowed': {'proxy.golang.org:443': 5}, 'denied': {}}}}] if status == 'published' else [], 'output_commit': 'b' * 40 if status == 'published' else None, 'pr_number': 12 if status == 'published' else None, 'pr_url': 'https://github.com/fixture/project/pull/12' if status == 'published' else None, 'attempts': 0, 'review_baseline': 0, 'superseded_by': [], 'supersedes': [], 'rediscovery_requested': False, 'auto_merge_snapshot': None, 'lifecycle': {'archived_at': None, 'discarded_at': None}, 'error': 'Verification timed out. Workspace preserved for inspection.' if status == 'blocked' else None, 'created_at': now, 'updated_at': now})
    slots = ['adversary-a', 'adversary-b']
    reviewer_sessions = [{'id': f'{slot}-session', 'role': slot, 'route': config['roles']['proposal_reviewer'], 'status': 'completed', 'started_at': now, 'summary': f'Synthetic browser-test review recorded for {slot}.'} for slot in slots]
    batches = [{'assessments': [{'id': p['id'], 'decision': 'accepted', 'reason': f'Synthetic browser-test verdict recorded for {slot}: the saved scope is concrete and bounded.'} for p in proposals]} for slot in slots]
    put('cycle', 'cycle-1', {'id': 'cycle-1', 'number': 1, 'mode': 'execution', 'status': 'completed', 'started_at': now, 'completed_at': now, 'grounding': {'revision': 'a' * 40, 'prs': [], 'external_prs': [{'number': 31, 'url': 'https://github.com/fixture/project/pull/31', 'title': 'Adjust the retry backoff', 'body': 'Synthetic browser test context.', 'branch': 'contributor/backoff', 'head': 'c' * 40, 'base': 'main', 'head_repository': 'contributor/project', 'base_repository': 'fixture/project', 'title_truncated': False, 'body_truncated': False}], 'pr_coverage': {'observed_at': now, 'complete': True, 'total_open': 2, 'total_external': 1, 'included_external': 1, 'omitted_external': 0, 'max_external': 20, 'max_title_chars': 200, 'max_body_chars': 2000, 'max_context_bytes': 20000}, 'history': [], 'maintenance_due': False, 'maintenance_targets': []}, 'proposals': proposals, 'assessments': batches, 'sessions': reviewer_sessions, 'error': None})
    def observation(number, title, branch, owned, head, delivered=None):
        pull = {'number': number, 'title': title, 'branch': branch, 'head': head, 'base': 'main', 'url': f'https://github.com/fixture/project/pull/{number}', 'body': 'Synthetic browser test pull request.', 'state': 'open', 'changed_lines': 42, 'created_at': now, 'owned': owned, 'head_repository': 'fixture/project' if owned else 'contributor/project', 'base_repository': 'fixture/project'}
        return {'repository': 'fixture/project', 'pr': pull, 'observed_at': now, 'delivered_head': delivered, 'external_head_movement': delivered is not None and delivered != head}
    put('pr', 'fixture/project:12', observation(12, rows[1][1], 'octomus/task-reviewed', True, 'b' * 40, delivered='b' * 40))
    put('pr', 'fixture/project:31', observation(31, 'Adjust the retry backoff', 'contributor/backoff', False, 'c' * 40))
    put('pr', 'fixture/project:7', observation(7, 'Record the first delivered change', 'octomus/first-delivery', True, 'd' * 40, delivered='e' * 40))
    checks = [('non_root', 'Runs as an unprivileged user', 'uid 10001'), ('no_capabilities', 'Holds no Linux capabilities', 'effective 0000000000000000, bounding 0000000000000000'), ('no_new_privileges', 'Cannot gain privileges through setuid programs', 'no_new_privs 1'), ('seccomp', 'System calls are filtered by seccomp', 'seccomp mode 2'), ('read_only_image', 'The image filesystem is read-only', 'read-only'), ('no_orchestrator_state', 'Cannot see Octomus state, secrets or the Docker socket', 'none visible'), ('no_direct_egress', 'Has no direct route to the internet', 'no route'), ('no_external_dns', 'Cannot resolve internet names directly', 'lookup refused'), ('no_host_route', 'Has no gateway to the host or its neighbours', 'no default route'), ('resource_limits', 'Runs under memory and process limits', 'memory.max 4294967296, pids.max 1024'), ('egress_gateway', 'The egress gateway refuses unlisted, metadata and local targets', 'refused example.com:443, 169.254.169.254:80, localhost:4200')]
    put('settings', 'sandbox_self_test', {'at': now, 'passed': True, 'checks': [{'id': i, 'label': label, 'passed': True, 'detail': detail} for i, label, detail in checks], 'kernel': 'synthetic', 'image_id': 'sha256:' + 'f' * 64, 'error': None})
    db.commit()
    db.close()
    broker_info = {'version': '0.1.0', 'docker_version': '29.0.0', 'api_version': '1.51', 'image': 'octomus-sandbox:local', 'image_id': 'sha256:' + 'f' * 64, 'image_digests': [], 'runtime': '', 'runners': {'codex': 'codex-cli 0.153.4', 'opencode': '1.18.30'}, 'limits': {'nano_cpus': 2000000000, 'memory_bytes': 4294967296, 'pids': 1024, 'tmpfs_bytes': 1073741824, 'max_sandboxes': 12, 'max_seconds': 21600}, 'networks': {'runner': 'octomus-sandbox-runner', 'verify': 'octomus-sandbox-verify'}, 'egress': True, 'live': 0}

    class Broker(BaseHTTPRequestHandler):
        """Synthetic sandbox broker: reports its posture and runs nothing."""
        def address_string(self):
            return 'broker'

        def do_GET(self):
            body = json.dumps(broker_info).encode()
            self.send_response(200 if self.path == '/v1/info' else 404)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    broker = socketserver.ThreadingUnixStreamServer(str(data / 'sandboxd.sock'), Broker)
    threading.Thread(target=broker.serve_forever, daemon=True).start()
    env.update(OCTOMUS_SANDBOX='docker', OCTOMUS_SANDBOXD_SOCKET=str(data / 'sandboxd.sock'), OCTOMUS_EGRESS_MODEL_HOSTS='chatgpt.com,auth.openai.com,api.openai.com', OCTOMUS_EGRESS_BUILD_HOSTS='proxy.golang.org,registry.npmjs.org')
    broker.daemon_threads = True
    with (data / 'service.log').open('w') as log:
        process = None
        try:
            process = subprocess.Popen([str(binary), '--listen', '127.0.0.1:0', '--data-dir', directory, '--assets', str(project / 'web/build')], env=env, stdout=log, stderr=log)
            port = wait_service_ready(process, data / 'service.log', deadline=time.monotonic() + 45, label='Browser service startup')
            print(f'Browser fixture ready on port {port}', flush=True)
            process.wait()
            if process.returncode:
                raise AssertionError(f'Browser service exited with status {process.returncode}\n{(data / "service.log").read_text()}')
        except KeyboardInterrupt:
            pass
        finally:
            if process is not None and process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=2)
            broker.shutdown()
            broker.server_close()
