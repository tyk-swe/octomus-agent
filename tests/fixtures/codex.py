#!/usr/bin/env python3
"""Deterministic app-server peer. No model calls or credentials required."""
import json
import os
from pathlib import Path
import sys
import uuid

from worker import mode as worker_mode

root = Path(os.environ['OCTOMUS_FIXTURE'])
assert 'OCTOMUS_TOKEN' not in os.environ
assert 'OCTOMUS_NOTIFICATION_WEBHOOK_URL' not in os.environ
if sys.argv[1:] == ['--version']:
    print('codex-cli ' + ((root / 'version').read_text().strip() if (root / 'version').exists() else '0.153.4'))
    sys.exit(0)
threads = root / 'threads'
threads.mkdir(exist_ok=True)
def mode():
    return worker_mode('codex')

def emit(value):
    print(json.dumps(value), flush=True)

def interrupted(request):
    """Report the turn as interrupted."""
    assert type(request.get('id')) in (int, str), 'turn/interrupt requires a request ID'
    params = request.get('params', {})
    emit({'id': request['id'], 'result': {}})
    emit({'method': 'turn/completed', 'params': {'threadId': params.get('threadId'), 'turn': {'id': params.get('turnId'), 'status': 'interrupted', 'error': None}}})


from worker import respond

for line in sys.stdin:
    request = json.loads(line)
    method = request.get('method')
    params = request.get('params', {})
    result = {}
    if method == 'initialized':
        continue
    if method == 'turn/interrupt':
        interrupted(request)
        continue
    if method == 'account/read':
        result = {'account': None, 'requiresOpenaiAuth': True} if mode() == 'no-auth' else {'account': {'type': 'apiKey'}, 'requiresOpenaiAuth': True}
    elif method == 'model/list':
        result = {'data': [{'model': m, 'displayName': m, 'supportedReasoningEfforts': [{'reasoningEffort': e} for e in ['low', 'medium', 'high', 'xhigh', 'max']]} for m in ['gpt-6-astra', 'gpt-5.6-luna']], 'nextCursor': None}
    elif method in ['thread/start', 'thread/resume']:
        identity = params.get('threadId') or str(uuid.uuid4())
        file = threads / f'{identity}.json'
        thread = json.loads(file.read_text()) if file.exists() else {'repairs': 0, 'turn_started': False}
        if method == 'thread/resume' and not thread.get('turn_started'):
            emit({'id': request['id'], 'error': {'code': -32600, 'message': f'no rollout found for thread id {identity}'}})
            continue
        thread.update(params)
        thread['id'] = identity
        file.write_text(json.dumps(thread))
        result = {'thread': {'id': identity}, 'model': params['model'], 'reasoningEffort': params['config']['model_reasoning_effort'], 'approvalPolicy': params['approvalPolicy'], 'sandbox': {'type': 'dangerFullAccess'}}
    elif method == 'turn/start':
        identity = params['threadId']
        file = threads / f'{identity}.json'
        thread = json.loads(file.read_text())
        thread['turn_started'] = True
        file.write_text(json.dumps(thread))
        prompt = params['input'][0]['text']
        if (root / 'interactive').exists() and prompt.startswith('Implement this accepted'):
            emit({'id': 'interactive-1', 'method': 'item/tool/requestUserInput', 'params': {'threadId': identity}})
            reply = json.loads(next(sys.stdin))
            assert reply['id'] == 'interactive-1' and 'error' in reply
            continue
        cwd = Path(params['cwd'])
        turn = str(uuid.uuid4())
        with (root / 'protocol.jsonl').open('a') as log:
            log.write(json.dumps({'thread': identity, 'prompt': prompt, 'cwd': str(cwd), 'model': params['model'], 'effort': params['effort'], 'sandbox': params['sandboxPolicy'], 'approval': params['approvalPolicy']}) + '\n')
        if mode() == 'bad-structured' and 'outputSchema' in params:
            text = 'not json at all'
        else:
            answer = respond(prompt, cwd, thread, file) if prompt != 'Fixture prompt' else 'Fixture completed. ✓'
            text = answer if isinstance(answer, str) else json.dumps(answer)
        completed_item = {'method': 'item/completed', 'params': {'threadId': identity, 'turnId': turn, 'item': {'type': 'agentMessage', 'phase': 'final_answer', 'text': text}}}
        completed_turn = {'method': 'turn/completed', 'params': {'threadId': identity, 'turn': {'id': turn, 'status': 'completed', 'error': None}}}
        emit(completed_item)
        emit({'id': request['id'], 'result': {'turn': {'id': turn}}})
        emit(completed_turn)
        continue
    emit({'id': request['id'], 'result': result})
