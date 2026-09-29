#!/usr/bin/env python3
"""Exercise deployment setup with offline Docker and ownership fixtures."""
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile

PROJECT = Path(__file__).resolve().parents[1]


def setup_secret_ownership(operator_uid, github_uid, recreate_github=False):
    with tempfile.TemporaryDirectory(prefix='octomus-setup-') as directory:
        root = Path(directory)
        peers = root / 'bin'
        peers.mkdir()
        docker_log = root / 'docker.log'
        docker = peers / 'docker'
        docker.write_text('''#!/bin/sh
printf '%s\\n' "$*" >> "$FIXTURE_DOCKER_LOG"
case "$*" in
  'compose version') exit 0 ;;
  'version --format {{.Server.Version}}') printf '28.0.0\\n' ;;
  'compose build octomus login'|'compose up -d'|run*) exit 0 ;;
  *) exit 1 ;;
esac
''')
        docker.chmod(0o755)
        stat = peers / 'stat'
        stat.write_text('''#!/bin/sh
case "$*" in
  '-c %g '*) printf '1000\\n' ;;
  '-c %u secrets/operator_token') printf '%s\\n' "$FIXTURE_OPERATOR_UID" ;;
  '-c %u secrets/github_token') printf '%s\\n' "$FIXTURE_GITHUB_UID" ;;
  *) exit 1 ;;
esac
''')
        stat.chmod(0o755)
        shutil.copy(PROJECT / 'deploy/docker/env.example', root / 'env.example')
        (root / '.env').write_text('OCTOMUS_GITHUB_REPO=fixture/repo\n')
        secrets = root / 'secrets'
        secrets.mkdir(mode=0o700)
        (secrets / 'operator_token').write_text('fixture-operator-token\n')
        (secrets / 'operator_token').chmod(0o600)
        if not recreate_github:
            (secrets / 'github_token').write_text('fixture-github-token\n')
            (secrets / 'github_token').chmod(0o600)
        with socket.socket(socket.AF_UNIX) as daemon:
            daemon.bind(str(root / 'docker.sock'))
            # Give the copied setup a local socket; everything else runs unchanged.
            setup = root / 'setup.sh'
            setup.write_text((PROJECT / 'deploy/docker/setup.sh').read_text().replace(
                'socket=/var/run/docker.sock', f'socket={root / "docker.sock"}'))
            env = dict(os.environ, PATH=str(peers) + os.pathsep + os.environ['PATH'],
                       FIXTURE_DOCKER_LOG=str(docker_log), FIXTURE_OPERATOR_UID=str(operator_uid),
                       FIXTURE_GITHUB_UID=str(github_uid))
            result = subprocess.run(['sh', str(setup)], input='fixture-github-token\n',
                                    env=env, capture_output=True, text=True, timeout=10)
            assert result.returncode == 0, result.stderr
        calls = docker_log.read_text().splitlines()
        repairs = [call for call in calls if call.startswith('run ')]
        if operator_uid != 10001 or github_uid != 10001:
            assert len(repairs) == 1, calls
            assert repairs[0].endswith('10001:10001 /secrets/operator_token /secrets/github_token'), repairs
            assert calls.index(repairs[0]) < calls.index('compose up -d'), calls
        else:
            assert not repairs, calls
        for name in ['operator_token', 'github_token']:
            assert (secrets / name).stat().st_mode & 0o777 == 0o600, name


def main():
    setup_secret_ownership(10001, 10001)
    setup_secret_ownership(1000, 10001)
    setup_secret_ownership(10001, 1000, recreate_github=True)
    print('PASS Docker setup: either secret ownership is repaired before startup with mode 0600 preserved')


if __name__ == '__main__':
    main()
