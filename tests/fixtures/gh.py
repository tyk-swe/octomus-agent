#!/usr/bin/env python3
"""GitHub fixture backed by a real local Git remote."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time
root = Path(os.environ['OCTOMUS_FIXTURE'])
assert 'OCTOMUS_TOKEN' not in os.environ
assert 'OCTOMUS_NOTIFICATION_WEBHOOK_URL' not in os.environ
import fcntl
lock = (root / 'github.lock').open('a')
fcntl.flock(lock, fcntl.LOCK_EX)
file = root / 'prs.json'
prs = json.loads(file.read_text()) if file.exists() else []
args = sys.argv[1:]

def arg(name):
    return args[args.index(name) + 1]

def save():
    """Replaces prs.json whole, so a reader never sees it truncated."""
    temporary = root / 'prs.json.tmp'
    temporary.write_text(json.dumps(prs))
    os.replace(temporary, file)

def refresh(pr):
    pr['base'].setdefault('repo', {'full_name': 'fixture/project'})
    owned_source = (pr['head'].get('repo') or {}).get('full_name') == 'fixture/project'
    if owned_source:
        pr['head']['sha'] = subprocess.check_output(['/usr/bin/git', '--git-dir', str(root / 'remote.git'), 'rev-parse', pr['head']['ref']], text=True).strip()
    return pr

if args[:2] == ['auth', 'status']:
    print('Authenticated fixture operator')
elif args[0] == 'api':
    route = args[1] if args[1] == 'graphql' else args[-1]
    if route == 'graphql':
        fields = dict(a.split('=', 1) for a in args if '=' in a)
        assert fields['owner'] == 'fixture' and fields['name'] == 'project', fields
        assert 'statusCheckRollup{state}' in fields['query'] and 'commits(last:1)' in fields['query']
        pr = refresh(next(p for p in prs if p['number'] == int(fields['number'])))
        rollup = {'state': pr['check_status']} if pr.get('check_status') else None
        value = {'number': pr['number'], 'url': pr['html_url'], 'headRefOid': pr['head']['sha'],
                 'reviewDecision': pr.get('review_decision'), 'mergeable': pr.get('mergeability', 'MERGEABLE'),
                 'commits': {'nodes': [{'commit': {'oid': pr['head']['sha'], 'statusCheckRollup': rollup}}]}}
        with (root / 'github-status.jsonl').open('a') as log:
            log.write(json.dumps({'number': pr['number']}) + '\n')
        print(json.dumps({'data': {'repository': {'pullRequest': value}}}))
    elif '/comments' in route:
        number = int(route.split('/')[-2])
        print(json.dumps(next(p for p in prs if p['number'] == number).get('comments', [])))
    elif '?' in route:
        print(json.dumps([refresh(p) for p in prs if p['state'] == 'open' or 'state=all' in route]))
    else:
        number = int(route.split('/')[-1])
        print(json.dumps(refresh(next(p for p in prs if p['number'] == number))))
elif args[:2] == ['pr', 'create']:
    branch = arg('--head')
    assert not any(p['head']['ref'] == branch for p in prs), 'Duplicate PR creation attempted'
    number = len(prs) + 1
    pr = {'number': number, 'title': arg('--title'), 'body': Path(arg('--body-file')).read_text(), 'head': {'ref': branch, 'sha': '', 'repo': {'full_name': 'fixture/project'}}, 'base': {'ref': arg('--base'), 'repo': {'full_name': 'fixture/project'}}, 'html_url': f'https://github.com/fixture/project/pull/{number}', 'state': 'open', 'merged_at': None, 'additions': 1, 'deletions': 0, 'created_at': '2026-09-07T00:00:00Z'}
    prs.append(refresh(pr))
    save()
    with (root / 'publications.jsonl').open('a') as log:
        log.write(json.dumps({'action': 'create', 'number': number}) + '\n')
    if (root / 'interrupt-publication').exists():
        (root / 'publication-created').touch()
        time.sleep(3)
    print(pr['html_url'])
elif args[:2] == ['pr', 'comment']:
    number = int(args[2])
    pr = next(p for p in prs if p['number'] == number)
    pr.setdefault('comments', []).append({'body': Path(arg('--body-file')).read_text()})
    with (root / 'publications.jsonl').open('a') as log:
        log.write(json.dumps({'action': 'comment', 'number': number}) + '\n')
    save()
    print(pr['html_url'])
else:
    raise AssertionError(args)
