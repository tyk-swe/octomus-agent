#!/usr/bin/env python3
"""Deterministic OpenCode 1.18.30 HTTP/SSE peer. No credentials or model calls."""
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import queue
import signal
import socket
import subprocess
import sys
import threading
import time
from urllib.parse import parse_qs, unquote, urlparse
import uuid

from worker import respond

root = Path(os.environ['OCTOMUS_FIXTURE'])
mode = lambda: (root / 'opencode-mode').read_text().strip() if (root / 'opencode-mode').exists() else ''
if sys.argv[1:] == ['--version']:
    print('1.18.30')
    sys.exit(0)
if mode() == 'startup-failure':
    sys.exit(1)
if mode() == 'startup-hang':
    time.sleep(120)
assert sys.argv[1] == 'serve'
assert 'OCTOMUS_TOKEN' not in os.environ
assert 'OCTOMUS_NOTIFICATION_WEBHOOK_URL' not in os.environ
assert os.environ['OPENCODE_DISABLE_PROJECT_CONFIG'] == 'true'
policy = json.loads(os.environ['OPENCODE_CONFIG_CONTENT'])
sessions = root / 'oc-sessions'
sessions.mkdir(exist_ok=True)
subscribers = []
lock = threading.Lock()
aborts = {}
children = {}


def log(name, value):
    with lock, (root / name).open('a') as file:
        file.write(json.dumps(value) + '\n')


def emit(directory, value):
    with lock:
        for target, events in subscribers:
            if directory == target:
                events.put(value)


def model(name, toolcall=True, variants=True):
    return {'id': name, 'name': name, 'capabilities': {'toolcall': toolcall, 'input': {'text': True}, 'output': {'text': True}}, 'variants': {'low': {}, 'high': {}} if variants else {}}


class Handler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *args):
        pass

    def setup_request(self):
        expected = 'Basic ' + base64.b64encode(f"{os.environ['OPENCODE_SERVER_USERNAME']}:{os.environ['OPENCODE_SERVER_PASSWORD']}".encode()).decode()
        if self.headers.get('Authorization') != expected:
            self.send_json({'error': 'unauthorized'}, 401)
            return False
        parsed = urlparse(self.path)
        self.parts = unquote(parsed.path).strip('/').split('/')
        self.directory = parse_qs(parsed.query).get('directory', [''])[0]
        log('opencode-requests.jsonl', {'method': self.command, 'path': parsed.path, 'directory': self.directory})
        return True

    def send_json(self, value, status=200):
        self.send_bytes(json.dumps(value).encode(), status)

    def send_bytes(self, value, status=200):
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(value)))
        self.end_headers()
        self.wfile.write(value)

    def do_GET(self):
        if not self.setup_request():
            return
        if self.parts == ['global', 'health']:
            self.send_json({'healthy': True, 'version': '0.0.0-fixture' if mode() == 'version-mismatch' else '1.18.30'})
        elif self.parts == ['config']:
            self.send_json({**policy, 'share': 'auto'} if mode() == 'wrong-policy' else policy)
        elif self.parts == ['provider']:
            providers = []
            for identity in ['fixture', 'alternate', 'offline']:
                providers.append({'id': identity, 'name': identity.title(), 'key': 'fixture-credential-do-not-expose', 'options': {'apiKey': 'another-fixture-secret'}, 'env': ['PRIVATE_API_KEY'], 'models': {'fixture-model': model('fixture-model'), 'plain-model': model('plain-model', variants=False), 'no-tools': model('no-tools', toolcall=False)}})
            self.send_json({'all': providers, 'default': {'fixture': 'fixture-model'}, 'connected': [] if mode() == 'no-provider' else ['fixture', 'alternate']})
        elif len(self.parts) == 2 and self.parts[0] == 'session':
            file = sessions / f'{self.parts[1]}.json'
            if file.exists():
                info = json.loads(file.read_text())
                if mode() == 'wrong-workspace':
                    info['directory'] = '/wrong-workspace'
                self.send_json(info)
            else:
                self.send_json({'error': 'missing session'}, 404)
        elif self.parts == ['event']:
            events = queue.Queue()
            with lock:
                subscribers.append((self.directory, events))
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Transfer-Encoding', 'chunked')
            self.end_headers()
            events.put({'type': 'server.connected', 'properties': {}})
            try:
                while True:
                    try:
                        value = events.get(timeout=1)
                    except queue.Empty:
                        value = {'type': 'server.heartbeat', 'properties': {}}
                    if value is None:
                        self.wfile.write(b'0\r\n\r\n')
                        break
                    frame = b'data: not-json\n\n' if value == 'malformed' else ('data: ' + json.dumps(value, ensure_ascii=False) + '\n\n').encode()
                    # Real HTTP chunks can split both SSE lines and UTF-8 codepoints.
                    for offset in range(0, len(frame), 17):
                        chunk = frame[offset:offset + 17]
                        self.wfile.write(f'{len(chunk):x}\r\n'.encode() + chunk + b'\r\n')
                    self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass
            finally:
                with lock:
                    subscribers.remove((self.directory, events))
        else:
            self.send_json({'error': 'unexpected endpoint'}, 404)

    def do_POST(self):
        if not self.setup_request():
            return
        body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', '0'))) or b'{}')
        if self.parts == ['session']:
            identity = 'ses_' + uuid.uuid4().hex
            info = {**body, 'id': identity, 'directory': self.directory, 'repairs': 0}
            (sessions / f'{identity}.json').write_text(json.dumps(info))
            self.send_json(info)
        elif len(self.parts) == 3 and self.parts[0] == 'session' and self.parts[2] == 'abort':
            aborts.setdefault(self.parts[1], threading.Event()).set()
            log('opencode-aborts.jsonl', {'session': self.parts[1]})
            child = children.pop(self.parts[1], None)
            if child is not None and child.poll() is None:
                os.killpg(child.pid, signal.SIGTERM)
                try:
                    child.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    os.killpg(child.pid, signal.SIGKILL)
                    child.wait(timeout=1)
            self.send_json(True)
        elif self.parts[0] in ['permission', 'question'] or self.parts[:2] == ['api', 'session']:
            assert self.parts[-1] == 'reject' or body['reply'] == 'reject'
            (root / 'opencode-rejected').touch()
            self.send_json(True)
        elif len(self.parts) == 3 and self.parts[0] == 'session' and self.parts[2] == 'message':
            self.prompt(body)
        else:
            self.send_json({'error': 'unexpected endpoint'}, 404)

    def prompt(self, body):
        identity = self.parts[1]
        file = sessions / f'{identity}.json'
        thread = json.loads(file.read_text())
        assert thread['directory'] == self.directory
        assert body['agent'] in policy['agent']
        assert 'Never publish' in body['system']
        assert body['model']['providerID'] == thread['model']['providerID']
        assert body['model']['modelID'] == thread['model']['id']
        message_id = body['messageID']
        assert message_id.startswith('msg_') and len(message_id) == 30
        assert all(c in '0123456789abcdef' for c in message_id[4:16])
        assert message_id > thread.get('last_user_message', '')
        thread['last_user_message'] = message_id
        file.write_text(json.dumps(thread))
        prompt = body['parts'][0]['text']
        log('protocol.jsonl', {'backend': 'opencode', 'thread': identity, 'prompt': prompt, 'cwd': self.directory, 'provider': body['model']['providerID'], 'model': body['model']['modelID'], 'variant': body.get('variant')})
        # Model-dependent failure markers leave the planning cycle intact in full-service tests.
        behavior = mode() if body['model']['modelID'] != 'plain-model' else ''
        info = {'id': 'msg_' + uuid.uuid4().hex, 'sessionID': identity, 'parentID': body['messageID'], 'role': 'assistant', 'modelID': body['model']['modelID'], 'providerID': body['model']['providerID'], 'time': {'created': 1, 'completed': 2}, 'finish': 'stop'}
        if 'variant' in body:
            info['variant'] = body['variant']
        if behavior in ['interactive', 'question', 'interactive-v2', 'question-v2', 'timeout', 'events-disconnect', 'invalid-event', 'hold', 'detached-hold']:
            (root / 'opencode-entered').touch()
            if behavior in ['interactive', 'question', 'interactive-v2', 'question-v2']:
                kind = 'permission' if behavior.startswith('interactive') else 'question'
                emit(self.directory, {'type': kind + ('.v2.asked' if behavior.endswith('-v2') else '.asked'), 'properties': {'sessionID': identity, 'id': 'request-1'}})
            elif behavior == 'events-disconnect':
                emit(self.directory, None)
            elif behavior == 'invalid-event':
                emit(self.directory, 'malformed')
            elif behavior == 'hold':
                child = subprocess.Popen(['sleep', '120'])
                (root / 'opencode-child-pid').write_text(str(child.pid))
            elif behavior == 'detached-hold':
                # OpenCode uses detached shell groups with a three-second SIGKILL fallback.
                ready = root / 'opencode-child-ready'
                script = 'import signal,time,sys;from pathlib import Path;signal.signal(signal.SIGTERM,signal.SIG_IGN);Path(sys.argv[1]).touch();time.sleep(120)'
                child = subprocess.Popen([sys.executable, '-c', script, str(ready)], start_new_session=True)
                children[identity] = child
                while not ready.exists():
                    time.sleep(0.005)
                (root / 'opencode-child-pid').write_text(str(child.pid))
            while not aborts.setdefault(identity, threading.Event()).wait(0.05):
                if behavior == 'hold' and not (root / 'opencode-mode').exists():
                    break
            if aborts[identity].is_set():
                self.send_json({'info': {**info, 'error': {'name': 'MessageAbortedError'}}, 'parts': []})
                return
        if behavior == 'disconnect':
            self.connection.shutdown(socket.SHUT_RDWR)
            self.connection.close()
            return
        if behavior == 'invalid-json':
            self.send_bytes(b'not json')
            return
        if behavior == 'oversized-json':
            self.send_bytes(b' ' * 16_000_001)
            return
        answer = respond(prompt, Path(self.directory), thread, file) if prompt != 'Fixture prompt' else 'Fixture completed. ✓'
        if behavior == 'wrong-model':
            info['modelID'] = 'substituted'
        elif behavior == 'wrong-variant':
            info['variant'] = 'substituted'
        elif behavior == 'wrong-session':
            info['sessionID'] = 'ses_unrelated'
        elif behavior == 'wrong-message':
            info['parentID'] = 'msg_unrelated'
        elif behavior == 'incomplete':
            del info['time']['completed']
        elif behavior == 'truncated':
            info['finish'] = 'length'
        elif behavior == 'failed':
            info['error'] = {'name': 'StructuredOutputError'}
        parts = [{'id': 'prt_fixture', 'sessionID': identity, 'messageID': info['id'], 'type': 'text', 'text': answer if isinstance(answer, str) else json.dumps(answer)}]
        if 'format' in body:
            info['structured'] = {'completed': True, 'summary': 'Fixture review.', 'findings': []} if prompt == 'Fixture prompt' else answer
            info['finish'] = 'tool-calls' if behavior != 'truncated' else 'length'
            if behavior == 'missing-structured':
                del info['structured']
            elif behavior == 'malformed-structured':
                info['structured'] = 'not a review object'
        if behavior == 'empty':
            parts = []
        # Include an unrelated event and Unicode progress data to exercise filtering and framing.
        emit(self.directory, {'type': 'message.updated', 'properties': {'info': {'sessionID': 'ses_unrelated', 'role': 'assistant', 'parentID': body['messageID'], 'modelID': 'ignored'}}})
        emit(self.directory, {'type': 'message.part.updated', 'properties': {'sessionID': identity, 'part': {'type': 'tool', 'state': {'status': 'completed', 'output': 'private fixture ✓'}}}})
        emit(self.directory, {'type': 'message.updated', 'properties': {'info': info}})
        self.send_json({'info': info, 'parts': parts})


class Server(ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, request, client_address):
        pass  # Disconnects are exercised deliberately.


port = int(sys.argv[sys.argv.index('--port') + 1])
server = Server(('127.0.0.1', port), Handler)
log('opencode-pids.jsonl', {'pid': os.getpid()})
print(f'opencode server listening on http://127.0.0.1:{server.server_port}', flush=True)
server.serve_forever()
