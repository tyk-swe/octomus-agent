#!/usr/bin/env python3
"""Synthetic model provider for actual pinned clients; loopback by default, optional TLS in the isolated image test."""
import argparse
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import ssl
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('root', type=Path)
parser.add_argument('--host', default='127.0.0.1')
parser.add_argument('--port', type=int, default=0)
parser.add_argument('--tls-cert')
parser.add_argument('--tls-key')
parser.add_argument('--namespaced', action='store_true')
args = parser.parse_args()
root = args.root
answer = {'completed': True, 'summary': 'Controlled contract result.', 'findings': []}

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        backend = self.path.split('/')[1] if args.namespaced else ''
        if args.namespaced and backend not in ('codex', 'opencode'):
            self.send_error(404)
            return
        if 'CANCEL_CONTRACT_TURN' in json.dumps(request):
            (root / ('turn-entered-' + backend if backend else 'turn-entered')).touch()
            time.sleep(90)
            return
        structured = bool(request.get('text', {}).get('format', {}).get('schema'))
        tools = request.get('tools', [])
        structured_tool = next((tool for tool in tools if 'structured' in tool.get('function', {}).get('name', '').lower()), None)
        text = json.dumps(answer) if structured else 'Controlled contract result.'
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Connection', 'close')
        self.end_headers()
        def event(value, name=None):
            if name:
                self.wfile.write(f'event: {name}\n'.encode())
            self.wfile.write(('data: ' + json.dumps(value) + '\n\n').encode())
            self.wfile.flush()
        try:
            if self.path.endswith('/responses'):
                message = {'type': 'message', 'id': 'msg_contract', 'role': 'assistant', 'status': 'completed', 'content': [{'type': 'output_text', 'text': text, 'annotations': []}]}
                response = {'id': 'resp_contract', 'object': 'response', 'model': request['model'], 'status': 'completed', 'output': [message], 'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}}
                for value in [
                    {'type': 'response.created', 'response': {**response, 'status': 'in_progress', 'output': []}},
                    {'type': 'response.output_item.added', 'output_index': 0, 'item': {**message, 'status': 'in_progress', 'content': []}},
                    {'type': 'response.output_text.delta', 'item_id': message['id'], 'output_index': 0, 'content_index': 0, 'delta': text},
                    {'type': 'response.output_item.done', 'output_index': 0, 'item': message},
                    {'type': 'response.completed', 'response': response},
                ]:
                    event(value, value['type'])
            else:
                delta = {'content': text, 'role': 'assistant'}
                finish = 'stop'
                if structured_tool:
                    delta = {'role': 'assistant', 'tool_calls': [{'index': 0, 'id': 'call_contract', 'type': 'function', 'function': {'name': structured_tool['function']['name'], 'arguments': json.dumps(answer)}}]}
                    finish = 'tool_calls'
                base = {'id': 'chatcmpl-contract', 'object': 'chat.completion.chunk', 'created': 1, 'model': request['model']}
                event({**base, 'choices': [{'index': 0, 'delta': delta, 'finish_reason': None}]})
                event({**base, 'choices': [{'index': 0, 'delta': {}, 'finish_reason': finish}], 'usage': {'prompt_tokens': 1, 'completion_tokens': 1, 'total_tokens': 2}})
                self.wfile.write(b'data: [DONE]\n\n')
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass
        self.close_connection = True

server = ThreadingHTTPServer((args.host, args.port), Handler)
if args.tls_cert:
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(args.tls_cert, args.tls_key)
    server.socket = context.wrap_socket(server.socket, server_side=True)
(root / 'provider-port').write_text(str(server.server_port))
server.serve_forever()
