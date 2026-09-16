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
    pr['base'].setdefault('repo', {'full_name': 'fixture/project'})
    owned_source = (pr['head'].get('repo') or {}).get('full_name') == 'fixture/project'
    if owned_source:
        pr['head']['sha'] = subprocess.check_output(['/usr/bin/git', '--git-dir', str(root / 'remote.git'), 'rev-parse', pr['head']['ref']], text=True).strip()
    return pr

if (root / 'reconcile-delay').exists():
    child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    with (root / 'reconcile-processes.jsonl').open('a') as log:
        log.write(json.dumps({'args': args, 'pid': os.getpid(), 'child_pid': child.pid}) + '\n')
    time.sleep(float((root / 'reconcile-delay').read_text()))

if args[:2] == ['auth', 'status']:
    if (root / 'reconcile-hold').exists():
        (root / 'reconcile-entered').touch()
        while (root / 'reconcile-hold').exists():
            time.sleep(0.02)
    print('Authenticated fixture operator')
elif args[0] == 'api':
    route = args[-1]
    with (root / 'gh-api.jsonl').open('a') as log:
        log.write(json.dumps({'route': route}) + '\n')
    if '?' in route:
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
    file.write_text(json.dumps(prs))
    with (root / 'publications.jsonl').open('a') as log:
        log.write(json.dumps({'action': 'create', 'number': number}) + '\n')
    if (root / 'interrupt-publication').exists():
        (root / 'publication-created').touch()
        time.sleep(3)
    if (root / 'publication-race').exists():
        remote = str(root / 'remote.git')
        parent = pr['head']['sha']
        tree = subprocess.check_output(['/usr/bin/git', '--git-dir', remote, 'rev-parse', f'{parent}^{{tree}}'], text=True).strip()
        advanced = subprocess.check_output(['/usr/bin/git', '--git-dir', remote, '-c', 'user.name=External', '-c', 'user.email=external@example.com', 'commit-tree', tree, '-p', parent, '-m', 'Publication race'], text=True).strip()
        subprocess.check_call(['/usr/bin/git', '--git-dir', remote, 'update-ref', 'refs/heads/' + branch, advanced])
    for field in ['body', 'base', 'owner']:
        if (root / ('publication-' + field)).exists():
            if field == 'body':
                pr['body'] = 'External replacement body'
            elif field == 'base':
                pr['base']['ref'] = 'other'
            else:
                pr['head']['repo']['full_name'] = 'external/project'
            file.write_text(json.dumps(prs))
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
