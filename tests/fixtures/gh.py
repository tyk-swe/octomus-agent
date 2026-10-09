#!/usr/bin/env python3
"""GitHub fixture backed by a real local Git remote."""
import json
import fnmatch
import os
from pathlib import Path
import subprocess
import sys
import time
root = Path(os.environ['OCTOMUS_FIXTURE'])
assert 'OCTOMUS_TOKEN' not in os.environ
assert 'OCTOMUS_NOTIFICATION_WEBHOOK_URL' not in os.environ
assert 'OCTOMUS_MERGE_TOKEN' not in os.environ
assert 'OCTOMUS_MERGE_TOKEN_FILE' not in os.environ
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

def merge_ruleset():
    override = root / 'merge-ruleset.json'
    if override.exists():
        return json.loads(override.read_text())
    return {'id': 17, 'target': 'branch', 'source_type': 'Repository',
            'source': 'fixture/project', 'enforcement': 'active',
            'bypass_actors': [{'actor_type': 'User', 'actor_id': 1001, 'bypass_mode': 'always'}],
            'conditions': {'ref_name': {'include': ['~ALL'], 'exclude': ['refs/heads/main']}},
            'rules': [{'type': 'update', 'parameters': {'update_allows_fetch_and_merge': False}}]}

def destination_restricted(pr):
    """Model a real configured update restriction, not a fictitious merge base parameter."""
    policy = merge_ruleset()
    if policy.get('enforcement') != 'active' or policy.get('target') != 'branch':
        return False
    refs = policy['conditions']['ref_name']
    ref = 'refs/heads/' + pr['base']['ref']
    matches = lambda pattern: pattern == '~ALL' or fnmatch.fnmatchcase(ref, pattern)
    if not any(map(matches, refs['include'])) or any(map(matches, refs['exclude'])):
        return False
    bypass = any(a.get('actor_type') == 'User' and a.get('actor_id') == 2002 for a in policy.get('bypass_actors', []))
    return not bypass and any(r.get('type') == 'update' for r in policy.get('rules', []))

def refresh(pr):
    pr['base'].setdefault('repo', {'full_name': 'fixture/project'})
    owned_source = (pr['head'].get('repo') or {}).get('full_name') == 'fixture/project'
    if owned_source:
        pr['head']['sha'] = subprocess.check_output(['/usr/bin/git', '--git-dir', str(root / 'remote.git'), 'rev-parse', pr['head']['ref']], text=True).strip()
    return pr

def maintenance_stats(pr):
    remote = str(root / 'remote.git')
    env = {**os.environ, 'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': os.devnull, 'GIT_CONFIG_COUNT': '0', 'GIT_CONFIG_PARAMETERS': '', 'GIT_ATTR_NOSYSTEM': '1'}
    env.pop('GIT_ATTR_SOURCE', None)
    def read(*parts):
        return subprocess.check_output(['/usr/bin/git', '--git-dir', remote, '-c', 'core.hooksPath=' + os.devnull, '-c', 'core.fsmonitor=false', '-c', 'core.attributesFile=' + os.devnull, *parts], env=env, timeout=10)
    base = read('rev-parse', 'refs/heads/' + pr['base']['ref']).decode().strip()
    head = pr['head']['sha']
    captured = pr.get('maintenance_stats')
    if captured and captured['head'] == head and captured['base_sha'] == base:
        return captured
    comparison = read('merge-base', base, head).decode().strip()
    if captured and captured['head'] == head and captured['comparison_base'] == comparison:
        captured = {**captured, 'base_sha': base}
        pr['maintenance_stats'] = captured
        save()
        return captured
    output = read('diff', '--numstat', '-z', '--no-renames', '--no-ext-diff', '--no-textconv', '--ignore-submodules=none', comparison, head, '--')
    entries = output[:-1].split(b'\0') if output else []
    assert not output or output.endswith(b'\0')
    additions = deletions = 0
    seen = set()
    for entry in entries:
        added, removed, path = entry.split(b'\t', 2)
        assert path and path not in seen
        seen.add(path)
        if added == b'-' or removed == b'-':
            assert added == removed == b'-'
        else:
            assert added.isdigit() and removed.isdigit()
            additions += int(added)
            deletions += int(removed)
    captured = {'head': head, 'additions': additions, 'deletions': deletions, 'changed_files': len(seen), 'base_sha': base, 'comparison_base': comparison}
    pr['maintenance_stats'] = captured
    save()
    return captured

def merge_status_response(pr):
    stats = maintenance_stats(pr)
    additions = pr['merge_additions'] if 'merge_additions' in pr else stats['additions']
    deletions = pr['merge_deletions'] if 'merge_deletions' in pr else stats['deletions']
    changed_files = pr['merge_changed_files'] if 'merge_changed_files' in pr else stats['changed_files']
    state = {'open': 'OPEN', 'closed': 'CLOSED', 'merged': 'MERGED'}[pr['state']]
    check = pr.get('check_status', 'SUCCESS')
    rollup = {'state': check, 'contexts': {'totalCount': pr.get('check_contexts', 1)}} if check else None
    return {'data': {'repository': {
        'nameWithOwner': 'fixture/project',
        'squashMergeAllowed': pr.get('squash_merge_allowed', True) and not (root / 'squash-disabled').exists(),
        'pullRequest': {
            'number': pr['number'], 'url': pr['html_url'], 'state': state,
            'isDraft': pr.get('is_draft', False),
            'headRefName': pr['head']['ref'], 'headRefOid': pr['head']['sha'],
            'headRepository': {'nameWithOwner': (pr['head'].get('repo') or {}).get('full_name') or 'fixture/project'},
            'baseRefName': pr['base']['ref'], 'baseRefOid': stats['base_sha'],
            'repository': {'nameWithOwner': pr['base'].get('repo', {}).get('full_name', 'fixture/project')},
            'isMergeQueueEnabled': bool(pr.get('merge_queue', False)) or (root / 'merge-queue').exists(),
            'reviewDecision': pr.get('review_decision'),
            'mergeable': pr.get('mergeability', 'MERGEABLE'),
            'mergeStateStatus': pr.get('merge_state_status', 'CLEAN'),
            'additions': additions, 'deletions': deletions, 'changedFiles': changed_files,
            'mergedAt': pr.get('merged_at'),
            'mergeCommit': {'oid': pr['merge_commit_sha']} if pr.get('merge_commit_sha') else None,
            'commits': {'nodes': [{'commit': {'oid': pr['head']['sha'], 'statusCheckRollup': rollup}}]},
        }}}}

def squash_merge(pr, head):
    remote = str(root / 'remote.git')
    env = {k: v for k, v in os.environ.items() if k != 'GIT_ATTR_SOURCE'}
    env.update({'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': os.devnull, 'GIT_CONFIG_COUNT': '0', 'GIT_CONFIG_PARAMETERS': '', 'GIT_ATTR_NOSYSTEM': '1'})
    base_ref = 'refs/heads/' + pr['base']['ref']
    base_sha = subprocess.check_output(['/usr/bin/git', '--git-dir', remote, 'rev-parse', base_ref], env=env, text=True).strip()
    merged = subprocess.run(['/usr/bin/git', '--git-dir', remote, 'merge-tree', '--write-tree', base_sha, head], env=env, text=True, capture_output=True)
    if merged.returncode != 0 or not merged.stdout.strip():
        raise AssertionError(f'merge-tree refused: {merged.stderr or merged.stdout}')
    tree = merged.stdout.strip().split('\n')[0]
    commit = subprocess.check_output(['/usr/bin/git', '--git-dir', remote, '-c', 'user.name=Octomus Fixture', '-c', 'user.email=fixture@example.com', 'commit-tree', tree, '-p', base_sha, '-m', f'Squash merge pull request #{pr["number"]}'], env=env, text=True).strip()
    update = subprocess.run(['/usr/bin/git', '--git-dir', remote, 'update-ref', base_ref, commit, base_sha], env=env, capture_output=True)
    if update.returncode != 0:
        raise AssertionError('update-ref lost a base race')
    return commit

if args[:2] == ['auth', 'status']:
    print('Authenticated fixture operator')
elif args[0] == 'api':
    endpoint = next((a for a in args[1:] if a in ('graphql', 'user') or a.startswith('repos/')), args[1])
    if endpoint == 'user' or endpoint.endswith('/merge'):
        assert os.environ.get('GH_TOKEN') == 'fixture-merge-token', 'restricted merge identity was not used'
        assert arg('--hostname') == 'github.com'
    else:
        assert os.environ.get('GH_TOKEN') != 'fixture-merge-token', 'merger credential escaped its dedicated requests'
    if endpoint == 'user':
        print(json.dumps({'id': 2002, 'type': 'User'}))
    elif '/rulesets/' in endpoint:
        assert endpoint == 'repos/fixture/project/rulesets/17?includes_parents=false'
        print(json.dumps(merge_ruleset()))
    elif endpoint == 'graphql':
        fields = dict(a.split('=', 1) for a in args if '=' in a)
        assert fields['owner'] == 'fixture' and fields['name'] == 'project', fields
        assert 'statusCheckRollup{state' in fields['query'] and 'commits(last:1)' in fields['query']
        pr = refresh(next(p for p in prs if p['number'] == int(fields['number'])))
        with (root / 'github-status.jsonl').open('a') as log:
            log.write(json.dumps({'number': pr['number'], 'merge': 'mergeStateStatus' in fields['query']}) + '\n')
        if 'mergeStateStatus' in fields['query']:
            if pr['state'] == 'merged' and (root / 'merge-status-unreadable').exists():
                status = (root / 'merge-status-unreadable').read_text().strip() or '503'
                print(f'HTTP {status}: Status temporarily unavailable', file=sys.stderr)
                sys.exit(1)
            print(json.dumps(merge_status_response(pr)))
        else:
            rollup = {'state': pr['check_status']} if pr.get('check_status') else None
            value = {'number': pr['number'], 'url': pr['html_url'], 'headRefOid': pr['head']['sha'],
                     'reviewDecision': pr.get('review_decision'), 'mergeable': pr.get('mergeability', 'MERGEABLE'),
                     'commits': {'nodes': [{'commit': {'oid': pr['head']['sha'], 'statusCheckRollup': rollup}}]}}
            print(json.dumps({'data': {'repository': {'pullRequest': value}}}))
    elif endpoint.endswith('/merge'):
        number = int(endpoint.split('/')[-2])
        assert arg('--method') == 'PUT', args
        fields = dict(a.split('=', 1) for a in args if '=' in a)
        assert fields.get('merge_method') == 'squash' and fields.get('sha'), fields
        pr = refresh(next(p for p in prs if p['number'] == number))
        with (root / 'merge-attempts.jsonl').open('a') as log:
            log.write(json.dumps({'number': number, 'sha': fields['sha'], 'method': fields['merge_method']}) + '\n')
        if (root / 'merge-hold').exists():
            (root / 'merge-entered').touch()
            while (root / 'merge-hold').exists():
                time.sleep(0.05)
        # Simulate a retarget after all client policy/readiness checks, at the
        # mutation itself. Only the server's configured rule can prevent it.
        if (root / 'merge-retarget.json').exists():
            pr['base']['ref'] = json.loads((root / 'merge-retarget.json').read_text())['base']
            save()
        owned_source = (pr['head'].get('repo') or {}).get('full_name') == 'fixture/project'
        if owned_source:
            pr['head']['sha'] = subprocess.check_output(['/usr/bin/git', '--git-dir', str(root / 'remote.git'), 'rev-parse', pr['head']['ref']], text=True).strip()
        refusal = None
        if (root / 'merge-fail.json').exists():
            failure = json.loads((root / 'merge-fail.json').read_text())
            print(json.dumps(failure.get('body', {'message': 'Not allowed'})), file=sys.stderr)
            sys.exit(failure.get('exit', 1))
        if pr['state'] != 'open':
            refusal = 'HTTP 405'
        elif pr.get('merge_queue') or (root / 'merge-queue').exists():
            refusal = 'HTTP 405: The repository requires its merge queue'
        elif not (pr.get('squash_merge_allowed', True) and not (root / 'squash-disabled').exists()):
            refusal = 'HTTP 405: Squash merging is not allowed'
        elif pr.get('is_draft'):
            refusal = 'HTTP 405: A draft pull request cannot be merged'
        elif pr.get('review_decision') in ('CHANGES_REQUESTED', 'REVIEW_REQUIRED'):
            refusal = 'HTTP 422: A required review is outstanding'
        elif pr.get('merge_state_status', 'CLEAN') != 'CLEAN':
            refusal = 'HTTP 422: Required merge state is not clean'
        elif pr.get('check_status', 'SUCCESS') != 'SUCCESS' or not pr.get('check_contexts', 1):
            refusal = 'HTTP 422: Required status checks are not green'
        elif pr.get('mergeability') == 'CONFLICTING':
            refusal = 'HTTP 422: The pull request has conflicts'
        elif not owned_source:
            refusal = 'HTTP 422: The pull request head is not owned by the repository'
        elif destination_restricted(pr):
            refusal = 'HTTP 403: Repository rule violations: Cannot update this protected ref'
        elif pr['head']['sha'] != fields['sha']:
            refusal = 'HTTP 422: Head sha does not match the requested commit'
        if refusal:
            print(json.dumps({'message': refusal}), file=sys.stderr)
            sys.exit(1)
        commit = squash_merge(pr, fields['sha'])
        pr['state'] = 'merged'
        pr['merged_at'] = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
        pr['merge_commit_sha'] = commit
        save()
        if (root / 'merge-after-hold').exists():
            (root / 'merge-written').touch()
            while (root / 'merge-after-hold').exists():
                time.sleep(0.05)
        acknowledgement = {'sha': commit, 'merged': True, 'message': 'Merge completed'}
        if (root / 'merge-ack.json').exists():
            acknowledgement.update(json.loads((root / 'merge-ack.json').read_text()))
        print(json.dumps(acknowledgement))
    elif '/comments' in endpoint:
        number = int(endpoint.split('/')[-2])
        print(json.dumps(next(p for p in prs if p['number'] == number).get('comments', [])))
    elif '?' in endpoint:
        print(json.dumps([refresh(p) for p in prs if p['state'] == 'open' or 'state=all' in endpoint]))
    else:
        number = int(endpoint.split('/')[-1])
        print(json.dumps(refresh(next(p for p in prs if p['number'] == number))))
elif args[:2] == ['pr', 'create']:
    branch = arg('--head')
    assert not any(p['head']['ref'] == branch for p in prs), 'Duplicate PR creation attempted'
    number = len(prs) + 1
    pr = {'number': number, 'title': arg('--title'), 'body': Path(arg('--body-file')).read_text(), 'head': {'ref': branch, 'sha': '', 'repo': {'full_name': 'fixture/project'}}, 'base': {'ref': arg('--base'), 'repo': {'full_name': 'fixture/project'}}, 'html_url': f'https://github.com/fixture/project/pull/{number}', 'state': 'open', 'merged_at': None, 'additions': 1, 'deletions': 0, 'created_at': '2026-09-07T00:00:00Z'}
    if (root / 'pr-create-patch.json').exists():
        pr.update(json.loads((root / 'pr-create-patch.json').read_text()))
    prs.append(refresh(pr))
    save()
    with (root / 'publications.jsonl').open('a') as log:
        log.write(json.dumps({'action': 'create', 'number': number}) + '\n')
    if (root / 'publication-hold').exists():
        (root / 'publication-created').touch()
        while (root / 'publication-hold').exists():
            time.sleep(0.05)
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
