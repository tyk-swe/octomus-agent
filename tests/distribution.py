#!/usr/bin/env python3
"""The executable as shipped: an HTTP smoke test with lock release, and with --package the release archive."""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import subprocess
import tarfile
import tempfile
import urllib.error

from harness import local_urlopen, poll

PROJECT = Path(__file__).resolve().parents[1]
TOKEN = 'distribution-fixture-token-at-least-32-characters'


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def smoke(binary):
    """Starts the executable twice from one directory with no helper programs on PATH.

    Each start must serve the embedded dashboard and its boot script over HTTP and answer the SPA
    route. While the first runs, a second start in the same directory is refused by the state lock;
    SIGTERM then exits 0 with nothing on stdout, and the second start proves the lock was released.
    Only the non-loopback listener warns. A missing --assets override is refused.
    """
    with tempfile.TemporaryDirectory(prefix='octomus-distribution-') as directory:
        root = Path(directory)
        executable = root / 'octomus-agent'
        shutil.copy(binary, executable)
        (root / 'empty-bin').mkdir()
        env = {k: v for k, v in os.environ.items() if not k.startswith('OCTOMUS_')}
        env.update(OCTOMUS_TOKEN=TOKEN, PATH=str(root / 'empty-bin'))
        subprocess.run([str(executable), '--version'], cwd=root, env=env, check=True, timeout=15)
        for listen in ['127.0.0.1', '0.0.0.0']:
            port = free_port()
            with (root / 'service.log').open('w+') as log:
                process = subprocess.Popen([str(executable), '--listen', f'{listen}:{port}'], cwd=root, env=env, stdout=subprocess.PIPE, stderr=log)
                try:
                    base = f'http://127.0.0.1:{port}'

                    def healthy():
                        try:
                            with local_urlopen(base + '/healthz', timeout=1) as response:
                                return json.load(response)['ok']
                        except (OSError, urllib.error.URLError):
                            assert process.poll() is None, 'Packaged service exited'
                            return False

                    if not poll(healthy, 5, interval=0.05):
                        raise AssertionError('Embedded service did not start')
                    with local_urlopen(base + '/', timeout=15) as response:
                        html = response.read().decode()
                        assert response.headers['Content-Type'].startswith('text/html')
                    js = re.search(r'_app/immutable/entry/[^"\s]+\.js', html)
                    assert js, 'SPA boot script missing'
                    with local_urlopen(base + '/' + js.group(), timeout=15) as response:
                        assert 'javascript' in response.headers['Content-Type']
                        assert response.read()
                    with local_urlopen(base + '/proposals', timeout=15) as response:
                        assert response.read().decode() == html
                    blocked = subprocess.run([str(executable), '--listen', f'127.0.0.1:{free_port()}'], cwd=root, env=env, capture_output=True, text=True, timeout=15)
                    assert blocked.returncode == 1 and 'Another Octomus service' in blocked.stderr, blocked
                    process.send_signal(signal.SIGTERM)
                    stdout, _ = process.communicate(timeout=15)
                    assert process.returncode == 0 and not stdout, (process.returncode, stdout)
                finally:
                    if process.poll() is None:
                        process.kill()
                        process.communicate(timeout=5)
                log.seek(0)
                assert ('Non-loopback listener' in log.read()) == (listen == '0.0.0.0')
        failure = subprocess.run([str(executable), '--assets', str(root / 'missing')], cwd=root, env=env, capture_output=True, text=True, timeout=15)
        assert failure.returncode != 0 and 'Dashboard override missing' in failure.stderr
    print('PASS executable: HTTP, JS, SPA, asset override, listener warning, state lock and its release')


def package_contents(archive):
    """The archive holds the executable and exactly the files scripts/release-files.txt lists, each once."""
    expected = {'octomus-agent', 'octomus-agent/octomus-agent'}
    for name in (PROJECT / 'scripts/release-files.txt').read_text().split():
        parts = name.split('/')
        expected.update('octomus-agent/' + '/'.join(parts[:depth]) for depth in range(1, len(parts) + 1))
    with tarfile.open(archive) as tar:
        names = [member.name for member in tar.getmembers()]
    assert len(names) == len(set(names)), f'{archive} repeats members'
    assert set(names) == expected, f'{archive} differs from the release manifest: {sorted(set(names) ^ expected)}'
    print(f'PASS package: {len(names)} members, exactly the executable and the release manifest')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--package', type=Path)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix='octomus-package-') as directory:
        if args.package:
            package_contents(args.package)
            with tarfile.open(args.package) as tar:
                tar.extractall(directory, filter='data')
            binary = Path(directory) / 'octomus-agent/octomus-agent'
        else:
            binary = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(PROJECT / 'bin/octomus-agent'))).resolve()
        smoke(binary)
