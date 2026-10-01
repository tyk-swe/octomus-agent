#!/usr/bin/env python3
"""Keep fixture Git operations independent of the developer's Git environment."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile

from harness import fixture_service, git


def check_fixture():
    before = dict(os.environ)
    with fixture_service('octomus-git-environment-', start=False) as (root, service):
        checkout = root / 'checkout'
        assert git('log', '-1', '--format=%s', cwd=checkout) == 'Initial fixture'
        assert git('config', '--local', 'user.name', cwd=checkout) == 'Fixture'
        assert git('config', '--local', 'user.email', cwd=checkout) == 'fixture@example.com'

        # The service's Git and fixture runners inherit this environment too.
        # Exercise their commit/push path, rather than checking environment keys.
        def child_git(*args):
            return subprocess.check_output(['/usr/bin/git', *args], cwd=checkout,
                                           env=service.env, stderr=subprocess.PIPE,
                                           text=True).strip()

        (checkout / 'feature.txt').write_text('fixed\n')
        child_git('add', 'feature.txt')
        child_git('commit', '-m', 'Fixture child commit')
        child_git('push', 'origin', 'main')
        assert git('log', '-1', '--format=%s', cwd=root / 'remote.git') == 'Fixture child commit'
        assert child_git('config', '--local', 'user.name') == 'Fixture'
    assert dict(os.environ) == before, 'fixture setup changed the parent environment'

    # Scenarios can still deliberately configure their own child processes.
    overrides = {'GIT_CONFIG_COUNT': '1', 'GIT_CONFIG_KEY_0': 'user.name',
                 'GIT_CONFIG_VALUE_0': 'Explicit fixture identity'}
    with fixture_service('octomus-git-override-', env=overrides, start=False) as (root, service):
        identity = subprocess.check_output(['/usr/bin/git', 'config', 'user.name'],
                                           cwd=root / 'checkout', env=service.env, text=True)
        assert identity.strip() == 'Explicit fixture identity', identity
    assert dict(os.environ) == before, 'fixture override changed the parent environment'


def main():
    with tempfile.TemporaryDirectory(prefix='octomus-ambient-git-') as directory:
        root = Path(directory)
        home = root / 'home'
        home.mkdir()
        xdg = root / 'xdg'
        xdg.mkdir()
        config = root / 'ambient.gitconfig'
        config.write_text('[commit]\n\tgpgSign = true\n[gpg]\n\tprogram = /no-fixture-signing-program\n')
        outside = root / 'outside'
        outside.write_text('This file is outside every fixture; do not change it.\n')
        original = outside.read_bytes()
        env = {key: value for key, value in os.environ.items() if not key.startswith('GIT_')}
        env.update(HOME=str(home), XDG_CONFIG_HOME=str(xdg), GIT_CONFIG_NOSYSTEM='1')
        cases = [
            ('global configuration', {'GIT_CONFIG_GLOBAL': str(config)}),
            ('system configuration', {'GIT_CONFIG_SYSTEM': str(config), 'GIT_CONFIG_NOSYSTEM': '0'}),
            ('inline configuration', {'GIT_CONFIG_COUNT': '1', 'GIT_CONFIG_KEY_0': 'commit.gpgSign', 'GIT_CONFIG_VALUE_0': 'true'}),
            ('shell configuration', {'GIT_CONFIG_PARAMETERS': "'commit.gpgSign=true'"}),
            ('configuration file', {'GIT_CONFIG': str(outside)}),
            ('repository directory', {'GIT_DIR': str(outside)}),
            ('work tree', {'GIT_WORK_TREE': str(outside)}),
            ('index file', {'GIT_INDEX_FILE': str(outside)}),
        ]
        # Each case runs in a child so parallel service scenarios never see a
        # temporarily replaced process-wide environment.
        for label, overrides in cases:
            result = subprocess.run([sys.executable, str(Path(__file__).resolve()), '--check'],
                                    env={**env, **overrides}, text=True, capture_output=True, timeout=30)
            assert result.returncode == 0, f'{label}:\n{result.stdout}\n{result.stderr}'
            assert outside.read_bytes() == original, f'{label} changed a file outside the fixture'
            print(f'PASS fixture Git environment: {label}', flush=True)


if __name__ == '__main__':
    if sys.argv[1:] == ['--check']:
        check_fixture()
    else:
        main()
