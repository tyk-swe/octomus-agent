#!/usr/bin/env python3
"""Operational regressions using synthetic runners and a real temporary Git remote."""
import json
from pathlib import Path
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
from e2e import Service, setup, existing_pr, git, process_gone, usage_report, TOKEN


def run(mode):
    with tempfile.TemporaryDirectory(prefix='octomus-hardening-') as directory:
        root = Path(directory)
        setup(root)
        if mode in ['chain', 'fork', 'unordered', 'dependency-rollback', 'publication-body-edit', 'publication-secret-followup']:
            existing_pr(root)
        (root / mode).touch()
        if mode == 'dependency-rollback':
            (root / 'chain').touch()
        if mode == 'audit-absorbed':
            (root / 'idle').touch()
        if mode in ['reconcile-controls', 'archive-uncertain']:
            (root / 'publication-body').touch()
        if mode == 'published-trimmed-title':
            (root / 'proposal-override.json').write_text(json.dumps({'title': '\t Complete the fixture feature \u2003', 'problem_key': 'original-feature-key'}))
        if mode in ['publication-secret', 'publication-secret-followup']:
            # Clearly synthetic secrets only: a token-patterned value and the
            # fixture operator token's value, which the service environment
            # legitimately carries.
            (root / 'proposal-override.json').write_text(json.dumps({
                'title': 'Complete the fixture feature ghp_fixturePublicationSecret0001',
                'problem': f'Missing output; leaked environment value {TOKEN} and ghp_fixturePublicationSecret0001'}))
        service = Service(root)
        try:
            service.start()
            if mode in ['live-budget', 'stale-retry', 'supersede', 'obsolete', 'interrupt-planning', 'cancel-route']:
                (root / 'audit-hold').touch()
            service.configure()
            if mode == 'reconcile-controls':
                task = service.wait(service.terminal_task, 'uncertain publication')
                assert task['status'] == 'blocked' and task['output_commit'], task
                service.wait(lambda: service.request('/state')['control']['paused'] and service.request('/state')['active_tasks'] == 0, 'uncertain publication paused')
                # First preserve an invalid remote identity to exercise failure cleanup,
                # then restore it and reconcile the already delivered commit.
                for succeeds in [False, True]:
                    if succeeds:
                        prs = json.loads((root / 'prs.json').read_text())
                        prs[0]['body'] = f'<!-- octomus:task:{task["id"]} -->'
                        (root / 'prs.json').write_text(json.dumps(prs))
                    (root / 'reconcile-entered').unlink(missing_ok=True)
                    (root / 'reconcile-hold').touch()
                    with ThreadPoolExecutor(max_workers=1) as pool:
                        # The held request stays open across the controls below,
                        # longer than the default 5 s socket timeout allows.
                        request = pool.submit(service.request, '/tasks/' + task['id'] + '/reconcile', 'POST', timeout=30)
                        try:
                            service.wait(lambda: (root / 'reconcile-entered').exists(), 'held reconciliation', seconds=3)
                            start = time.monotonic()
                            service.request('/control/pause', 'POST')
                            assert time.monotonic() - start < 1, 'Pause waited for remote reconciliation'
                            assert service.request('/state')['active_tasks'] == 1
                            assert service.request('/tasks/' + task['id'])['status'] == 'publishing'
                            for action in ['reconcile', 'cancel', 'archive', 'discard', 'retry']:
                                code, body = service.expect('/tasks/' + task['id'] + '/' + action, 'POST')
                                assert code == 409, (f'{action} changed a reserved publication', code, body)
                            service.request('/control/resume', 'POST')

                            def resumed():
                                state = service.request('/state')
                                return state if state['active_tasks'] == 1 and not state['cycle_active'] else None

                            state = service.wait(resumed, 'resumed publication')
                            assert state['active_tasks'] == 1 and not state['cycle_active']
                            service.request('/control/pause', 'POST')
                        finally:
                            (root / 'reconcile-hold').unlink(missing_ok=True)
                        assert request.result(timeout=5)['ok']
                    saved = service.request('/tasks/' + task['id'])
                    assert saved['status'] == ('published' if succeeds else 'blocked'), saved
                    assert service.request('/state')['active_tasks'] == 0
                    assert saved['output_commit'] == task['output_commit']
                    assert saved['attempts'] == task['attempts'] and saved['sessions'] == task['sessions']
                assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
                return
            if mode == 'archive-uncertain':
                task = service.wait(service.terminal_task, 'uncertain publication')
                assert task['status'] == 'blocked' and task['blocked_reason'] == 'publication_uncertain', task
                assert task['output_commit'] and task['pr_number'] is None, task
                service.wait(lambda: service.request('/state')['control']['paused'] and service.request('/state')['active_tasks'] == 0, 'uncertain publication paused')
                assert service.request('/state')['pr_capacity']['reserved'] == 1
                service.request('/tasks/' + task['id'] + '/archive', 'POST')
                archived = service.request('/tasks/' + task['id'])
                assert archived['status'] == 'cancelled' and archived['allowed_actions'] == ['discard'], archived
                # The remote side of the checkpoint settles closed. Remote
                # inspection must release the reservation rather than stranding
                # the slot on an archived task. The follow-up cycle discovers no
                # new work so the released count cannot race a fresh admission.
                prs = json.loads((root / 'prs.json').read_text())
                prs[0]['state'] = 'closed'
                (root / 'prs.json').write_text(json.dumps(prs))
                (root / 'idle').touch()
                service.request('/control/cycle', 'POST')
                service.wait(lambda: service.request('/state')['pr_capacity']['reserved'] == 0, 'archived checkpoint reservation released')
                service.stop(); service.start(); time.sleep(1)
                assert service.request('/state')['pr_capacity']['reserved'] == 0, 'released reservation resurrected at restart'
                assert service.request('/tasks/' + task['id'])['status'] == 'cancelled'
                return
            if mode == 'audit-absorbed':
                service.wait(lambda: (s := service.request('/state'))['cycles'] and s['control']['paused'] and not s['cycle_active'], 'initial idle cycle')
                (root / 'idle').unlink()
                service.request('/control/audit', 'POST')
                state = service.wait(lambda: (s := service.request('/state'))['cycles'][0]['mode'] == 'audit' and not s['cycle_active'] and s, 'audit with absorbed candidate')
                assert state['cycles'][0]['status'] == 'completed' and not state['tasks'], state
                audit = service.request('/cycles/' + state['cycles'][0]['id'])
                assert [p['decision'] for p in audit['proposals']] == ['accepted', 'rejected']
                assert len({p['problem_key'] for p in audit['proposals']}) == 1
                assert len(audit['decision_memory']) == 1 and audit['decision_memory'][0]['decision'] == 'accepted'
                service.stop(); service.start()
                service.request('/control/cycle', 'POST')
                task = service.wait(service.terminal_task, 'execution after audit absorption')
                assert task['status'] == 'published', task['error']
                assert len(service.request('/state')['tasks']) == 1
                assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
                return
            if mode in ['published-duplicate', 'published-case-change', 'published-trimmed-title']:
                task = service.wait(service.terminal_task, 'first delivery')
                assert task['status'] == 'published' and not task['proposal']['relevant_paths'], task
                service.wait(lambda: service.request('/state')['control']['paused'], 'first delivery paused')
                if mode == 'published-trimmed-title':
                    assert task['proposal']['title'] == '\t Complete the fixture feature \u2003'
                    (root / 'proposal-override.json').write_text(json.dumps({'title': 'complete the fixture feature', 'problem_key': 'different-feature-key'}))
                if mode == 'published-case-change':
                    c = service.request('/config')['config']
                    c['github_repo'] = 'Fixture/Project'
                    service.save_config(c)
                    service.request('/doctor', 'POST')
                    service.stop(); service.start()
                else:
                    checkout = root / 'checkout'
                    (checkout / 'unrelated.txt').write_text('Unrelated change on main\n')
                    git('add', '.', cwd=checkout); git('commit', '-m', 'Unrelated main change', cwd=checkout); git('push', 'origin', 'main', cwd=checkout)
                service.request('/control/cycle', 'POST')
                state = service.wait(lambda: (s := service.request('/state'))['cycles'][0]['id'] != task['cycle_id'] and not s['cycle_active'] and s, 'duplicate planning completes')
                cycle = service.request('/cycles/' + state['cycles'][0]['id'])
                assert (cycle['grounding']['revision'] == task['source_revision']) == (mode == 'published-case-change')
                assert any(pr['number'] == task['pr_number'] and pr['state'] == 'open' for pr in cycle['grounding']['prs'])
                assert cycle['status'] == 'failed' and 'duplicates recorded work' in cycle['error'], cycle
                assert len(state['tasks']) == 1
                assert len(state['prs']) == 1 and state['prs'][0]['delivered_head'] == task['output_commit']
                assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
                return
            if mode == 'interrupt-planning':
                service.wait(lambda: (root / 'audit-entered').exists(), 'one-shot planning entered')
                service.stop(crash=True)
                (root / 'audit-hold').unlink()
                service.start()

                def restored():
                    state = service.request('/state')
                    return state if state['control']['mode'] == 'paused' and state['cycles'] and state['cycles'][0]['status'] == 'interrupted' else None

                state = service.wait(restored, 'interrupted cycle after restart')
                assert state['control']['mode'] == 'paused'
                assert not state['tasks'] and len(usage_report(root)['admissions']) == 1
                return
            if mode in ['live-budget', 'stale-retry', 'supersede', 'obsolete', 'cancel-route']:
                service.wait(lambda: (root / 'audit-entered').exists(), 'held planning')
                service.request('/control/pause', 'POST')
                (root / 'audit-hold').unlink()
                service.wait(lambda: service.request('/state')['tasks'] and not service.request('/state')['cycle_active'], 'paused queue committed')
                row = service.request('/state')['tasks'][0]
                if mode == 'cancel-route':
                    original = service.request('/tasks/' + row['id'])
                    assert original['status'] == 'queued'
                    service.request('/tasks/' + original['id'] + '/cancel', 'POST')
                    cancelled = service.request('/tasks/' + original['id'])
                    assert not cancelled['rediscovery_requested'] and 'supersede' in cancelled['allowed_actions']
                    c = service.request('/config')['config']
                    c['tiers']['M'] = {**c['tiers']['M'], 'model': 'gpt-5.6-luna', 'effort': 'low'}
                    c['github_repo'] = 'Fixture/Project'
                    service.save_config(c)
                    service.request('/tasks/' + original['id'] + '/supersede', 'POST')
                    service.stop(); service.start()
                    service.request('/control/cycle', 'POST')
                    old = service.wait(lambda: (t := service.request('/tasks/' + original['id']))['superseded_by'] and t, 'cancelled task rediscovered on unchanged repository')
                    replacement_id = old['superseded_by'][0]
                    replacement = service.wait(lambda: (t := service.request('/tasks/' + replacement_id))['status'] == 'published' and t, 'replacement published with the new route')
                    assert old['status'] == 'cancelled' and not old['rediscovery_requested']
                    assert old['route'] == original['route'] and old['config'] == original['config']
                    assert replacement['source_revision'] == original['source_revision']
                    assert replacement['supersedes'] == [original['id']]
                    assert replacement['route'] == c['tiers']['M'] != original['route']
                    assert next(s for s in replacement['sessions'] if s['role'] == 'executor')['route'] == c['tiers']['M']
                    assert len(service.request('/state')['tasks']) == 2
                    assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
                    return
                if mode == 'live-budget':
                    c = service.request('/config')['config']
                    c['max_sessions_per_day'] = service.request('/state')['sessions_today']
                    service.save_config(c)
                    service.stop(); service.start()
                    code, body = service.expect('/control/cycle', 'POST')
                    assert code == 409, ('Unaffordable Run once was accepted', code, body)
                    service.request('/control/resume', 'POST')
                    task = service.wait(service.terminal_task, 'live admission denied')
                    assert task['blocked_reason'] == 'budget_exhausted'
                    assert service.request('/state')['sessions_today'] == c['max_sessions_per_day']
                    assert task['config']['max_sessions_per_day'] == 150
                    service.request('/control/pause', 'POST')
                    service.wait(lambda: service.request('/state')['active_tasks'] == 0, 'exhausted task stopped')
                    assert len(service.request('/state')['cycles']) == 1
                    c['max_sessions_per_day'] += 20
                    service.save_config(c)
                    service.request('/tasks/' + task['id'] + '/retry', 'POST')
                    service.request('/control/resume', 'POST')
                    task = service.wait(service.terminal_task, 'raised live policy permits retry')
                    assert task['status'] == 'published', task['error']
                    return
                checkout = root / 'checkout'
                (checkout / 'context.txt').write_text('New repository context\n')
                git('add', '.', cwd=checkout); git('commit', '-m', 'Change context', cwd=checkout); git('push', 'origin', 'main', cwd=checkout)
                service.request('/control/cycle', 'POST')
                task = service.wait(service.terminal_task, 'stale task blocked')
                assert task['blocked_reason'] == 'stale_base' and 'retry' not in task['allowed_actions']
                assert service.request('/state')['sessions_today'] == 13
                code, body = service.expect('/tasks/' + task['id'] + '/retry', 'POST')
                assert code == 409, ('Stale task retry was accepted', code, body)
                if mode == 'stale-retry':
                    return
                service.wait(lambda: service.request('/state')['control']['paused'], 'stale drain paused')
                service.request('/tasks/' + task['id'] + '/supersede', 'POST')
                service.request('/control/cycle', 'POST')
                service.wait(lambda: not service.request('/state')['cycle_active'] and service.request('/tasks/' + task['id'])['rediscovery_result'], 'rediscovery resolved')
                old = service.request('/tasks/' + task['id'])
                assert old['status'] == 'cancelled' and not old['rediscovery_requested']
                if mode == 'obsolete':
                    assert not old['superseded_by'] and old['rediscovery_result'].startswith('rejected:')
                else:
                    replacement = service.request('/tasks/' + old['superseded_by'][0])
                    assert replacement['source_revision'] != old['source_revision'] and replacement['supersedes'] == [old['id']]
                    service.wait(lambda: service.request('/tasks/' + replacement['id'])['status'] == 'published', 'replacement published')
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
                prs = json.loads((root / 'prs.json').read_text())
                prs[0].update(state='closed', merged_at='2026-09-10T00:00:00Z')
                (root / 'prs.json').write_text(json.dumps(prs))
                service.start()
                service.wait(lambda: service.request('/state')['merged_prs'] == 1, 'merge outcome reconciled from old open record')
                assert service.request('/tasks/' + task['id'])['status'] == 'published'
                assert service.request('/state')['control']['paused']
                return
            if mode in ['fork', 'unordered']:
                service.wait(lambda: (state := service.request('/state'))['cycles'] and state['cycles'][0]['status'] == 'failed', 'invalid branch plan rejected')
                assert not service.request('/state')['tasks'] and not (root / 'publications.jsonl').exists()
                return
            if mode == 'chain':
                service.wait(lambda: service.request('/state')['counts'].get('published') == 3, 'three branch tasks delivered')
                tasks = [service.request('/tasks/' + r['id']) for r in service.request('/state')['tasks']]
                for t in tasks:
                    if t['proposal']['dependencies']:
                        predecessor = next(x for x in tasks if x['id'] == t['proposal']['dependencies'][0])
                        assert t['source_revision'] == predecessor['output_commit']
                first = next(t for t in tasks if not t['proposal']['dependencies'])
                service.request('/tasks/' + first['id'] + '/archive', 'POST')
                service.stop(); service.start()
                service.wait(lambda: bool(service.request('/state')['prs']), 'archived predecessor observation')
                time.sleep(1)
                assert not service.request('/state')['prs'][0]['external_head_movement'], 'Archival must not replace the latest known delivery head'
            elif mode == 'dependency-rollback':
                # The first follow-up lands on the shared branch; the remote then
                # reports the branch rewound to its parent, so the delivered
                # commit is no longer an ancestor of the head. Both dependents
                # must block on the dependency instead of planning over it.
                service.wait(lambda: service.request('/state')['control']['paused'], 'rollback drain paused')
                tasks = [service.request('/tasks/' + r['id']) for r in service.request('/state')['tasks']]
                published = [t for t in tasks if t['status'] == 'published']
                blocked = [t for t in tasks if t['status'] == 'blocked']
                assert len(tasks) == 3 and len(published) == 1 and len(blocked) == 2, [(t['status'], t['blocked_reason']) for t in tasks]
                assert not published[0]['proposal']['dependencies']
                assert all(t['blocked_reason'] == 'dependency_blocked' for t in blocked), [t['blocked_reason'] for t in blocked]
                assert not (root / 'dependency-rollback').exists(), 'the injected rollback never fired'
                assert git('rev-parse', 'octomus/existing', cwd=root / 'remote.git') == published[0]['source_revision']
                actions = [json.loads(line)['action'] for line in (root / 'publications.jsonl').read_text().splitlines()]
                assert actions == ['comment'], actions
                return
            elif mode == 'publication-body-edit':
                # The maintainer's concurrent description edit survives the
                # follow-up: evidence lands as a comment, never a body rewrite.
                task = service.wait(service.terminal_task, 'follow-up publication')
                assert task['status'] == 'published' and task['pr_number'] == 42, task
                pr = json.loads((root / 'prs.json').read_text())[0]
                assert pr['body'].startswith('Maintainer edit during follow-up.'), pr['body']
                assert '<!-- octomus:task:earlier -->' in pr['body'], pr['body']
                assert any(task['id'] in c['body'] for c in pr['comments']), pr['comments']
                actions = [json.loads(line)['action'] for line in (root / 'publications.jsonl').read_text().splitlines()]
                assert actions == ['comment'], actions
                service.wait(lambda: service.request('/state')['control']['paused'], 'one-shot paused')
                return
            elif mode in ['publication-secret', 'publication-secret-followup']:
                # The same secret policy covers a new PR's title and body and
                # an owned PR's append-only follow-up comment; the durable
                # record keeps the canonical private text.
                task = service.wait(service.terminal_task, mode)
                assert task['status'] == 'published', task
                prs = json.loads((root / 'prs.json').read_text())
                assert len(prs) == 1, prs
                pr = prs[0]
                if mode == 'publication-secret':
                    sent = pr['title'] + '\n' + pr['body']
                    assert '[redacted]' in pr['title'], pr['title']
                else:
                    assert task['pr_number'] == 42, task
                    assert pr['body'].startswith('Existing context.'), pr['body']
                    assert len(pr.get('comments', [])) == 1, pr['comments']
                    sent = pr['comments'][0]['body']
                assert TOKEN not in sent and 'ghp_fixturePublicationSecret0001' not in sent, sent
                assert '[redacted]' in sent, sent
                assert f'<!-- octomus:task:{task["id"]} -->' in sent, sent
                assert f'Reviewed commit: `{task["output_commit"]}`' in sent, sent
                assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
                if mode == 'publication-secret':
                    service.stop()
                    import sqlite3
                    with sqlite3.connect(root / '.octomus/state.db') as db:
                        raw = db.execute("SELECT data FROM records WHERE kind='task' AND id=?", (task['id'],)).fetchone()[0]
                    canonical = json.loads(raw)
                    assert 'ghp_fixturePublicationSecret0001' in canonical['proposal']['title'], canonical['proposal']
                    assert TOKEN in canonical['proposal']['problem'], canonical['proposal']
                return
            else:
                task = service.wait(service.terminal_task, mode)
                assert task['status'] == 'blocked' and task['output_commit'] and task['blocked_reason'] == 'publication_uncertain', task
                assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
            service.wait(lambda: service.request('/state')['control']['paused'], 'one-shot completion')
            cycles = len(service.request('/state')['cycles'])
            # Past one scheduler tick (schedulerInterval in internal/engine/engine.go).
            service.stop(); service.start(); time.sleep(1.2)
            assert service.request('/state')['control']['mode'] == 'paused'
            assert len(service.request('/state')['cycles']) == cycles
        finally:
            service.stop()
            service.log.close()

def reconciliation_deadline():
    with tempfile.TemporaryDirectory(prefix='octomus-reconciliation-deadline-') as directory:
        root = Path(directory)
        setup(root)
        (root / 'publication-body').touch()
        (root / 'failed-executor-start').touch()
        service = Service(root)
        try:
            service.start()
            service.configure()
            task = service.wait(service.terminal_task, 'failed executor start')
            assert task['status'] == 'blocked' and task['blocked_reason'] == 'runner_unavailable', task
            service.wait(lambda: service.request('/state')['control']['paused'] and service.request('/state')['active_tasks'] == 0, 'failed executor paused')
            (root / 'failed-executor-start').unlink()
            config = service.request('/config')['config']
            config.update(session_timeout_seconds=10, task_timeout_seconds=10)
            service.save_config(config)
            service.request('/tasks/' + task['id'] + '/retry', 'POST')
            service.request('/control/cycle', 'POST')
            task = service.wait(service.terminal_task, 'publication retry with saved deadline')
            assert task['blocked_reason'] == 'publication_uncertain', task
            assert task['config']['task_timeout_seconds'] == 120
            assert task['attempt_policy']['task_timeout_seconds'] == 10
            service.wait(lambda: service.request('/state')['control']['paused'] and service.request('/state')['active_tasks'] == 0, 'publication retry paused')
            config.update(session_timeout_seconds=30, task_timeout_seconds=120)
            service.save_config(config)
            prs = json.loads((root / 'prs.json').read_text())
            prs[0]['body'] = f'<!-- octomus:task:{task["id"]} -->'
            (root / 'prs.json').write_text(json.dumps(prs))
            # Each command fits its 10-second limit; their total exceeds the task's.
            (root / 'reconcile-delay').write_text('6')
            start = time.monotonic()
            with ThreadPoolExecutor(max_workers=1) as pool:
                # The client gives up after its 5 s socket timeout, before the
                # 7 s wait below: reconciliation must outlive the disconnect.
                request = pool.submit(service.request, '/tasks/' + task['id'] + '/reconcile', 'POST', timeout=5)
                service.wait(lambda: (root / 'reconcile-processes.jsonl').exists(), 'slow reconciliation entered', seconds=3)
                try:
                    request.result(timeout=7)
                    raise AssertionError('The fixture request should disconnect before reconciliation finishes')
                except TimeoutError:
                    assert request.done(), 'The HTTP client did not disconnect'
                state = service.request('/state')
                assert state['active_tasks'] == 1
                assert service.request('/tasks/' + task['id'])['status'] == 'publishing'
                saved = service.wait(lambda: (t := service.request('/tasks/' + task['id']))['status'] != 'publishing' and t, 'reconciliation task deadline', seconds=10)
            elapsed = time.monotonic() - start
            assert saved['status'] == 'blocked' and saved['blocked_reason'] == 'timeout', saved
            assert 'Task time limit exceeded' in saved['error']
            assert 9 <= elapsed < 12, f'Reconciliation took {elapsed:.2f}s for a 10s task limit'
            assert service.request('/state')['active_tasks'] == 0
            for field in ['output_commit', 'attempts', 'sessions', 'reviews', 'verification', 'config', 'attempt_policy']:
                assert saved[field] == task[field], field
            calls = [json.loads(line) for line in (root / 'reconcile-processes.jsonl').read_text().splitlines()]
            assert len(calls) == 2 and calls[0]['args'][:2] == ['auth', 'status'] and calls[1]['args'][0] == 'api', calls
            for call in calls:
                for pid in [call['pid'], call['child_pid']]:
                    service.wait(lambda: process_gone(pid), f'reconciliation process {pid} stopped', seconds=2)
            (root / 'reconcile-delay').unlink()
            service.request('/tasks/' + task['id'] + '/retry', 'POST')
            service.request('/control/cycle', 'POST')
            recovered = service.wait(service.terminal_task, 'scheduler dispatch after reconciliation timeout')
            assert recovered['status'] == 'published', recovered
            assert recovered['output_commit'] == task['output_commit'] and recovered['sessions'] == task['sessions']
            assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
        finally:
            service.stop()
            service.log.close()


if __name__ == '__main__':
    for mode in ['reconcile-controls', 'archive-uncertain', 'published-duplicate', 'published-case-change', 'published-trimmed-title', 'cancel-route', 'audit-absorbed', 'live-budget', 'stale-retry', 'supersede', 'obsolete', 'interrupt-planning', 'chain', 'dependency-rollback', 'fork', 'unordered', 'pr-outcome', 'publication-race', 'publication-body', 'publication-base', 'publication-owner', 'publication-body-edit', 'publication-secret', 'publication-secret-followup']:
        run(mode)
        print(f'PASS hardening {mode}', flush=True)
    reconciliation_deadline()
    print('PASS hardening reconciliation deadline and process cleanup', flush=True)
