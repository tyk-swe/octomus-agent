#!/usr/bin/env python3
"""Deterministic app-server peer. No model calls or credentials required."""
import json
import os
from pathlib import Path
import sys
import uuid

root = Path(os.environ['OCTOMUS_FIXTURE'])
if sys.argv[1:] == ['--version']:
    print('codex-cli ' + ((root / 'version').read_text().strip() if (root / 'version').exists() else '0.153.4'))
    sys.exit(0)
threads = root / 'threads'
threads.mkdir(exist_ok=True)

def emit(value):
    print(json.dumps(value), flush=True)

from worker import respond

for line in sys.stdin:
    request = json.loads(line)
    method = request.get('method')
    params = request.get('params', {})
    result = {}
    if method == 'initialized':
        continue
    if method == 'account/read':
        result = {'account': {'type': 'apiKey'}, 'requiresOpenaiAuth': True}
    elif method == 'model/list':
        result = {'data': [{'model': m, 'displayName': m, 'supportedReasoningEfforts': [{'reasoningEffort': e} for e in ['low', 'medium', 'high', 'xhigh', 'max']]} for m in ['gpt-6-astra', 'gpt-5.6-luna']], 'nextCursor': None}
    elif method in ['thread/start', 'thread/resume']:
        executor_start_failed = (root / 'failed-executor-start').exists() and method == 'thread/start' and Path(params['cwd']).parent.parent == root / '.octomus/tasks'
        if (root / 'failed-start').exists() or executor_start_failed:
            emit({'id': request['id'], 'error': {'code': -32000, 'message': 'Fixture failed start'}})
            continue
        identity = params.get('threadId') or str(uuid.uuid4())
        file = threads / f'{identity}.json'
        thread = json.loads(file.read_text()) if file.exists() else {'repairs': 0}
        # The pinned client cannot resume a rollout before its first turn starts.
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
        if ((root / 'interactive').exists() and prompt.startswith('Implement this accepted')) or ((root / 'interactive-repair').exists() and prompt.startswith('Repair actionable')):
            emit({'id': 'interactive-1', 'method': 'item/tool/requestUserInput', 'params': {'threadId': identity}})
            reply = json.loads(next(sys.stdin))
            assert reply['id'] == 'interactive-1' and 'error' in reply
            continue
        cwd = Path(params['cwd'])
        turn = str(uuid.uuid4())
        with (root / 'protocol.jsonl').open('a') as log:
            log.write(json.dumps({'thread': identity, 'prompt': prompt, 'cwd': str(cwd), 'model': params['model'], 'effort': params['effort'], 'sandbox': params['sandboxPolicy'], 'approval': params['approvalPolicy']}) + '\n')
        answer = respond(prompt, cwd, thread, file)
        text = answer if isinstance(answer, str) else json.dumps(answer)
        # Exercise out-of-order notifications before the turn/start RPC response.
        emit({'method': 'item/completed', 'params': {'threadId': identity, 'turnId': turn, 'item': {'type': 'agentMessage', 'phase': 'final_answer', 'text': text}}})
        emit({'id': request['id'], 'result': {'turn': {'id': turn}}})
        emit({'method': 'turn/completed', 'params': {'threadId': identity, 'turn': {'id': turn, 'status': 'completed', 'error': None}}})
        continue
    emit({'id': request['id'], 'result': result})
