#!/usr/bin/env python3
"""Redirect only fixture GitHub identity to the local bare remote. All Git operations are real."""
import os
from pathlib import Path
import sys
args = sys.argv[1:]
if args == ['remote', 'get-url', 'origin']:
    print('https://github.com/fixture/project.git')
    sys.exit(0)
args = [str(Path(os.environ['OCTOMUS_FIXTURE']) / 'remote.git') if a == 'https://github.com/fixture/project.git' else a for a in args]
os.execv('/usr/bin/git', ['git', *args])
