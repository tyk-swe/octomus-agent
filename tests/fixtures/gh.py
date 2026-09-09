#!/usr/bin/env python3
"""GitHub fixture backed by a real local Git remote, with publication fault injection."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time
root = Path(os.environ['OCTOMUS_FIXTURE'])
import fcntl
lock = (root / 'github.lock').open('a')
fcntl.flock(lock, fcntl.LOCK_EX)
file = root / 'prs.json'
prs = json.loads(file.read_text()) if file.exists() else []
args = sys.argv[1:]

def arg(name):
    return args[args.index(name) + 1]

def refresh(pr):
    pr['head']['sha'] = subprocess.check_output(['/usr/bin/git', '--git-dir', str(root / 'remote.git'), 'rev-parse', pr['head']['ref']], text=True).strip()
    return pr

if args[:2] == ['auth', 'status']:
    print('Authenticated fixture operator')
elif args[0] == 'api':
    route = args[-1]
    parts = route.split('?')[0].split('/')
    feedback = json.loads((root / 'feedback.json').read_text()) if (root / 'feedback.json').exists() else {}
    with (root / 'feedback-requests.jsonl').open('a') as log:
        log.write(route + '\n')
    if parts[-1] == 'check-runs':
        pr = next((p for p in prs if refresh(p)['head']['sha'] == parts[-2]), None)
        runs = feedback.get(str(pr['number']), {}).get('check_runs', []) if pr else []
        print(json.dumps({'total_count': len(runs), 'check_runs': runs}))
    elif parts[-1] in ['reviews', 'comments'] and parts[-3] in ['pulls', 'issues']:
        kind = 'reviews' if parts[-1] == 'reviews' else 'review_comments' if parts[-3] == 'pulls' else 'issue_comments'
        print(json.dumps(feedback.get(parts[-2], {}).get(kind, [])))
    elif '?' in route:
        print(json.dumps([refresh(p) for p in prs if p['state'] == 'open' or 'state=all' in route]))
    else:
        number = int(parts[-1])
        print(json.dumps(refresh(next(p for p in prs if p['number'] == number))))
elif args[:2] == ['pr', 'create']:
    branch = arg('--head')
    assert not any(p['head']['ref'] == branch for p in prs), 'Duplicate PR creation attempted'
    number = len(prs) + 1
    pr = {'number': number, 'title': arg('--title'), 'body': Path(arg('--body-file')).read_text(), 'head': {'ref': branch, 'sha': '', 'repo': {'full_name': 'fixture/project'}}, 'base': {'ref': arg('--base')}, 'html_url': f'https://github.com/fixture/project/pull/{number}', 'state': 'open', 'merged_at': None, 'additions': 1, 'deletions': 0, 'created_at': '2026-09-07T00:00:00Z'}
    prs.append(refresh(pr))
    file.write_text(json.dumps(prs))
    with (root / 'publications.jsonl').open('a') as log:
        log.write(json.dumps({'action': 'create', 'number': number}) + '\n')
    if (root / 'interrupt-publication').exists():
        (root / 'publication-created').touch()
        time.sleep(3)
    print(pr['html_url'])
elif args[:2] == ['pr', 'edit']:
    number = int(args[2])
    pr = next(p for p in prs if p['number'] == number)
    pr['body'] = Path(arg('--body-file')).read_text()
    with (root / 'publications.jsonl').open('a') as log:
        log.write(json.dumps({'action': 'edit', 'number': number}) + '\n')
    file.write_text(json.dumps(prs))
    print(pr['html_url'])
else:
    raise AssertionError(args)
