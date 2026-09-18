#!/usr/bin/env python3
"""Preview the static site at the exact GitHub Pages subdirectory, without an API."""
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

site = Path(__file__).resolve().parents[1] / 'dist/site'
prefix = '/octomus-agent/'


class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(site), **kwargs)

    def do_GET(self):
        if not self.path.startswith(prefix):
            self.send_error(404)
            return
        self.path = '/' + self.path[len(prefix):]
        super().do_GET()


if __name__ == '__main__':
    ThreadingHTTPServer(('127.0.0.1', 4310), Handler).serve_forever()
