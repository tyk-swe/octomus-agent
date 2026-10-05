#!/usr/bin/env python3
"""Generate a golden state database from a released ref for the upgrade tests.

    python3 scripts/golden-state.py --ref v0.1.0 --scenario chain --output internal/store/testdata/state-v0.1.0.db

The ref is extracted with `git archive` into a temporary directory (no worktree, no .git
changes), built there, and the named e2e scenario runs against that binary with the ref's
own harness. Every Service.stop snapshots the fixture's state.db through Python sqlite3's
backup API, verifies integrity and user_version, and the last snapshot becomes the output
after a secret scan. Provenance prints as JSON on stdout.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
from contextlib import closing
from datetime import datetime, timezone

REPO = Path(__file__).resolve().parents[1]


def run(args, cwd, **kwargs):
    # Build chatter goes to stderr so stdout carries only the provenance JSON.
    return subprocess.run(args, cwd=cwd, check=True, stdout=sys.stderr, **kwargs)


def extract(ref, dst):
    commit = subprocess.check_output(['git', 'rev-parse', f'{ref}^{{commit}}'], cwd=REPO, text=True).strip()
    archive = subprocess.run(['git', 'archive', commit], cwd=REPO, capture_output=True, check=True)
    subprocess.run(['tar', '-x', '-C', str(dst)], input=archive.stdout, check=True)
    return commit


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--ref', required=True, help='git ref whose binary produces the golden state')
    parser.add_argument('--scenario', required=True, help='e2e scenario name to run (as registered in tests/e2e.py)')
    parser.add_argument('--output', required=True, type=Path, help='destination for the golden state.db')
    parser.add_argument('--expect-version', type=int, default=7, help='schema user_version the golden must carry')
    args = parser.parse_args()
    output = args.output.resolve()

    with tempfile.TemporaryDirectory(prefix='octomus-golden-') as tmp:
        work = Path(tmp)
        src = work / 'src'
        src.mkdir()
        commit = extract(args.ref, src)

        run(['npm', 'ci', '--prefix', str(src / 'web')], cwd=src)
        run(['make', '-C', str(src), 'build'], cwd=src)
        binary = src / 'bin' / 'octomus-agent'
        version = subprocess.check_output([str(binary), '--version'], text=True).strip()
        expected_version = (src / 'VERSION').read_text().strip()
        if version != f'octomus-agent {expected_version}':
            raise SystemExit(f'--version printed {version!r}; expected octomus-agent {expected_version}')

        os.environ['OCTOMUS_TEST_BINARY'] = str(binary)
        sys.path.insert(0, str(src / 'tests'))
        import harness  # noqa: E402
        import e2e  # noqa: E402

        snapshot = work / 'snapshot.db'
        original_stop = harness.Service.stop

        def stop_and_snapshot(self, *a, **kw):
            original_stop(self, *a, **kw)
            source = self.root / '.octomus' / 'state.db'
            if not source.exists():
                return
            partial = work / 'snapshot-partial.db'
            partial.unlink(missing_ok=True)
            with closing(sqlite3.connect(source.resolve().as_uri() + '?mode=ro', uri=True)) as reader:
                with closing(sqlite3.connect(partial)) as copy:
                    reader.backup(copy)
            with closing(sqlite3.connect(partial)) as check:
                if check.execute('PRAGMA integrity_check').fetchall() != [('ok',)]:
                    raise AssertionError('snapshot integrity check failed')
                user_version = check.execute('PRAGMA user_version').fetchone()[0]
                if user_version != args.expect_version:
                    raise AssertionError(f'snapshot user_version is {user_version}, expected {args.expect_version}')
            os.replace(partial, snapshot)

        harness.Service.stop = stop_and_snapshot
        scenarios = dict(e2e.SCENARIOS)
        if args.scenario not in scenarios:
            raise SystemExit(f'unknown scenario {args.scenario!r}; available: {", ".join(scenarios)}')
        scenarios[args.scenario]()
        if not snapshot.exists():
            raise SystemExit('the scenario never produced a state database snapshot')

        raw = snapshot.read_bytes()
        with closing(sqlite3.connect(snapshot.resolve().as_uri() + '?mode=ro', uri=True)) as db:
            dump = '\n'.join(db.iterdump())
            user_version = db.execute('PRAGMA user_version').fetchone()[0]
        patterns = [rb'ghp_[A-Za-z0-9]{20,}', rb'github_pat_[A-Za-z0-9_]{20,}', rb'\bsk-[A-Za-z0-9_-]{16,}', rb'PRIVATE KEY']
        patterns += [re.escape(needle.encode()) for needle in [harness.TOKEN, str(Path.home())]]
        for pattern in patterns:
            if re.search(pattern, raw) or re.search(pattern, dump.encode()):
                raise SystemExit(f'golden state matches {pattern!r}; refusing to write {output}')

        output.parent.mkdir(parents=True, exist_ok=True)
        staged = output.with_name(output.name + '.tmp')
        shutil.copyfile(snapshot, staged)
        os.replace(staged, output)

        provenance = {
            'ref': args.ref,
            'commit': commit,
            'scenario': args.scenario,
            'version': expected_version,
            'sha256': hashlib.sha256(raw).hexdigest(),
            'bytes': len(raw),
            'user_version': user_version,
            'generated_at': datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'),
            'sqlite_version': sqlite3.sqlite_version,
        }
        print(json.dumps(provenance, indent=2))


if __name__ == '__main__':
    main()
