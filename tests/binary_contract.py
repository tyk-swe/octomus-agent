#!/usr/bin/env python3
"""CLI startup and embedded-dashboard contracts for the Go executable."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

PROJECT = Path(__file__).resolve().parents[1]
BINARY = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(PROJECT / 'bin/octomus-agent'))).resolve()


def run(binary, args, cwd, environment=None):
    env = {k: v for k, v in os.environ.items() if not k.startswith('OCTOMUS_')}
    env.update(environment or {})
    return subprocess.run([str(binary), *args], cwd=cwd, env=env,
                          text=True, capture_output=True, timeout=15)


def service_startup():
    with tempfile.TemporaryDirectory(prefix='octomus-binary-service-') as directory:
        root = Path(directory)
        # Startup initializes state before doctor/config/assets checks.
        for args, expected in [
            ([], 'Dashboard override missing 200.html'),
            (['--doctor'], 'model and effort'),
            (['--doctor', '--audit'], 'model and effort'),
        ]:
            result = run(BINARY, ['--data-dir', str(root / 'state'), *args], root,
                         {'OCTOMUS_TOKEN': 'fixture-token-not-an-operator-token', 'OCTOMUS_ASSETS': '/nonexistent'})
            assert result.returncode == 1 and not result.stdout, (args, result)
            assert expected in result.stderr, (args, result.stderr)
            assert (root / 'state' / 'state.db').exists(), f'Service startup skipped the store: {args}'
            shutil.rmtree(root / 'state')
        # Without an operator token the service refuses before the assets check.
        result = run(BINARY, ['--data-dir', str(root / 'state')], root,
                     {'OCTOMUS_ASSETS': '/nonexistent'})
        assert result.returncode == 1 and 'OCTOMUS_TOKEN' in result.stderr, result.stderr
        shutil.rmtree(root / 'state')
        # Read-only exports: missing state is an explicit failure that never
        # creates the data directory or takes the service lock.
        for args in [['--usage-report'], ['--export-run', 'synthetic-cycle']]:
            result = run(BINARY, ['--data-dir', str(root / 'state'), *args], root,
                         {'OCTOMUS_TOKEN': 'fixture-token-not-an-operator-token', 'OCTOMUS_ASSETS': '/nonexistent'})
            assert result.returncode == 1 and not result.stdout, (args, result)
            assert 'state database' in result.stderr, (args, result.stderr)
            assert not list(root.iterdir()), f'Read-only export wrote state: {args}'


def embedding_contracts():
    # The shipped executable includes both the SPA entrypoint and JS assets.
    binary = BINARY.read_bytes()
    assets = [PROJECT / 'web/build/200.html', *sorted((PROJECT / 'web/build/_app/immutable/entry').glob('*.js'))]
    assert len(assets) > 1
    for asset in assets:
        assert asset.read_bytes() in binary, f'executable omitted {asset.name}'

    # Work in an isolated Go package instead of moving the shared web/build while
    # other tests may read it. Use the actual checked-in embed declaration.
    with tempfile.TemporaryDirectory(prefix='octomus-binary-embed-') as directory:
        root = Path(directory)
        shutil.copy2(PROJECT / 'web/embed.go', root / 'embed.go')
        (root / 'go.mod').write_text('module embedded-contract\n\ngo 1.27.1\n')
        for stage in ['absent', 'entrypoint-only', 'complete', 'bundle-without-entrypoint']:
            if stage == 'entrypoint-only':
                (root / 'build').mkdir()
                shutil.copy2(PROJECT / 'web/build/200.html', root / 'build/200.html')
            elif stage == 'complete':
                shutil.rmtree(root / 'build')
                shutil.copytree(PROJECT / 'web/build', root / 'build')
            elif stage == 'bundle-without-entrypoint':
                (root / 'build/200.html').unlink()
            result = subprocess.run(['go', 'build', '.'], cwd=root, text=True,
                                    capture_output=True, timeout=60)
            if stage == 'complete':
                assert result.returncode == 0, result.stderr
            else:
                assert result.returncode != 0 and 'no matching files found' in result.stderr, (stage, result.stderr)


if __name__ == '__main__':
    service_startup()
    embedding_contracts()
    print('Go binary contracts passed: startup order, embedded dashboard and missing-asset build failures.')
