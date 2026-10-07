"""Behavior checks for fixture startup, isolated ports and failure diagnostics."""
import contextlib
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest

from harness import poll, wait_service_ready


PEER = """
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import sys

print('Peer started', flush=True)
mode = sys.argv[1]
if mode == 'exit':
    print('Deliberate startup failure', flush=True)
    sys.exit(3)

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({'ok': mode != 'unhealthy'}).encode()
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass

server = HTTPServer(('127.0.0.1', 0), Handler)
if mode != 'silent':
    print('Octomus listening on http://127.0.0.1:' + str(server.server_port), flush=True)
server.serve_forever()
"""


@contextlib.contextmanager
def peer(log_path, mode='healthy'):
    with log_path.open('a') as log:
        offset = log_path.stat().st_size
        process = subprocess.Popen([sys.executable, '-c', PEER, mode], stdout=log, stderr=log)
        try:
            assert poll(lambda: b'Peer started' in log_path.read_bytes()[offset:], 5), 'peer did not start'
            yield process, offset
        finally:
            if process.poll() is None:
                process.terminate()
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=2)


class StartupTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix='octomus-harness-')
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)

    def test_concurrent_servers_get_distinct_ports(self):
        with peer(self.root / 'first.log') as (first, _), peer(self.root / 'second.log') as (second, _):
            deadline = time.monotonic() + 5
            first_port = wait_service_ready(first, self.root / 'first.log', deadline=deadline)
            second_port = wait_service_ready(second, self.root / 'second.log', deadline=deadline)
            self.assertNotEqual(first_port, second_port)

    def test_new_launch_ignores_previous_announcement(self):
        path = self.root / 'service.log'
        with peer(path) as (first, _):
            first_port = wait_service_ready(first, path, deadline=time.monotonic() + 5)
            with peer(path) as (next_launch, offset):
                port = wait_service_ready(next_launch, path, deadline=time.monotonic() + 5, offset=offset)
                self.assertNotEqual(port, first_port)

    def test_early_exit_reports_status_and_log(self):
        path = self.root / 'service.log'
        with peer(path, 'exit') as (process, _):
            with self.assertRaisesRegex(AssertionError, r'service exited with status 3[\s\S]*Deliberate startup failure'):
                wait_service_ready(process, path, deadline=time.monotonic() + 5)

    def test_missing_announcement_obeys_deadline(self):
        path = self.root / 'service.log'
        with peer(path, 'silent') as (process, _):
            with self.assertRaisesRegex(AssertionError, r'did not become healthy[\s\S]*Peer started'):
                wait_service_ready(process, path, deadline=time.monotonic() + 0.15)
            self.assertIsNone(process.poll())

    def test_unhealthy_server_is_not_ready(self):
        path = self.root / 'service.log'
        with peer(path, 'unhealthy') as (process, _):
            with self.assertRaisesRegex(AssertionError, 'did not become healthy'):
                wait_service_ready(process, path, deadline=time.monotonic() + 0.15)
            self.assertIsNone(process.poll())


if __name__ == '__main__':
    unittest.main()
