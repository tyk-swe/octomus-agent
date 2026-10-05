#!/usr/bin/env python3
"""The Docker deployment end to end: the shipped compose file, a real broker, egress gateway and sandboxes.

Opt-in because it needs a Docker Engine 28+ daemon: run `make test-sandbox` or this file directly. It builds
test-only images (the control plane with the sandboxfixture tag, and a sandbox image carrying the deterministic
runner fixtures), starts an isolated compose project with its own volumes and networks, and never contacts GitHub
or a model provider.
"""
import contextlib
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import urllib.error
import urllib.request

from harness import FEATURE_CHECK, PROJECT, TOKEN, local_urlopen, poll, routes, run_selected, setup

COMPOSE = PROJECT / 'deploy/docker/compose.yaml'
IMAGES = {'base': 'octomus-agent:e2e-base', 'control': 'octomus-agent:e2e', 'sandbox': 'octomus-sandbox:e2e'}
_built = threading.Lock()
_images_ready = False


def docker(*args, check=True, timeout=600, env=None):
    return subprocess.run(['docker', *args], check=check, capture_output=True, text=True, timeout=timeout, env=env)


def build_images():
    """Builds the three test images once per run; later scenarios reuse them."""
    global _images_ready
    with _built:
        if _images_ready:
            return
        steps = [
            ['build', '-q', '-f', 'deploy/docker/Dockerfile', '--build-arg', 'GO_TAGS=sandboxfixture', '-t', IMAGES['base'], '.'],
            ['build', '-q', '-f', 'tests/docker/control.Dockerfile', '--build-arg', f'BASE={IMAGES["base"]}', '-t', IMAGES['control'], '.'],
            ['build', '-q', '-f', 'tests/docker/sandbox.Dockerfile', '-t', IMAGES['sandbox'], '.'],
        ]
        for step in steps:
            result = subprocess.run(['docker', *step], cwd=PROJECT, capture_output=True, text=True, timeout=1800)
            assert result.returncode == 0, f'docker {" ".join(step)} failed:\n{result.stderr[-4000:]}'
        _images_ready = True


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


class Stack:
    """One isolated compose project: the shipped compose file plus a test override."""

    def __init__(self, root, secrets_dir):
        self.root = root
        self.project = 'octomus-e2e-' + secrets.token_hex(4)
        self.port = free_port()
        self.secrets_dir = secrets_dir
        self.override = secrets_dir / 'compose.override.yaml'
        self.env = {**os.environ, 'OCTOMUS_GITHUB_REPO': 'fixture/project', 'OCTOMUS_PORT': str(self.port),
                    'DOCKER_GID': str(os.stat('/var/run/docker.sock').st_gid)}

    def volume(self, name):
        return f'{self.project}-{name}'

    def compose(self, *args, check=True, timeout=300):
        return docker('compose', '-p', self.project, '-f', str(COMPOSE), '-f', str(self.override), *args,
                      check=check, timeout=timeout, env=self.env)

    def prepare(self, *, own_remote=False):
        (self.root / 'control-bin').mkdir()
        for name in ['gh', 'git']:
            shutil.copy(self.root / 'bin' / name, self.root / 'control-bin' / name)
        # Containers run as uid 10001; this directory is test data only.
        subprocess.run(['chmod', '-R', 'a+rwX', str(self.root)], check=True)
        (self.secrets_dir / 'operator_token').write_text(TOKEN + '\n')
        (self.secrets_dir / 'github_token').write_text('fixture-github-token-not-a-credential\n')
        for path in self.secrets_dir.iterdir():
            path.chmod(0o644)
        for name in ['data', 'runner', 'tools']:
            docker('volume', 'create', self.volume(name))
        docker('volume', 'create', '--driver', 'local', '--opt', 'type=none', '--opt', f'device={self.root}',
               '--opt', 'o=bind', self.volume('fixture'))
        if own_remote:
            # Real release images keep Git's ownership check. Transfer only
            # this fixture remote, after the host-side permissions are set.
            docker('run', '--rm', '--network', 'none', '--user', '0',
                   '-v', f'{self.volume("fixture")}:{self.root}', '--entrypoint', '/bin/chown', IMAGES['control'],
                   '-R', '10001:10001', str(self.root / 'remote.git'))
        # The deployment's trusted checkout lives in the data volume; the fixture remote stands in for GitHub.
        docker('run', '--rm', '--network', 'none', '-v', f'{self.volume("data")}:/var/lib/octomus/data',
               '-v', f'{self.volume("fixture")}:{self.root}', '--entrypoint', '/usr/bin/git', IMAGES['control'],
               'clone', '-q', str(self.root / 'remote.git'), '/var/lib/octomus/data/checkout')
        root, project = str(self.root), self.project
        override = {
            'services': {
                'octomus': {'image': IMAGES['control'], 'environment': {
                    'OCTOMUS_FIXTURE': root,
                    'PATH': f'{root}/control-bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'},
                    'volumes': [f'fixture:{root}']},
                'sandboxd': {'image': IMAGES['control'], 'environment': {
                    'OCTOMUS_SANDBOX_IMAGE': IMAGES['sandbox'],
                    'OCTOMUS_SANDBOX_DATA_VOLUME': self.volume('data'),
                    'OCTOMUS_SANDBOX_RUNNER_VOLUME': self.volume('runner'),
                    'OCTOMUS_SANDBOX_TOOLS_VOLUME': self.volume('tools'),
                    'OCTOMUS_SANDBOX_RUNNER_NETWORK': f'{project}-runner',
                    'OCTOMUS_SANDBOX_VERIFY_NETWORK': f'{project}-verify',
                    'OCTOMUS_SANDBOX_INSTANCE': project,
                    'OCTOMUS_SANDBOX_MEMORY': '512m',
                    'OCTOMUS_SANDBOX_MAX': '16',
                    'OCTOMUS_SANDBOX_FIXTURE_VOLUME': self.volume('fixture'),
                    'OCTOMUS_SANDBOX_FIXTURE_PATH': root}},
                'egress': {'image': IMAGES['control']},
                'login-lease': {'image': IMAGES['control']},
                'login': {'image': IMAGES['sandbox']},
            },
            'secrets': {name: {'file': str(self.secrets_dir / name)} for name in ['operator_token', 'github_token']},
            'volumes': {name: {'name': self.volume(name), 'external': True} for name in ['data', 'runner', 'tools', 'fixture']},
            'networks': {'sandbox-runner': {'name': f'{project}-runner'}, 'sandbox-verify': {'name': f'{project}-verify'}},
        }
        self.override.write_text(json.dumps(override, indent=2))

    def up(self):
        self.compose('up', '-d', '--no-build', timeout=600)
        self.wait_healthy()

    def wait_healthy(self):
        def healthy():
            with local_urlopen(f'http://127.0.0.1:{self.port}/healthz', timeout=2) as response:
                return json.load(response)['ok']
        if not poll(healthy, 120, interval=0.5):
            raise AssertionError(f'{self.project} never became healthy\n{self.logs()}')
        if not poll(lambda: self.request('/state')['sandbox']['healthy'], 120, interval=0.5):
            raise AssertionError(f'{self.project} broker never answered\n{self.logs()}')

    def logs(self):
        return self.compose('logs', '--no-color', '--tail', '120', check=False).stdout

    def request(self, path, method='GET', value=None, timeout=120):
        request = urllib.request.Request(f'http://127.0.0.1:{self.port}/api{path}', method=method,
                                         headers={'Authorization': f'Bearer {TOKEN}', 'Content-Type': 'application/json'},
                                         data=json.dumps(value or {}).encode() if method != 'GET' else None)
        try:
            with local_urlopen(request, timeout=timeout) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise AssertionError(f'{method} {path}: HTTP {error.code} {error.read()[:2000].decode(errors="replace")}') from None

    def sandboxes(self):
        return docker('ps', '-aq', '--filter', f'label=octomus.sandbox.instance={self.project}').stdout.split()

    def configure(self, commands=None):
        view = self.request('/config')
        config = view['config']
        assert config['repository'] == '/var/lib/octomus/data/checkout' and config['github_repo'] == 'fixture/project', config
        config.update(**routes(), verification_commands=commands or [FEATURE_CHECK], session_timeout_seconds=60,
                      command_timeout_seconds=60, task_timeout_seconds=600, cycle_interval_seconds=3600)
        self.request('/config', 'PUT', {'expected_revision': view['revision'], 'config': config})

    def teardown(self):
        # Every profile: `compose run login` leaves the exited login-lease container, which holds two project volumes.
        self.compose('--profile', '*', 'down', '--volumes', '--remove-orphans', '--timeout', '30', check=False)
        for container in self.sandboxes():
            docker('rm', '-f', container, check=False)
        # Sandboxes wrote fixture files as uid 10001; remove them with a matching owner before the directory goes.
        docker('run', '--rm', '--network', 'none', '--user', '0', '-v', f'{self.volume("fixture")}:/fixture',
               '--entrypoint', 'sh', IMAGES['sandbox'], '-c', 'rm -rf /fixture/* /fixture/.[!.]*', check=False)
        for name in ['data', 'runner', 'tools', 'fixture']:
            docker('volume', 'rm', '-f', self.volume(name), check=False)


@contextlib.contextmanager
def stack(prefix, prepare=None):
    build_images()
    with tempfile.TemporaryDirectory(prefix=prefix) as tmp, tempfile.TemporaryDirectory(prefix='octomus-e2e-secrets-') as secrets_dir:
        root = Path(tmp)
        setup(root)
        if prepare:
            prepare(root)
        s = Stack(root, Path(secrets_dir))
        try:
            s.prepare()
            s.up()
            yield s
        except BaseException:
            print(s.logs(), file=sys.stderr)
            raise
        finally:
            s.teardown()


def self_test_scenario():
    with stack('octomus-e2e-selftest-') as s:
        result = s.request('/sandbox/self-test', 'POST')
        failed = [check for check in result['checks'] if not check['passed']]
        assert result['passed'] and not failed and len(result['checks']) == 11, json.dumps(result, indent=2)
        gateway = next(check for check in result['checks'] if check['id'] == 'egress_gateway')
        assert 'refused example.com:443' in gateway['detail'], gateway
        posture = s.request('/state')['sandbox']
        assert posture['mode'] == 'docker' and posture['healthy'] and posture['pinned_repository'] == 'fixture/project', posture
        assert posture['broker']['image'] == IMAGES['sandbox'] and posture['broker']['egress'], posture
        assert posture['self_test']['passed'], posture
        egress = s.compose('logs', '--no-color', 'egress').stdout
        assert '"decision":"denied"' in egress and '"host":"example.com"' in egress, egress[-2000:]
        assert not s.sandboxes(), 'the probe sandbox was not removed'
        print('PASS self-test: containment proven from inside a real sandbox, denials logged by the gateway')


# A request to a host outside every allowlist: the gateway refuses it and the broker records the refusal.
UNLISTED = "python3 -c \"import urllib.request\ntry: urllib.request.urlopen('https://example.com/', timeout=10)\nexcept Exception: pass\""


def delivery_scenario():
    with stack('octomus-e2e-delivery-') as s:
        s.configure([UNLISTED, FEATURE_CHECK])
        diagnostic = s.request('/doctor', 'POST')
        assert diagnostic['ok'] and diagnostic['sandbox']['mode'] == 'docker', diagnostic
        assert diagnostic['sandbox']['self_test']['passed'], diagnostic['sandbox']
        assert diagnostic['codex_version'] == 'codex-cli 0.153.4', diagnostic
        s.request('/control/cycle', 'POST')

        def published():
            state = s.request('/state')
            assert not state['control']['error'], state['control']['error']
            tasks = state['tasks']
            return tasks[0] if tasks and tasks[0]['status'] == 'published' and state['control']['paused'] else None
        task = poll(published, 300, interval=1)
        assert task, f'task never published\n{json.dumps(s.request("/state")["tasks"], indent=2)}'
        prs = json.loads((s.root / 'prs.json').read_text())
        assert len(prs) == 1 and prs[0]['head']['ref'].startswith('octomus/'), prs
        protocol = [json.loads(line) for line in (s.root / 'protocol.jsonl').read_text().splitlines()]
        assert protocol and all(entry['cwd'].startswith('/var/lib/octomus/data/') for entry in protocol), protocol
        assert all(entry['sandbox'] == {'type': 'dangerFullAccess'} for entry in protocol), protocol
        # Runners exist only in the sandbox image; the control plane cannot have run one itself.
        assert s.compose('exec', '-T', 'octomus', 'sh', '-c', 'command -v codex', check=False).returncode != 0
        detail = s.request(f'/tasks/{task["id"]}')
        for session in detail['sessions']:
            record = session['sandbox']
            assert record and record['runs'] >= 1 and record['image_id'].startswith('sha256:'), session
        records = [v['sandbox'] for v in detail['verification']]
        assert all(record and record['runs'] == 1 for record in records), detail['verification']
        unlisted = [r for v, r in zip(detail['verification'], records) if v['command'] == UNLISTED]
        assert unlisted and all(r['egress']['denied'].get('example.com:443', 0) >= 1 for r in unlisted), unlisted
        layout = s.compose('exec', '-T', 'octomus', 'ls', str(Path(detail['workspace']).parent)).stdout.split()
        assert {'repo.git', 'workspace', 'home'} <= set(layout), layout
        assert poll(lambda: not s.sandboxes(), 30), f'sandboxes left behind: {s.sandboxes()}'
        print('PASS delivery: planning, execution, review and verification ran in sandboxes; the control plane published')


SCENARIOS = [
    ('self-test', self_test_scenario),
    ('delivery', delivery_scenario),
]

if __name__ == '__main__':
    run_selected('sandbox', SCENARIOS, sys.argv[1:])
