#!/usr/bin/env python3
"""The dependency-rollback fault for git.sh: rewind the existing PR head once the first comment lands."""
import os
from pathlib import Path
import subprocess
import sys
args = sys.argv[1:]
root = Path(os.environ['OCTOMUS_FIXTURE'])
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
