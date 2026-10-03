#!/usr/bin/env python3
"""Operational regressions using synthetic runners and a real temporary Git remote."""
import functools
import json
import sqlite3
import sys

from harness import TOKEN, existing_pr, fixture_service, git, run_selected, update_prs


def run(mode):
    def prepare(root):
        if mode == 'chain':
            existing_pr(root)
        (root / mode).touch()
        if mode == 'publication-secret':
            (root / 'proposal-override.json').write_text(json.dumps({
                'title': 'Complete the fixture feature ghp_fixturePublicationSecret0001',
                'problem': f'Missing output; leaked environment value {TOKEN} and ghp_fixturePublicationSecret0001'}))

    with fixture_service('octomus-hardening-', prepare) as (root, service):
        service.configure()
        if mode == 'published-duplicate':
            task = service.wait(service.terminal_task, 'first delivery')
            assert task['status'] == 'published' and not task['proposal']['relevant_paths'], task
            service.wait(lambda: service.request('/state')['control']['paused'], 'first delivery paused')
            checkout = root / 'checkout'
            (checkout / 'unrelated.txt').write_text('Unrelated change on main\n')
            git('add', '.', cwd=checkout); git('commit', '-m', 'Unrelated main change', cwd=checkout); git('push', 'origin', 'main', cwd=checkout)
            service.request('/control/cycle', 'POST')
            state = service.wait(lambda: (s := service.request('/state'))['cycles'][0]['id'] != task['cycle_id'] and not s['cycle_active'] and s, 'duplicate planning completes')
            cycle = service.request('/cycles/' + state['cycles'][0]['id'])
            assert cycle['grounding']['revision'] != task['source_revision']
            assert any(pr['number'] == task['pr_number'] and pr['state'] == 'open' for pr in cycle['grounding']['prs'])
            assert cycle['status'] == 'failed' and 'duplicates recorded work' in cycle['error'], cycle
            assert len(state['tasks']) == 1
            assert len(state['prs']) == 1 and state['prs'][0]['delivered_head'] == task['output_commit']
            assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
            return
        if mode == 'pr-outcome':
            task = service.wait(service.terminal_task, 'published task')
            assert task['status'] == 'published', task['error']
            service.wait(lambda: service.request('/state')['control']['paused'], 'one-shot paused')
            assert service.request('/state')['prs'][0]['pr']['head'] == task['output_commit']
            service.stop(); service.start()
            service.wait(lambda: service.request('/state')['control']['context_fingerprint'], 'initial repository observation')
            fingerprint = service.request('/state')['control']['context_fingerprint']
            deadline = service.request('/state')['control']['next_cycle_at']
            service.stop()
            remote = str(root / 'remote.git')
            tree = git('--git-dir', remote, 'rev-parse', task['output_commit'] + '^{tree}', cwd=root)
            advanced = git('--git-dir', remote, '-c', 'user.name=External', '-c', 'user.email=fixture@example.com', 'commit-tree', tree, '-p', task['output_commit'], '-m', 'External follow-up', cwd=root)
            git('--git-dir', remote, 'update-ref', 'refs/heads/' + task['branch'], advanced, cwd=root)
            service.start()
            service.wait(lambda: service.request('/state')['prs'][0]['external_head_movement'], 'external head observed while paused')
            service.wait(lambda: service.request('/state')['control']['context_fingerprint'] != fingerprint, 'changed context observed')
            assert service.request('/state')['control']['next_cycle_at'] == deadline, 'Observations must preserve ordinary cadence'
            service.stop()
            update_prs(root, lambda prs: prs[0].update(state='closed', merged_at='2026-09-10T00:00:00Z'))
            service.start()
            service.wait(lambda: service.request('/state')['merged_prs'] == 1, 'merge outcome reconciled from old open record')
            assert service.request('/tasks/' + task['id'])['status'] == 'published'
            assert service.request('/state')['control']['paused']
            return
        if mode == 'publication-secret':
            task = service.wait(service.terminal_task, mode)
            assert task['status'] == 'published', task
            prs = json.loads((root / 'prs.json').read_text())
            assert len(prs) == 1, prs
            pr = prs[0]
            sent = pr['title'] + '\n' + pr['body']
            assert '[redacted]' in pr['title'], pr['title']
            assert TOKEN not in sent and 'ghp_fixturePublicationSecret0001' not in sent, sent
            assert '[redacted]' in sent, sent
            assert f'<!-- octomus:task:{task["id"]} -->' in sent, sent
            assert f'Reviewed commit: `{task["output_commit"]}`' in sent, sent
            assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
            messages = git('log', '--format=%B', task['branch'], cwd=root / 'remote.git')
            assert TOKEN not in messages and 'ghp_fixturePublicationSecret0001' not in messages, messages
            assert 'Complete the fixture feature [redacted]' in messages, messages
            service.stop()
            with sqlite3.connect(root / '.octomus/state.db') as db:
                raw = db.execute("SELECT data FROM records WHERE kind='task' AND id=?", (task['id'],)).fetchone()[0]
            canonical = json.loads(raw)
            assert 'ghp_fixturePublicationSecret0001' in canonical['proposal']['title'], canonical['proposal']
            assert TOKEN in canonical['proposal']['problem'], canonical['proposal']
            return
        service.wait(lambda: service.request('/state')['counts'].get('published') == 3, 'three branch tasks delivered')
        tasks = [service.request('/tasks/' + r['id']) for r in service.request('/state')['tasks']]
        for t in tasks:
            if t['proposal']['dependencies']:
                predecessor = next(x for x in tasks if x['id'] == t['proposal']['dependencies'][0])
                assert t['source_revision'] == predecessor['output_commit']
        first = next(t for t in tasks if not t['proposal']['dependencies'])
        service.request('/tasks/' + first['id'] + '/archive', 'POST')
        delivered = {p['pr']['number']: p['observed_at'] for p in service.request('/state')['prs']}
        service.stop(); service.start()
        def refreshed_prs():
            prs = service.request('/state')['prs']
            return bool(prs) and all(p['observed_at'] != delivered.get(p['pr']['number']) for p in prs)
        service.wait(refreshed_prs, 'refreshed observation of the archived predecessor')
        assert not service.request('/state')['prs'][0]['external_head_movement'], 'Archival must not replace the latest known delivery head'
        service.wait(lambda: service.request('/state')['control']['paused'], 'one-shot completion')
        cycles = len(service.request('/state')['cycles'])
        observed = service.request('/state')['pr_capacity']['observed_at']
        service.stop(); service.start()
        service.wait(lambda: service.request('/state')['pr_capacity']['observed_at'] != observed, 'fresh PR observation after restart')
        assert service.request('/state')['control']['mode'] == 'paused'
        assert len(service.request('/state')['cycles']) == cycles


def hardening(mode):
    run(mode)
    print(f'PASS hardening {mode}', flush=True)


SCENARIOS = [(mode, functools.partial(hardening, mode)) for mode in ['published-duplicate', 'chain', 'pr-outcome', 'publication-secret']]


if __name__ == '__main__':
    run_selected('hardening', SCENARIOS, sys.argv[1:])
