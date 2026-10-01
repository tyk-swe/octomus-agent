#!/usr/bin/env python3
"""Exercise deployment setup with offline Docker and ownership fixtures, and the shipped compose file's rendering."""
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile

PROJECT = Path(__file__).resolve().parents[1]
COMPOSE = PROJECT / 'deploy/docker/compose.yaml'
DOCKER = '''#!/bin/sh
printf '%s\\n' "$*" >> "$FIXTURE_DOCKER_LOG"
case "$*" in
  'compose version') exit 0 ;;
  'version --format {{.Server.Version}}') printf '28.0.0\\n' ;;
  'compose build octomus sandbox-image'|run*) exit 0 ;;
  'compose up -d') exit "${FIXTURE_UP_STATUS:-0}" ;;
  *) exit 1 ;;
esac
'''
STAT = '''#!/bin/sh
case "$*" in
  '-c %g '*) printf '1000\\n' ;;
  '-c %u secrets/operator_token') printf '%s\\n' "$FIXTURE_OPERATOR_UID" ;;
  '-c %u secrets/github_token') printf '%s\\n' "$FIXTURE_GITHUB_UID" ;;
  *) exit 1 ;;
esac
'''
# Records what the script asks of the terminal; the fixture's input is a pipe, never a terminal.
STTY = '''#!/bin/sh
printf '%s\\n' "$*" >> "$FIXTURE_STTY_LOG"
'''


class Setup:
    """A copy of setup.sh in a scratch deploy directory, with fake docker, stat and stty peers."""

    def __init__(self, root, env_file='OCTOMUS_GITHUB_REPO=fixture/repo\n'):
        self.root = root
        peers = root / 'bin'
        peers.mkdir()
        for name, text in [('docker', DOCKER), ('stat', STAT), ('stty', STTY)]:
            (peers / name).write_text(text)
            (peers / name).chmod(0o755)
        shutil.copy(PROJECT / 'deploy/docker/env.example', root / 'env.example')
        (root / '.env').write_text(env_file)
        self.secrets = root / 'secrets'
        self.secrets.mkdir(mode=0o700)
        self.docker_log = root / 'docker.log'
        self.stty_log = root / 'stty.log'
        self.env = {key: value for key, value in os.environ.items() if not key.startswith('OCTOMUS_')}
        self.env.update(PATH=str(peers) + os.pathsep + os.environ['PATH'], FIXTURE_DOCKER_LOG=str(self.docker_log),
                        FIXTURE_STTY_LOG=str(self.stty_log), FIXTURE_OPERATOR_UID='10001', FIXTURE_GITHUB_UID='10001')

    def secret(self, name, value):
        (self.secrets / name).write_text(value + '\n')
        (self.secrets / name).chmod(0o600)

    def run(self, stdin='fixture-github-token\n', **env):
        (self.root / 'docker.sock').unlink(missing_ok=True)
        with socket.socket(socket.AF_UNIX) as daemon:
            daemon.bind(str(self.root / 'docker.sock'))
            # Give the copied setup a local socket; everything else runs unchanged.
            setup = self.root / 'setup.sh'
            setup.write_text((PROJECT / 'deploy/docker/setup.sh').read_text().replace(
                'socket=/var/run/docker.sock', f'socket={self.root / "docker.sock"}'))
            return subprocess.run(['sh', str(setup)], input=stdin, env={**self.env, **env},
                                  capture_output=True, text=True, timeout=10)

    def calls(self):
        return self.docker_log.read_text().splitlines() if self.docker_log.exists() else []


def setup_secret_ownership(operator_uid, github_uid, recreate_github=False):
    with tempfile.TemporaryDirectory(prefix='octomus-setup-') as directory:
        s = Setup(Path(directory))
        s.secret('operator_token', 'fixture-operator-token')
        if not recreate_github:
            s.secret('github_token', 'fixture-github-token')
        result = s.run(FIXTURE_OPERATOR_UID=str(operator_uid), FIXTURE_GITHUB_UID=str(github_uid))
        assert result.returncode == 0, result.stderr
        calls = s.calls()
        repairs = [call for call in calls if call.startswith('run ')]
        if operator_uid != 10001 or github_uid != 10001:
            assert len(repairs) == 1, calls
            assert repairs[0].endswith('10001:10001 /secrets/operator_token /secrets/github_token'), repairs
            assert calls.index(repairs[0]) < calls.index('compose up -d'), calls
        else:
            assert not repairs, calls
        # The base sandbox image builds under its own tag, never over OCTOMUS_SANDBOX_IMAGE.
        assert 'compose build octomus sandbox-image' in calls, calls
        for name in ['operator_token', 'github_token']:
            assert (s.secrets / name).stat().st_mode & 0o777 == 0o600, name


def render(env_file):
    """The compose file as Compose resolves it with this .env, every profile enabled."""
    with tempfile.TemporaryDirectory(prefix='octomus-compose-') as directory:
        path = Path(directory) / '.env'
        path.write_text(env_file)
        env = {key: value for key, value in os.environ.items() if not key.startswith(('OCTOMUS_', 'DOCKER_GID'))}
        result = subprocess.run(['docker', 'compose', '-f', str(COMPOSE), '--env-file', str(path), '--profile', '*',
                                 'config', '--format', 'json'], env=env, capture_output=True, text=True, timeout=60)
        assert result.returncode == 0, result.stderr
        return json.loads(result.stdout)


def compose_contract():
    if subprocess.run(['docker', 'compose', 'version'], capture_output=True).returncode != 0:
        print('SKIP compose contract: docker compose is not installed')
        return
    example = (PROJECT / 'deploy/docker/env.example').read_text()
    required = 'OCTOMUS_GITHUB_REPO=owner/repository\nDOCKER_GID=999\n'
    config = render(example + required)
    services = config['services']
    for name, service in services.items():
        assert service.get('logging', {}).get('options', {}).get('max-size'), f'{name} keeps an unrotated log'
    assert services['egress'].get('mem_limit') and services['egress'].get('pids_limit'), services['egress']

    # Building never retags the operator's derived sandbox image.
    login = services['login']
    assert 'build' not in login, login
    assert services['sandbox-image']['image'] == 'octomus-sandbox:local', services['sandbox-image']
    derived = render(example + required + 'OCTOMUS_SANDBOX_IMAGE=my-sandbox:go\n')['services']
    assert derived['login']['image'] == 'my-sandbox:go' and derived['sandbox-image']['image'] == 'octomus-sandbox:local'

    # An empty model allowlist stays empty; only an absent one gets the Codex default.
    hosts = lambda env_file: render(env_file)['services']['egress']['environment']['OCTOMUS_EGRESS_MODEL_HOSTS']
    assert hosts(required + 'OCTOMUS_EGRESS_MODEL_HOSTS=\n') == '', 'a blank model allowlist was widened'
    assert hosts(required) == 'chatgpt.com,auth.openai.com,api.openai.com'
    assert hosts(required + 'OCTOMUS_EGRESS_MODEL_HOSTS=models.opencode.ai\n') == 'models.opencode.ai'


def main():
    setup_secret_ownership(10001, 10001)
    setup_secret_ownership(1000, 10001)
    setup_secret_ownership(10001, 1000, recreate_github=True)
    compose_contract()
    print('PASS Docker setup: secret ownership and the compose contract')


if __name__ == '__main__':
    main()
