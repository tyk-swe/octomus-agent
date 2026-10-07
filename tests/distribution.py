#!/usr/bin/env python3
"""The executable as shipped (OCTOMUS_TEST_BINARY, or an extracted release archive's): an HTTP smoke test with lock release."""
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import tempfile
import time

from harness import BINARY, local_urlopen, wait_service_ready

TOKEN = 'distribution-fixture-token-at-least-32-characters'


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
            with (root / 'service.log').open('w+') as log:
                deadline = time.monotonic() + 5
                process = subprocess.Popen([str(executable), '--listen', f'{listen}:0'], cwd=root, env=env, stdout=subprocess.PIPE, stderr=log)
                try:
                    port = wait_service_ready(process, root / 'service.log', deadline=deadline, label='Packaged service startup')
                    base = f'http://127.0.0.1:{port}'
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
                    blocked = subprocess.run([str(executable), '--listen', '127.0.0.1:0'], cwd=root, env=env, capture_output=True, text=True, timeout=15)
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


if __name__ == '__main__':
    smoke(BINARY.resolve())
