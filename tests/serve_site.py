#!/usr/bin/env python3
"""Preview the public site at the domain root and legacy Pages subdirectory."""
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

site = Path(__file__).resolve().parents[1] / 'dist/site'
prefix = '/octomus-agent/'


class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(site), **kwargs)

    def do_GET(self):
        if self.path.startswith(prefix):
            self.path = '/' + self.path[len(prefix):]
        super().do_GET()

    def send_error(self, code, message=None, explain=None):
        if code != 404:
            return super().send_error(code, message, explain)
        content = (site / '404.html').read_bytes()
        self.send_response(404)
        self.send_header('Content-Type', 'text/html; charset=utf-8')
        self.send_header('Content-Length', str(len(content)))
        self.end_headers()
        if self.command != 'HEAD':
            self.wfile.write(content)


if __name__ == '__main__':
    ThreadingHTTPServer(('127.0.0.1', 4310), Handler).serve_forever()
