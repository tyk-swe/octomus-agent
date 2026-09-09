#!/usr/bin/env python3
"""Deterministic app-server peer. No model calls or credentials required."""
import json
import os
from pathlib import Path
import sys
import time
import uuid

root = Path(os.environ['OCTOMUS_FIXTURE'])
if sys.argv[1:] == ['--version']:
    print('codex-cli ' + ((root / 'version').read_text().strip() if (root / 'version').exists() else '0.153.4'))
    sys.exit(0)
threads = root / 'threads'
threads.mkdir(exist_ok=True)

def emit(value):
    print(json.dumps(value), flush=True)

def proposal():
    return {
        'id': 'd0-feature', 'title': 'Complete the fixture feature',
        'problem': 'The fixture has no complete feature output.',
        'evidence': ['README.md: the feature contract requires fixed output'],
        'benefit': 'Delivers the documented feature.', 'category': 'features',
        'target': (root / 'target').read_text().strip() if (root / 'target').exists() else 'main', 'tier': 'M', 'scope': 'Implement feature.txt only.',
        'dependencies': [], 'prompt': 'Create feature.txt with fixed output and verify its contents. fixture-file=feature.txt',
        'decision': 'accepted', 'reason': 'Both independent reviews accept the concrete feature; no duplicates.'
    }

def move_main(marker):
    """External work lands on the remote default branch exactly once for the given scenario marker."""
    if not (root / marker).exists() or (root / 'external-revision').exists():
        return
    import subprocess
    checkout = root / 'checkout'
    # main-conflict lands a different feature.txt; main-absorbed lands the executor's exact patch.
    name = 'feature.txt' if marker in ['main-conflict', 'main-absorbed'] else 'external.txt'
    (checkout / name).write_text('needs repair\n' if marker == 'main-absorbed' else 'external\n')
    run = lambda *args: subprocess.check_output(['/usr/bin/git', *args], cwd=checkout, text=True).strip()
    run('add', name)
    run('-c', 'user.name=External', '-c', 'user.email=external@example.com', 'commit', '-m', 'External work on main')
    run('push', 'origin', 'main')
    (root / 'external-revision').write_text(run('rev-parse', 'HEAD'))

def proposals():
    first = proposal()
    if (root / 'parallel').exists() or (root / 'dependencies').exists():
        second = {**first, 'id': 'd0-followup', 'title': 'Complete the next fixture feature', 'problem': 'The next output capability is missing.', 'scope': 'Implement feature-next.txt only.', 'evidence': ['README.md: next feature output'], 'prompt': 'Implement the next fixture capability. fixture-file=feature-next.txt'}
        if (root / 'dependencies').exists():
            second['dependencies'] = [first['id']]
        return [first, second]
    if (root / 'audit-decisions').exists():
        return [first, {**first, 'id': 'd0-rejected', 'title': 'Unnecessary rewrite', 'decision': 'rejected', 'reason': 'No measured benefit; both adversaries reject it.'}, {**first, 'id': 'd0-deferred', 'title': 'Later improvement', 'decision': 'deferred', 'reason': 'Wait for evidence from operation.'}]
    return [first]

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
        executor_start_failed = (root / 'failed-executor-start').exists() and method == 'thread/start' and Path(params['cwd']) == root / '.octomus'
        if (root / 'failed-start').exists() or executor_start_failed:
            emit({'id': request['id'], 'error': {'code': -32000, 'message': 'Fixture failed start'}})
            continue
        identity = params.get('threadId') or str(uuid.uuid4())
        file = threads / f'{identity}.json'
        thread = json.loads(file.read_text()) if file.exists() else {'repairs': 0}
        thread.update(params)
        thread['id'] = identity
        file.write_text(json.dumps(thread))
        result = {'thread': {'id': identity}, 'model': params['model'], 'reasoningEffort': params['config']['model_reasoning_effort'], 'approvalPolicy': params['approvalPolicy'], 'sandbox': {'type': 'dangerFullAccess'}}
    elif method == 'turn/start':
        identity = params['threadId']
        file = threads / f'{identity}.json'
        thread = json.loads(file.read_text())
        prompt = params['input'][0]['text']
        if ((root / 'interactive').exists() and prompt.startswith('Implement this accepted')) or ((root / 'interactive-repair').exists() and prompt.startswith('Repair actionable')):
            emit({'id': 'interactive-1', 'method': 'item/tool/requestUserInput', 'params': {'threadId': identity}})
            reply = json.loads(next(sys.stdin))
            assert reply['id'] == 'interactive-1' and 'error' in reply
            continue
        cwd = Path(params['cwd'])
        import re
        match = re.search(r'fixture-file=([a-z-]+\.txt)', prompt)
        feature_file = match.group(1) if match else 'feature.txt'
        turn = str(uuid.uuid4())
        with (root / 'protocol.jsonl').open('a') as log:
            log.write(json.dumps({'thread': identity, 'prompt': prompt, 'cwd': str(cwd), 'model': params['model'], 'effort': params['effort'], 'sandbox': params['sandboxPolicy'], 'approval': params['approvalPolicy']}) + '\n')
        if prompt.startswith('Ground this repository'):
            if (root / 'audit-hold').exists():
                (root / 'audit-entered').touch()
                while (root / 'audit-hold').exists():
                    time.sleep(0.05)
            answer = {'context': 'Small fixture with a feature contract in README.md.'}
        elif prompt.startswith('Discover worthwhile'):
            answer = {'proposals': [] if (root / 'idle').exists() or 'IDs prefixed d0-' not in prompt else proposals()}
        elif prompt.startswith('Adversarial proposal'):
            answer = {'assessments': [] if (root / 'idle').exists() else [{'id': p['id'], 'decision': 'accepted', 'reason': 'Concrete and useful.'} for p in proposals()]}
        elif prompt.startswith('Act as final orchestrator'):
            answer = {'proposals': [] if (root / 'idle').exists() else proposals()}
            if (root / 'audit-malformed').exists():
                answer = {'proposals': []}
        elif prompt.startswith('Implement this accepted task'):
            (cwd / feature_file).write_text('needs repair\n')
            move_main('main-moved')
            move_main('main-conflict')
            move_main('main-absorbed')
            answer = 'Implemented feature.txt. Relevant verification is pending.'
        elif prompt.startswith('Perform a fresh code review'):
            if (root / 'malformed-review').exists():
                answer = 'not valid review JSON'
            elif (root / 'incomplete-review').exists():
                answer = {'completed': False, 'summary': 'Review interrupted.', 'findings': []}
            elif (cwd / feature_file).read_text().strip() == 'fixed':
                if (root / 'remote-conflict').exists():
                    import subprocess
                    remote = str(root / 'remote.git')
                    branch = (root / 'target').read_text().strip()
                    parent = subprocess.check_output(['/usr/bin/git', '--git-dir', remote, 'rev-parse', branch], text=True).strip()
                    tree = subprocess.check_output(['/usr/bin/git', '--git-dir', remote, 'rev-parse', f'{parent}^{{tree}}'], text=True).strip()
                    commit = subprocess.check_output(['/usr/bin/git', '--git-dir', remote, '-c', 'user.name=External', '-c', 'user.email=external@example.com', 'commit-tree', tree, '-p', parent, '-m', 'External work'], text=True).strip()
                    subprocess.check_call(['/usr/bin/git', '--git-dir', remote, 'update-ref', f'refs/heads/{branch}', commit])
                    (root / 'external-revision').write_text(commit)
                move_main('main-moved-late')
                answer = {'completed': True, 'summary': 'Reviewed the complete diff; no actionable findings remain.', 'findings': []}
            else:
                answer = {'completed': True, 'summary': 'The output contract is incomplete.', 'findings': [{'title': 'Complete the output', 'file': 'feature.txt:1', 'detail': 'Must contain fixed.', 'priority': 'P1'}]}
        elif prompt.startswith('Repair actionable findings'):
            thread['repairs'] += 1
            file.write_text(json.dumps(thread))
            (cwd / feature_file).write_text('fixed\n' if thread['repairs'] >= 2 else 'partial\n')
            answer = 'Repaired feature output and checked the contract.'
        else:
            raise AssertionError(f'Unexpected prompt: {prompt[:100]}')
        text = answer if isinstance(answer, str) else json.dumps(answer)
        # Exercise out-of-order notifications before the turn/start RPC response.
        emit({'method': 'item/completed', 'params': {'threadId': identity, 'turnId': turn, 'item': {'type': 'agentMessage', 'phase': 'final_answer', 'text': text}}})
        emit({'id': request['id'], 'result': {'turn': {'id': turn}}})
        emit({'method': 'turn/completed', 'params': {'threadId': identity, 'turn': {'id': turn, 'status': 'completed', 'error': None}}})
        continue
    emit({'id': request['id'], 'result': result})
