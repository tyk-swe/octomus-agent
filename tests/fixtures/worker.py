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
        'problem_key': '', 'relevant_paths': [], 'reconsiders': [],
        'dependencies': [], 'prompt': 'Create feature.txt with fixed output and verify its contents. fixture-file=feature.txt',
        'decision': 'accepted', 'reason': 'Both independent reviews accept the concrete feature; no duplicates.'
    }

def proposals():
    first = proposal()
    if (root / 'proposal-override.json').exists():
        first.update(json.loads((root / 'proposal-override.json').read_text()))
    if (root / 'audit-absorbed').exists():
        first['problem_key'] = 'fixture-feature-output'
        return [first, {**first, 'id': 'd0-absorbed', 'title': 'Alternate wording for the fixture feature', 'decision': 'rejected', 'reason': 'Absorbed into d0-feature; both reviews support the consolidated scope.'}]
    if any((root / name).exists() for name in ['parallel','dependencies','chain','fork','unordered']):
        second = {**first, 'id': 'd0-followup', 'title': 'Complete the next fixture feature', 'problem': 'The next output capability is missing.', 'scope': 'Implement feature-next.txt only.', 'evidence': ['README.md: next feature output'], 'prompt': 'Implement the next fixture capability. fixture-file=feature-next.txt'}
        if any((root / name).exists() for name in ['dependencies','chain','fork']):
            second['dependencies'] = [first['id']]
        if (root / 'chain').exists() or (root / 'fork').exists():
            third = {**second, 'id': 'd0-third', 'title': 'Complete the third fixture feature', 'prompt': 'Implement the third capability. fixture-file=feature-third.txt', 'dependencies': [second['id'] if (root / 'chain').exists() else first['id']]}
            return [first, second, third]
        return [first, second]
    if (root / 'audit-decisions').exists():
        first = {**first, 'title': 'Audit fixture documentation', 'problem_key': 'audit-fixture-documentation', 'problem': 'Fixture guidance is incomplete.', 'scope': 'Document fixture behavior.'}
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
        if (root / 'failed-discovery').exists() and cwd.parent.name == 'discovery-0':
            answer = 'this discovery answer is not JSON'
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
    if prompt.startswith('Adversarial proposal') or prompt.startswith('Act as final orchestrator'):
        ids = sorted(set(re.findall(r'rediscover-([0-9a-f-]{36})', prompt)))
        for identity in ids:
            if prompt.startswith('Adversarial proposal'):
                answer['assessments'].append({'id': 'rediscover-' + identity, 'decision': 'accepted', 'reason': 'Fresh context assessed.'})
            else:
                accepted = {**proposal(), 'id': 'rediscover-' + identity, 'reconsiders': [identity], 'reason': 'Fresh evidence supports replacing stale work.'}
                if (root / 'obsolete').exists():
                    accepted.update(decision='rejected', reason='The objective is obsolete in the new context.')
                answer['proposals'].append(accepted)
        if ids and prompt.startswith('Act as final orchestrator'):
            for candidate in answer['proposals']:
                if not candidate['reconsiders']:
                    candidate.update(decision='rejected', reason='Absorbed into the rediscovery task.')
    return answer
