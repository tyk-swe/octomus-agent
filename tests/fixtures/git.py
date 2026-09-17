#!/usr/bin/env python3
"""Redirect only fixture GitHub identity to the local bare remote. All Git operations are real."""
import os
from pathlib import Path
import subprocess
import sys
args = sys.argv[1:]
if args == ['remote', 'get-url', 'origin']:
    print('https://github.com/fixture/project.git')
    sys.exit(0)
args = [str(Path(os.environ['OCTOMUS_FIXTURE']) / 'remote.git') if a == 'https://github.com/fixture/project.git' else a for a in args]
root = Path(os.environ['OCTOMUS_FIXTURE'])
# Once the first follow-up delivery lands and the caller has fetched it, the next
# read of the shared branch reports it rewound to the pre-delivery head. Reads
# while armed-but-undelivered record the planning head; that first qualifying
# read always precedes the delivery push. Requiring the delivered head in the
# caller's object store keeps the race deterministic: pre-fetch reads still
# observe the pushed head, while the dependent's later ancestry check sees the
# rewound remote with the delivered commit already local.
if (root / 'dependency-rollback').exists() and args[:2] == ['ls-remote', '--heads'] and args[-1] == 'refs/heads/octomus/existing':
    remote = str(root / 'remote.git')
    head = subprocess.check_output(['/usr/bin/git', '--git-dir', remote, 'rev-parse', 'octomus/existing'], text=True).strip()
    target = root / 'rollback-target'
    if not (root / 'first-comment-done').exists():
        if not target.exists():
            target.write_text(head)
    elif target.exists() and subprocess.call(
        ['/usr/bin/git', '-C', os.getcwd(), 'cat-file', '-e', head],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ) == 0:
        (root / 'dependency-rollback').unlink()
        subprocess.check_call(['/usr/bin/git', '--git-dir', remote, 'update-ref', 'refs/heads/octomus/existing', target.read_text().strip()])
os.execv('/usr/bin/git', ['git', *args])
