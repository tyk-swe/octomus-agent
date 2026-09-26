#!/usr/bin/env python3
"""Run only as root on a disposable systemd CI VM; never uses live service state."""
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import tempfile
import uuid

PROJECT = Path(__file__).resolve().parents[1]
if os.geteuid() != 0 or not Path('/run/systemd/system').exists():
    raise SystemExit('Requires root on a disposable systemd VM; no live validation claimed')
account = pwd.getpwnam('nobody')
root = Path(tempfile.mkdtemp(prefix='octomus-systemd-test-', dir='/opt'))
unit = 'octomus-fixture-' + uuid.uuid4().hex
try:
    for name in ['home', 'checkout', 'forbidden']:
        path = root / name
        path.mkdir()
        os.chown(path, account.pw_uid, account.pw_gid)
    root.chmod(0o755)
    script = root / 'check.sh'
    script.write_text(f'''#!/bin/sh
set -eu
touch '{root}/home/state' '{root}/checkout/source'
touch /tmp/octomus-private-fixture
if touch '{root}/forbidden/escape'; then exit 1; fi
sleep 120 &
echo $! > '{root}/home/child.pid'
''')
    script.chmod(0o755)
    source = (PROJECT / 'deploy/octomus-agent.service').read_text()
    # Validate the shipped syntax using fixture paths and an existing executable.
    unit_text = source.replace('User=octomus', 'User=nobody').replace('Group=octomus', 'Group=nogroup')
    unit_text = unit_text.replace('WorkingDirectory=/var/lib/octomus', f'WorkingDirectory={root}/home')
    unit_text = unit_text.replace('EnvironmentFile=/etc/octomus/agent.env', f'EnvironmentFile=-{root}/unused.env')
    unit_text = unit_text.replace('ReadWritePaths=/var/lib/octomus /srv/projects/octomus-agent', f'ReadWritePaths={root}/home {root}/checkout')
    unit_text = '\n'.join('ExecStart=/bin/true' if line.startswith('ExecStart=') else line for line in unit_text.splitlines()) + '\n'
    unit_file = root / 'octomus-validation.service'
    unit_file.write_text(unit_text)
    subprocess.run(['systemd-analyze', 'verify', str(unit_file)], check=True)
    properties = []
    for line in (PROJECT / 'deploy/octomus-agent.service').read_text().splitlines():
        key = line.partition('=')[0]
        if key in ['NoNewPrivileges', 'ProtectSystem', 'PrivateTmp', 'ProtectKernelTunables', 'RestrictSUIDSGID', 'KillMode', 'TimeoutStopSec']:
            properties += ['-p', line]
    properties += ['-p', f'ReadWritePaths={root}/home {root}/checkout', '-p', 'User=nobody']
    subprocess.run(['systemd-run', '--unit', unit, '--wait', '--pipe', *properties, str(script)], check=True)
    assert (root / 'home/state').exists() and (root / 'checkout/source').exists()
    assert not (root / 'forbidden/escape').exists()
    pid = (root / 'home/child.pid').read_text().strip()
    # Gone means reaped or a zombie; the state follows the last ')', since the
    # command name before it may contain spaces or parentheses.
    try:
        state = Path(f'/proc/{pid}/stat').read_text().rpartition(')')[2].split()[0]
    except (FileNotFoundError, ProcessLookupError):
        state = 'reaped'
    assert state in ['reaped', 'Z', 'X'], f'Child survived control-group cleanup (state {state})'
    print('PASS systemd: allowed writes, protected filesystem and child cleanup')
finally:
    subprocess.run(['systemctl', 'stop', unit], check=False, capture_output=True)
    subprocess.run(['systemctl', 'reset-failed', unit], check=False, capture_output=True)
    shutil.rmtree(root)
