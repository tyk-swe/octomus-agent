#!/usr/bin/env python3
"""Offline distribution tests: real executable/HTTP, local release and curl fixtures."""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import tarfile
import tempfile
import urllib.error
import urllib.request

from e2e import poll

PROJECT = Path(__file__).resolve().parents[1]
TOKEN = 'distribution-fixture-token-at-least-32-characters'


def smoke(binary):
    with tempfile.TemporaryDirectory(prefix='octomus-distribution-') as directory:
        root = Path(directory)
        # Only the executable is copied; there is no web directory or Node dependency.
        executable = root / 'octomus-agent'
        shutil.copy(binary, executable)
        (root / 'empty-bin').mkdir()
        # Like binary_contract.run: no ambient OCTOMUS_* setting (such as a real
        # webhook URL) reaches the packaged service.
        env = {k: v for k, v in os.environ.items() if not k.startswith('OCTOMUS_')}
        env.update(OCTOMUS_TOKEN=TOKEN, PATH=str(root / 'empty-bin'))
        subprocess.run([str(executable), '--version'], cwd=root, env=env, check=True, timeout=15)
        for listen in ['127.0.0.1', '0.0.0.0']:
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', 0))
                port = sock.getsockname()[1]
            with (root / 'service.log').open('w+') as log:
                process = subprocess.Popen([str(executable), '--listen', f'{listen}:{port}'], cwd=root, env=env, stdout=log, stderr=log)
                try:
                    base = f'http://127.0.0.1:{port}'

                    def healthy():
                        try:
                            with urllib.request.urlopen(base + '/healthz', timeout=1) as response:
                                return json.load(response)['ok']
                        except (OSError, urllib.error.URLError):
                            assert process.poll() is None, 'Packaged service exited'
                            return False

                    if not poll(healthy, 5, interval=0.05):
                        raise AssertionError('Embedded service did not start')
                    with urllib.request.urlopen(base + '/', timeout=15) as response:
                        html = response.read().decode()
                        assert response.headers['Content-Type'].startswith('text/html')
                    js = re.search(r'_app/immutable/entry/[^"\s]+\.js', html)
                    assert js, 'SPA boot script missing'
                    with urllib.request.urlopen(base + '/' + js.group(), timeout=15) as response:
                        assert 'javascript' in response.headers['Content-Type']
                        assert response.read()
                    with urllib.request.urlopen(base + '/proposals', timeout=15) as response:
                        assert response.read().decode() == html
                finally:
                    process.terminate()
                    process.wait(timeout=15)
                log.seek(0)
                assert ('Non-loopback listener' in log.read()) == (listen == '0.0.0.0')
        # A regressed override check would start serving instead of exiting.
        failure = subprocess.run([str(executable), '--assets', str(root / 'missing')], cwd=root, env=env, capture_output=True, text=True, timeout=15)
        assert failure.returncode != 0 and 'Dashboard override missing' in failure.stderr
    print('PASS embedded binary: HTTP, JS, SPA, override validation and listener warnings')


def installer(binary):
    with tempfile.TemporaryDirectory(prefix='octomus-installer-') as directory:
        root = Path(directory)
        peers = root / 'peers'
        peers.mkdir()
        release = root / 'release'
        release.mkdir()
        archive = release / 'package.tar.gz'
        with tarfile.open(archive, 'w:gz') as tar:
            tar.add(binary, arcname='octomus-agent/octomus-agent')
        (peers / 'curl').write_text('''#!/usr/bin/env python3
import hashlib, os, pathlib, sys
args = sys.argv[1:]
root = pathlib.Path(os.environ['INSTALLER_FIXTURE'])
mode = os.environ.get('INSTALLER_MODE', '')
if mode == 'missing': sys.exit(22)
if '%{url_effective}' in args:
    print('https://github.com/tyk-swe/octomus-agent/releases' + ('' if mode == 'nolatest' else '/tag/v0.1.0'), end='')
    sys.exit(0)
url = next(a for a in args if a.startswith('https://'))
output = pathlib.Path(args[args.index('-o') + 1])
archive = (root / 'release/package.tar.gz').read_bytes()
if url.endswith('/SHA256SUMS'):
    digest = '0' * 64 if mode == 'checksum' else hashlib.sha256(archive).hexdigest()
    name = 'octomus-agent-v0.1.0-' + os.environ['INSTALLER_TARGET'] + '.tar.gz'
    output.write_text(digest + '  ' + name + '\\n')
else: output.write_bytes(archive)
''')
        (peers / 'uname').write_text('''#!/bin/sh
if [ "$1" = -s ]; then echo Linux; else echo "$INSTALLER_ARCH"; fi
''')
        # Every destination here is user-writable, so sudo must never be needed.
        (peers / 'sudo').write_text('''#!/bin/sh
echo "$*" >> "$INSTALLER_FIXTURE/sudo-used"
exit 1
''')
        for peer in peers.iterdir():
            peer.chmod(0o755)
        dest = root / 'bin'
        dest.mkdir()
        installed = dest / 'octomus-agent'
        env = {k: v for k, v in os.environ.items() if not k.startswith('OCTOMUS_')}
        env.update(PATH=f'{peers}:{os.environ["PATH"]}', INSTALL_DIR=str(dest), INSTALLER_FIXTURE=str(root))

        def install(*args, mode='', **overrides):
            installed.write_text('previous installation')
            return subprocess.run(['sh', str(PROJECT / 'install.sh'), *args], env={**env, 'INSTALLER_MODE': mode, **overrides}, capture_output=True, text=True, timeout=60)

        def installed_binary(result, path=installed):
            assert result.returncode == 0, f'exit {result.returncode}: {result.stderr}'
            assert path.read_bytes() == binary.read_bytes()
            assert os.access(path, os.X_OK)
            # The next steps print literally (quoted heredoc) and name the documented
            # data directory, so state never lands in the shell's current directory.
            assert 'OCTOMUS_TOKEN="$(openssl rand -hex 32)"' in result.stdout, result.stdout
            assert 'octomus-agent --data-dir /var/lib/octomus/.octomus\n' in result.stdout, result.stdout

        def refused(result, message=''):
            assert result.returncode != 0, result.stdout
            assert message in result.stderr, result.stderr
            assert installed.read_text() == 'previous installation'

        for arch, target in [('x86_64', 'x86_64-unknown-linux-gnu'), ('aarch64', 'aarch64-unknown-linux-gnu')]:
            env.update(INSTALLER_ARCH=arch, INSTALLER_TARGET=target)
            for mode in ['', 'checksum', 'missing', 'unsupported']:
                env['INSTALLER_ARCH'] = 'riscv64' if mode == 'unsupported' else arch
                result = install(*([] if not mode else ['v0.1.0']), mode=mode)
                if mode:
                    refused(result)
                else:
                    installed_binary(result)
        env.update(INSTALLER_ARCH='x86_64', INSTALLER_TARGET='x86_64-unknown-linux-gnu')
        # Only OCTOMUS_VERSION selects a release; a generic VERSION belongs to other tools.
        installed_binary(install(VERSION='1.4.2'))
        refused(install(mode='nolatest'), 'No published stable release found')
        installed_binary(install(mode='nolatest', OCTOMUS_VERSION='v0.1.0'))
        # A missing destination under a writable parent is created without sudo.
        fresh = root / 'fresh/bin'
        result = install(INSTALL_DIR=str(fresh))
        assert not (root / 'sudo-used').exists(), 'installer used sudo: ' + (root / 'sudo-used').read_text()
        installed_binary(result, fresh / 'octomus-agent')
    print('PASS installer: architectures, latest/versioned/OCTOMUS_VERSION release, no stable release, sudo-free new INSTALL_DIR, checksum/missing/unsupported failures')


# The package archive must never carry operator state, credentials, runner
# transcripts, caches or test fixtures.
DENIED_DIRECTORIES = {'.octomus', 'node_modules', 'tests', 'fixtures', '.git',
                      '.codex', '.opencode', '.cache', '.npm', '__pycache__'}
DENIED_SUFFIXES = ('.db', '.sqlite', '.sqlite3', '.wal', '-wal', '.shm', '-shm',
                   '.journal', '-journal', '.lock', '.log', '.jsonl', '.pem',
                   '.key', '.env')
DENIED_NAME = re.compile(r'(?i)(state\.db|service\.lock|credential|secret|token|'
                         r'private[-_]?key|id_rsa|transcript|rollout|fixture)')


def package_denylist(archive):
    """Asserts the shipped archive contains none of the denied entries."""
    with tarfile.open(archive) as tar:
        names = [member.name for member in tar.getmembers()]
    assert names, f'{archive} is empty'
    denied = []
    for name in names:
        parts = [part for part in name.split('/') if part not in ('', '.')]
        base = parts[-1] if parts else ''
        if any(part in DENIED_DIRECTORIES for part in parts):
            denied.append((name, 'private directory'))
        elif base.endswith(DENIED_SUFFIXES) or DENIED_NAME.search(base):
            denied.append((name, 'state, credential or transcript file'))
    assert not denied, f'{archive} carries denied entries: {denied}'
    assert 'octomus-agent/octomus-agent' in names, f'{archive} lacks the executable'
    print(f'PASS package denylist: {len(names)} members, none match the state/credential/transcript denylist')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--package', type=Path)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix='octomus-package-') as directory:
        if args.package:
            package_denylist(args.package)
            with tarfile.open(args.package) as tar:
                tar.extractall(directory, filter='data')
            binary = Path(directory) / 'octomus-agent/octomus-agent'
        else:
            binary = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(PROJECT / 'bin/octomus-agent'))).resolve()
        smoke(binary)
        installer(binary)
