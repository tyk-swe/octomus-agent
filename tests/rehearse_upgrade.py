#!/usr/bin/env python3
"""Rehearse a release-image upgrade on isolated fixture state, never an operator VM.

Requires Docker Engine 28+ and both pairs of images already pulled/built. Uses the
shipped compose topology and the v0.1.0 golden (produced by the released binary).
No real GitHub account, provider login, model turn or publication is used. The
isolated runner has a dummy API-key fixture for account/catalogue reads only.
The only control-plane command overrides are local Git/GitHub fixtures.

python3 tests/rehearse_upgrade.py --new-agent octomus-agent:v0.2.0-rehearsal \
    --new-sandbox octomus-sandbox:v0.2.0-rehearsal
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import sys
import tempfile

import e2e_sandbox
from e2e import existing_pr
from harness import PROJECT, local_urlopen, setup


def compatible(value):
    """New optional PR fields must not change any existing saved evidence."""
    if isinstance(value, dict):
        added = {'review_decision', 'check_status', 'mergeability', 'status_source', 'status_observed_at'}
        return {k: compatible(v) for k, v in value.items()
                if k != 'generated_at' and not (k in added and v == '')}
    if isinstance(value, list):
        return [compatible(v) for v in value]
    return value


def capture(stack):
    state = stack.request('/state')
    tasks = {r['id']: stack.request('/tasks/' + r['id']) for r in state['tasks']}
    cycles = {r['id']: stack.request('/cycles/' + r['id']) for r in state['cycles']}
    evidence = {r['id']: stack.request('/cycles/' + r['id'] + '/evidence') for r in state['cycles']}
    assert len(tasks) == 3 and len(cycles) == 2 and len(state['prs']) == 1
    assert all(t['status'] == 'published' and t['pr_number'] == 42 for t in tasks.values())
    return compatible({'tasks': tasks, 'cycles': cycles, 'evidence': evidence})


def run(args):
    # This fixture only owns the managed host daemon, never a selected remote context.
    for name in ['DOCKER_CONTEXT', 'DOCKER_TLS', 'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH']:
        os.environ.pop(name, None)
    os.environ['DOCKER_HOST'] = 'unix:///var/run/docker.sock'
    docker = e2e_sandbox.docker
    report = {'old_agent': args.old_agent, 'old_sandbox': args.old_sandbox,
              'new_agent': args.new_agent, 'new_sandbox': args.new_sandbox,
              'source': 'internal/store/testdata/state-v0.1.0.db',
              'source_sha256': hashlib.sha256((PROJECT / 'internal/store/testdata/state-v0.1.0.db').read_bytes()).hexdigest()}
    for image in [args.old_agent, args.old_sandbox, args.new_agent, args.new_sandbox]:
        report.setdefault('image_ids', {})[image] = docker('image', 'inspect', '--format', '{{.Id}}', image).stdout.strip()
    with tempfile.TemporaryDirectory(prefix='octomus-upgrade-compose-') as tmp, tempfile.TemporaryDirectory(prefix='octomus-upgrade-secrets-') as secret_dir:
        root = Path(tmp)
        setup(root)
        existing_pr(root)
        prs = json.loads((root / 'prs.json').read_text())
        prs[0]['base']['repo'] = {'full_name': 'fixture/project'}
        (root / 'pr-detail.json').write_text(json.dumps(prs[0]))
        (root / 'pr-inventory.json').write_text(json.dumps(prs))
        pr = prs[0]
        status = {'number': 42, 'url': pr['html_url'], 'headRefOid': pr['head']['sha'],
                  'reviewDecision': 'APPROVED', 'mergeable': 'MERGEABLE',
                  'commits': {'nodes': [{'commit': {'oid': pr['head']['sha'], 'statusCheckRollup': {'state': 'SUCCESS'}}}]}}
        (root / 'pr-status.json').write_text(json.dumps({'data': {'repository': {'pullRequest': status}}}))
        # A shell-only fixture works in the actual release control image: no Python
        # installation or modification of its binary/runtime/isolation boundary.
        (root / 'bin/gh').write_text('''#!/bin/sh
set -eu
case "$1/$2" in
  auth/status) printf 'Authenticated fixture operator\\n' ;;
  api/graphql) cat "$OCTOMUS_FIXTURE/pr-status.json" ;;
  api/--paginate) cat "$OCTOMUS_FIXTURE/pr-inventory.json" ;;
  api/repos/fixture/project/pulls/42) cat "$OCTOMUS_FIXTURE/pr-detail.json" ;;
  *) printf 'Unexpected fixture gh request\\n' >&2; exit 1 ;;
esac
''')
        (root / 'bin/gh').chmod(0o755)
        (root / 'seed').mkdir()
        shutil.copyfile(PROJECT / report['source'], root / 'seed/state.db')
        e2e_sandbox.IMAGES.update(control=args.old_agent, sandbox=args.old_sandbox)
        stack = e2e_sandbox.Stack(root, Path(secret_dir))
        def data_command(command, image=args.old_agent):
            return docker('run', '--rm', '--network', 'none', '--user', '0',
                          '-v', f'{stack.volume("data")}:/data', '-v', f'{root}:/fixture',
                          '--entrypoint', 'sh', image, '-c', command)
        def version(image):
            return docker('run', '--rm', '--network', 'none', image, '--version').stdout.strip()
        def running_version():
            with local_urlopen(f'http://127.0.0.1:{stack.port}/healthz', timeout=5) as response:
                return json.load(response)['version']
        try:
            stack.prepare(own_remote=True)
            override = json.loads(stack.override.read_text())
            # Production images do not have the sandboxfixture-only mount extension.
            broker = override['services']['sandboxd']['environment']
            broker.pop('OCTOMUS_SANDBOX_FIXTURE_VOLUME')
            broker.pop('OCTOMUS_SANDBOX_FIXTURE_PATH')
            # Use the shipped image selectors for every phase, including the
            # broker, egress and login profiles; fixed test overrides mask them.
            broker.pop('OCTOMUS_SANDBOX_IMAGE')
            for service in override['services'].values():
                service.pop('image')
            stack.env.update(OCTOMUS_IMAGE=args.old_agent, OCTOMUS_SANDBOX_IMAGE=args.old_sandbox)
            stack.override.write_text(json.dumps(override))
            # Exercise the real app-server account/catalogue protocol without
            # reading an operator credential or making an inference request.
            docker('run', '--rm', '--network', 'none', '--user', '0',
                   '-v', f'{stack.volume("runner")}:/runner', '--entrypoint', 'sh', args.old_agent,
                   '-c', '''mkdir -p /runner/codex && printf '%s\\n' '{"OPENAI_API_KEY":"fixture-only-not-a-real-key"}' > /runner/codex/auth.json && chown 10001:10001 /runner && chown -R 10001:10001 /runner/codex && chmod 600 /runner/codex/auth.json''')
            report['authentication'] = 'synthetic API-key fixture; no real credentials or inference'
            data_command('install -o 10001 -g 10001 -m 600 /fixture/seed/state.db /data/state.db')
            report['old_version'] = version(args.old_agent)
            assert report['old_version'] == 'octomus-agent 0.1.0', report
            stack.up()
            report['old_runtime_version'] = running_version()
            assert report['old_runtime_version'] == '0.1.0', report
            stack.configure()
            view = stack.request('/config')
            view['config']['max_sessions_per_day'] = 1000
            stack.request('/config', 'PUT', {'expected_revision': view['revision'], 'config': view['config']})
            doctor = stack.request('/doctor', 'POST', timeout=120)
            assert doctor['ok'], doctor
            report['old_connection_ok'] = True
            before = capture(stack)
            old_probe = stack.request('/sandbox/self-test', 'POST', timeout=120)
            assert old_probe['passed'] and all(c['passed'] for c in old_probe['checks']), old_probe
            report['old_containment_checks'] = len(old_probe['checks'])
            stack.compose('stop', '--timeout', '30')
            # Stop first and back up the whole data directory, not a live DB/WAL copy.
            data_command(f'mkdir /fixture/manual-backup && cp -a /data/. /fixture/manual-backup/ && chown -R {os.getuid()}:{os.getgid()} /fixture/manual-backup')
            with sqlite3.connect(root / 'manual-backup/state.db') as db:
                assert db.execute('PRAGMA user_version').fetchone()[0] == 7
                assert db.execute('PRAGMA integrity_check').fetchall() == [('ok',)]
            report['manual_backup_version'] = 7
            report['manual_backup_sha256'] = hashlib.sha256((root / 'manual-backup/state.db').read_bytes()).hexdigest()
            stack.env.update(OCTOMUS_IMAGE=args.new_agent, OCTOMUS_SANDBOX_IMAGE=args.new_sandbox)
            e2e_sandbox.IMAGES.update(control=args.new_agent, sandbox=args.new_sandbox)
            report['new_version'] = version(args.new_agent)
            assert report['new_version'] == 'octomus-agent 0.2.0', report
            stack.up()
            report['new_runtime_version'] = running_version()
            assert report['new_runtime_version'] == '0.2.0', report
            doctor = stack.request('/doctor', 'POST', timeout=120)
            assert doctor['ok'], doctor
            report['new_connection_ok'] = True
            assert capture(stack) == before, 'saved tasks, cycles or evidence changed during upgrade'
            new_probe = stack.request('/sandbox/self-test', 'POST', timeout=120)
            assert new_probe['passed'] and all(c['passed'] for c in new_probe['checks']), new_probe
            report['new_containment_checks'] = len(new_probe['checks'])
            stack.compose('stop', '--timeout', '30')
            data_command(f'mkdir /fixture/upgraded && cp -p /data/state.db /fixture/upgraded/state.db && cp -p /data/state.db.v7-backup-* /fixture/upgraded/ && chown -R {os.getuid()}:{os.getgid()} /fixture/upgraded', args.new_agent)
            backups = list((root / 'upgraded').glob('state.db.v7-backup-*'))
            assert len(backups) == 1, backups
            assert backups[0].stat().st_mode & 0o777 == 0o600
            for path, expected in [(root / 'upgraded/state.db', 8), (backups[0], 7)]:
                with sqlite3.connect(path) as db:
                    assert db.execute('PRAGMA integrity_check').fetchall() == [('ok',)]
                    assert db.execute('PRAGMA user_version').fetchone()[0] == expected
            report.update(automatic_backup=backups[0].name, upgraded_schema_version=8,
                          saved_tasks=3, saved_cycles=2, evidence_unchanged=True)
            # Demonstrate the documented restore-based rollback with the actual old image.
            data_command('rm -f /data/state.db /data/state.db-wal /data/state.db-shm && install -o 10001 -g 10001 -m 600 /fixture/manual-backup/state.db /data/state.db', args.new_agent)
            stack.env.update(OCTOMUS_IMAGE=args.old_agent, OCTOMUS_SANDBOX_IMAGE=args.old_sandbox)
            e2e_sandbox.IMAGES.update(control=args.old_agent, sandbox=args.old_sandbox)
            stack.up()
            assert running_version() == '0.1.0'
            assert capture(stack) == before, 'restored v0.1.0 state differs from the original'
            report['rollback_verified'] = True
            print(json.dumps(report, indent=2))
        except BaseException as error:
            if isinstance(error, subprocess.CalledProcessError) and error.stderr:
                print(error.stderr, file=sys.stderr)
            print(stack.logs(), file=sys.stderr)
            raise
        finally:
            stack.teardown()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--old-agent', default='ghcr.io/tyk-swe/octomus-agent:0.1.0')
    parser.add_argument('--old-sandbox', default='ghcr.io/tyk-swe/octomus-sandbox:0.1.0')
    parser.add_argument('--new-agent', required=True)
    parser.add_argument('--new-sandbox', required=True)
    run(parser.parse_args())
