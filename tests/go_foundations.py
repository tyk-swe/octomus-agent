#!/usr/bin/env python3
"""M1 CLI/embedding contracts, entirely synthetic and selectable by executable.

Run with OCTOMUS_TEST_BINARY=...; the default remains the Rust reference.
This never opens the operator's data directory or contacts an external peer.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

PROJECT = Path(__file__).resolve().parents[1]
BINARY = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(PROJECT / 'target/debug/octomus-agent'))).resolve()
FIXTURES = PROJECT / 'tests/fixtures/compatibility'


def run(binary, args, cwd, environment=None):
    env = {k: v for k, v in os.environ.items() if not k.startswith('OCTOMUS_')}
    env.update(environment or {})
    return subprocess.run([str(binary), *args], cwd=cwd, env=env,
                          text=True, capture_output=True, timeout=15)


def cli_contracts():
    corpus = json.loads((FIXTURES / 'cli.json').read_text())
    with tempfile.TemporaryDirectory(prefix='octomus-foundations-cli-') as directory:
        root = Path(directory)
        executable = root / 'relocated-agent'
        shutil.copy2(BINARY, executable)
        working = root / 'unrelated-cwd'
        working.mkdir()
        for case in corpus['cases']:
            result = run(executable, case['args'], working, case['env'])
            expected = case['expected']
            assert result.returncode == expected['code'], (case['name'], result.returncode, expected['code'], result.stderr)
            if expected['code']:
                assert not result.stdout and result.stderr, case['name']
            else:
                assert not result.stderr, (case['name'], result.stderr)
                if any(flag in case['args'] for flag in ('--help', '-h')):
                    for flag in ['data-dir', 'listen', 'assets', 'print-config', 'doctor', 'audit', 'usage-report', 'export-run', 'help', 'version']:
                        assert f'--{flag}' in result.stdout, flag
                else:
                    assert result.stdout == expected['stdout'], case['name']
            assert not list(working.iterdir()), f"CLI case wrote state: {case['name']}"
    return len(corpus['cases'])


def unavailable_commands():
    with tempfile.TemporaryDirectory(prefix='octomus-foundations-unavailable-') as directory:
        root = Path(directory)
        for args in [[], ['--doctor'], ['--doctor', '--audit'], ['--usage-report'], ['--export-run', 'synthetic-cycle']]:
            result = run(BINARY, ['--data-dir', str(root / 'state'), *args], root,
                         {'OCTOMUS_TOKEN': 'fixture-token-not-an-operator-token', 'OCTOMUS_ASSETS': '/nonexistent'})
            assert result.returncode == 1 and not result.stdout, (args, result)
            assert 'not implemented in the Go executable' in result.stderr, (args, result.stderr)
            assert not list(root.iterdir()), f'Unavailable command wrote state: {args}'


def embedding_contracts(go_binary=False):
    if go_binary:
        # Test the shipped executable, not only a separately linked Go test
        # package: both the SPA entrypoint and underscore-prefixed JS are there.
        binary = BINARY.read_bytes()
        assets = [PROJECT / 'web/build/200.html', *sorted((PROJECT / 'web/build/_app/immutable/entry').glob('*.js'))]
        assert len(assets) > 1
        for asset in assets:
            assert asset.read_bytes() in binary, f'executable omitted {asset.name}'

    # Work in an isolated Go package instead of moving the shared web/build while
    # other tests may read it. Use the actual checked-in embed declaration.
    with tempfile.TemporaryDirectory(prefix='octomus-foundations-embed-') as directory:
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
    parser = argparse.ArgumentParser()
    parser.add_argument('--go-m1', action='store_true', help='also assert that later Go commands fail without writing state')
    args = parser.parse_args()
    count = cli_contracts()
    if args.go_m1:
        unavailable_commands()
    embedding_contracts(args.go_m1)
    unavailable = ', explicit unavailable commands' if args.go_m1 else ''
    print(f'M1 foundations passed: {count} frozen CLI cases, relocated executable{unavailable}, real dashboard and missing-asset build failures.')
