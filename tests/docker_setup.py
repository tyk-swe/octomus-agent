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


def setup_keeps_a_new_token_when_startup_fails():
    with tempfile.TemporaryDirectory(prefix='octomus-setup-') as directory:
        s = Setup(Path(directory))
        s.secret('github_token', 'fixture-github-token')
        failed = s.run(FIXTURE_UP_STATUS='1')
        assert failed.returncode != 0, failed.stdout
        token = (s.secrets / 'operator_token').read_text().strip()
        assert len(token) == 64 and token in failed.stdout, (failed.stdout, failed.stderr)
        # A later run cannot show a token it did not make, but says where it is.
        again = s.run()
        assert again.returncode == 0, again.stderr
        assert token not in again.stdout, again.stdout
        assert f'sudo cat {s.secrets / "operator_token"}' in again.stdout, again.stdout


def setup_asks_for_github_first_and_restores_echo():
    with tempfile.TemporaryDirectory(prefix='octomus-setup-') as directory:
        s = Setup(Path(directory))
        result = s.run(stdin='')
        assert result.returncode != 0 and 'A GitHub token is required' in result.stderr, result.stderr
        assert s.stty_log.read_text().splitlines() == ['-echo', 'echo'], s.stty_log.read_text()
        assert not (s.secrets / 'operator_token').exists(), 'an abandoned setup left an operator token nobody saw'
        assert 'compose up -d' not in s.calls(), s.calls()


def setup_tunnel_uses_the_published_port():
    with tempfile.TemporaryDirectory(prefix='octomus-setup-') as directory:
        s = Setup(Path(directory), env_file='OCTOMUS_GITHUB_REPO=fixture/repo\nOCTOMUS_PORT="4300"\n')
        s.secret('operator_token', 'fixture-operator-token')
        s.secret('github_token', 'fixture-github-token')
        result = s.run()
        assert result.returncode == 0, result.stderr
        assert 'ssh -N -L 4200:127.0.0.1:4300 ' in result.stdout, result.stdout
        # Compose prefers the invoking shell's value over .env, and so does the printed tunnel.
        result = s.run(OCTOMUS_PORT='4400')
        assert 'ssh -N -L 4200:127.0.0.1:4400 ' in result.stdout, result.stdout


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


def login_lease_runs_the_agent_mode(services):
    """login-lease runs the agent's --login-lease mode (cmd/octomus-agent, tested in Go) with no network, as the image's
    unprivileged user: it writes the lease the broker writes, where the gateway reads it, and hands the login a proxy
    URL for the gateway sandboxes use."""
    service, login = services['login-lease'], services['login']
    assert service.get('entrypoint') is None and service['command'] == ['--login-lease'], service
    assert service['network_mode'] == 'none' and service['restart'] == 'no', service
    assert service['read_only'] and service['cap_drop'] == ['ALL'] and service['user'] == '10001:10001', service
    mounts = {mount['source']: mount['target'] for mount in service['volumes']}
    assert mounts == {'egress-state': '/run/octomus-egress', 'login-proxy': '/run/octomus-login'}, mounts
    environment, broker = service['environment'], services['sandboxd']['environment']
    assert environment['OCTOMUS_EGRESS_LEASES'] == mounts['egress-state'] == broker['OCTOMUS_EGRESS_LEASES'], environment
    assert environment['OCTOMUS_EGRESS_PROXY'] == broker['OCTOMUS_EGRESS_PROXY'], environment
    proxy = environment['OCTOMUS_LOGIN_PROXY_FILE']
    assert proxy == mounts['login-proxy'] + '/proxy', environment
    read = [mount for mount in login['volumes'] if mount['source'] == 'login-proxy']
    assert len(read) == 1 and read[0]['target'] == mounts['login-proxy'] and read[0]['read_only'], read
    assert f'$(cat {proxy})' in login['entrypoint'][2].replace('$$', '$'), login['entrypoint']


def compose_contract():
    """Returns whether the contract ran: it needs the docker compose plugin to render the file."""
    if subprocess.run(['docker', 'compose', 'version'], capture_output=True).returncode != 0:
        print('SKIP compose contract: docker compose is not installed')
        return False
    example = (PROJECT / 'deploy/docker/env.example').read_text()
    required = 'OCTOMUS_GITHUB_REPO=owner/repository\nDOCKER_GID=999\n'
    config = render(example + required)
    services = config['services']
    for name, service in services.items():
        assert service.get('logging', {}).get('options', {}).get('max-size'), f'{name} keeps an unrotated log'
    assert services['egress'].get('mem_limit') and services['egress'].get('pids_limit'), services['egress']

    # Logins run runner programs against state runner sandboxes can write; they get no more reach than a sandbox.
    networks = config['networks']
    login = services['login']
    assert list(login['networks']) == ['sandbox-runner'] and networks['sandbox-runner']['internal'], login['networks']
    assert login['depends_on']['login-lease']['condition'] == 'service_completed_successfully', login['depends_on']
    mounted = {mount.get('volume', {}).get('subpath') for mount in login['volumes'] if mount['source'] == 'runner'}
    assert mounted == {'codex', 'opencode/data'}, mounted
    assert all(mount['source'] != 'egress-state' for mount in login['volumes']), login['volumes']
    assert login['read_only'] and login['cap_drop'] == ['ALL'], login
    login_lease_runs_the_agent_mode(services)
    for name, service in services.items():
        if name != 'egress':
            assert 'egress-out' not in service.get('networks', {}), f'{name} reaches the internet directly'

    # Building never retags the operator's derived sandbox image, and a missing image is never pulled.
    assert 'build' not in login, login
    for name in ['login', 'login-lease']:
        assert services[name].get('pull_policy') == 'never', f'{name} would pull a missing image'
    assert services['sandbox-image']['image'] == 'octomus-sandbox:local', services['sandbox-image']
    derived = render(example + required + 'OCTOMUS_SANDBOX_IMAGE=my-sandbox:go\n')['services']
    assert derived['login']['image'] == 'my-sandbox:go' and derived['sandbox-image']['image'] == 'octomus-sandbox:local'

    # An empty model allowlist stays empty; only an absent one gets the Codex default.
    hosts = lambda env_file: render(env_file)['services']['egress']['environment']['OCTOMUS_EGRESS_MODEL_HOSTS']
    assert hosts(required + 'OCTOMUS_EGRESS_MODEL_HOSTS=\n') == '', 'a blank model allowlist was widened'
    assert hosts(required) == 'chatgpt.com,auth.openai.com,api.openai.com'
    assert hosts(required + 'OCTOMUS_EGRESS_MODEL_HOSTS=models.opencode.ai\n') == 'models.opencode.ai'
    return True


def main():
    setup_secret_ownership(10001, 10001)
    setup_secret_ownership(1000, 10001)
    setup_secret_ownership(10001, 1000, recreate_github=True)
    setup_keeps_a_new_token_when_startup_fails()
    setup_asks_for_github_first_and_restores_echo()
    setup_tunnel_uses_the_published_port()
    checked = 'secret ownership, token display, terminal echo, tunnel port'
    if compose_contract():
        checked += ' and the compose contract'
    print(f'PASS Docker setup: {checked}')


if __name__ == '__main__':
    main()
