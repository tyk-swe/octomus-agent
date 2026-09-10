"""Shared synthetic repository work for protocol fixtures; no model calls."""
import os
import json
from pathlib import Path
import re
import time

root = Path(os.environ['OCTOMUS_FIXTURE'])

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

def respond(prompt, cwd, thread, file):
    match = re.search(r'fixture-file=([a-z-]+\.txt)', prompt)
    feature_file = match.group(1) if match else 'feature.txt'
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
    return answer
