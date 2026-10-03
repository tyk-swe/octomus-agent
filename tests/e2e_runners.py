#!/usr/bin/env python3
"""Mixed-runner behavior with synthetic peers, SQLite and real local Git only."""
import functools
import json
from pathlib import Path
import shutil
import sys

from harness import configuration, fixture_service, git, route, run_selected, usage_report


def successful_workflow(mode):
    with fixture_service('octomus-runners-') as (root, service):
        preview = root / 'bin/opencode-preview'
        shutil.copy(root / 'bin/opencode', preview)
        models = service.request('/model-catalog', 'POST', {'backend': 'opencode', 'binary': str(preview)})
        assert any(m['available'] for m in models)
        assert not any(word in json.dumps(models) for word in ['fixture-credential', 'another-fixture-secret', 'PRIVATE_API_KEY'])
        assert service.request('/config')['config']['opencode_binary'] == 'opencode'
        assert not usage_report(root)['admissions'] and not (root / 'protocol.jsonl').exists()
        c = configuration(service, **({'planning': 'codex', 'reviewer': 'codex'} if mode == 'mixed' else {}))
        diagnostic = service.request('/doctor', 'POST')
        assert {d['backend'] for d in diagnostic['backends']} == ({'opencode'} if mode == 'opencode' else {'codex', 'opencode'})
        assert not (root / 'protocol.jsonl').exists()
        service.request('/control/cycle', 'POST')
        task = service.wait(service.terminal_task, f'{mode} delivery')
        assert task['status'] == 'published', task['error']
        assert task['route'] == c['tiers']['M']
        assert task['workspace'].endswith(f'tasks/{task["id"]}/workspace')
        assert len(task['reviews']) == 3 and len({r['session_id'] for r in task['reviews']}) == 3
        assert all(r['comparison_base'] == task['comparison_base'] for r in task['reviews'])
        repairs = [s for s in task['sessions'] if s['role'] == 'repair']
        assert len(repairs) == 1 and repairs[0]['route'] == c['repair_route']
        assert task['verification'][-1]['success'] and task['verification'][-1]['revision'] == task['output_commit']
        assert git('rev-parse', 'main', cwd=root / 'remote.git') == task['default_revision']
        assert len((root / 'publications.jsonl').read_text().splitlines()) == 1
        calls = [json.loads(line) for line in (root / 'protocol.jsonl').read_text().splitlines()]
        assert len({p['thread'] for p in calls if p['prompt'].startswith('Repair actionable')}) == 1
        report = usage_report(root)
        assert len(report['admissions']) == 19
        assert any(a['route']['backend'] == 'opencode' for a in report['admissions'])
        assert report['tasks'][0]['repair_route'] == c['repair_route']
        print(f'PASS {mode}: exact routes, fresh reviews, persistent repairs and verified delivery')


def failed_review(mode):
    with fixture_service('octomus-runner-failure-') as (root, service):
        c = configuration(service)
        c['tiers'] = {tier: route('opencode', planning=True) for tier in c['tiers']}
        service.save_config(c)
        (root / 'opencode-mode').write_text(mode)
        service.request('/control/cycle', 'POST')
        task = service.wait(service.terminal_task, f'{mode} blocked review')
        assert task['status'] == 'blocked', task
        assert task['error'] and not task['reviews'] and not task['verification']
        assert Path(task['workspace']).is_dir() and not (root / 'publications.jsonl').exists()
        print(f'PASS OpenCode {mode}: failed review cannot authorize publication')


SCENARIOS = [
    *[(mode, functools.partial(successful_workflow, mode)) for mode in ['opencode', 'mixed']],
    ('wrong-model', functools.partial(failed_review, 'wrong-model')),
]


if __name__ == '__main__':
    run_selected('runners', SCENARIOS, sys.argv[1:])
