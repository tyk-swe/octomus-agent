#!/usr/bin/env python3
"""Exercise scripts/package.sh argument validation and the archive allowlist.

The shipped artifact is the tar archive, so its fixed input allowlist and
rejection cases are what need guarding.
"""
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

PROJECT = Path(__file__).resolve().parents[1]
SCRIPT = PROJECT / 'scripts/package.sh'
TARGET = 'x86_64-unknown-linux-gnu'
VERSION = 'v9.9.9-guard'

# The archive is exactly the executable plus the documented allowlist: legal
# files, operator docs and the systemd unit. Anything else is a packaging bug.
ALLOWED = re.compile(
    r'octomus-agent(|/('
    r'octomus-agent|LICENSE|README\.md|SECURITY\.md|CHANGELOG\.md|CONTRIBUTING\.md|AGENTS\.md'
    r'|docs(|/.+)|deploy(|/.+)))/?$'
)
# Private state, credentials, transcripts, caches and migration-only fixtures
# must never appear even if the allowlist above is loosened by mistake.
DENIED = re.compile(
    r'(\.git|\.octomus|service\.lock|state\.db|\.db-(wal|shm|journal)|node_modules'
    r'|package(-lock)?\.json|/(src|internal|cmd|web|tests|target|bin|dist)/|Cargo\.|\.crate$|fixtures)'
)


def rejected(args):
    result = subprocess.run([str(SCRIPT), *args], cwd=PROJECT, capture_output=True, text=True)
    assert result.returncode != 0, (args, result.stdout)
    return result.stderr


def main():
    with tempfile.TemporaryDirectory(prefix='octomus-package-guard-') as directory:
        root = Path(directory)
        binary = root / 'octomus-agent'
        binary.write_bytes(b'guard fixture executable\n')
        binary.chmod(0o755)
        out = root / 'out'

        assert 'Usage' in rejected([])
        assert 'Usage' in rejected([VERSION, TARGET, str(binary), str(out), 'extra'])
        for version in ['9.9.9', 'v', 'vx.y.z', 'v1.0/evil', 'v1.0 evil']:
            assert rejected([version, TARGET, str(binary), str(out)]), version
        for target in ['amd64', 'x86_64-pc-windows-msvc', 'aarch64-apple-darwin']:
            assert 'Unsupported release target' in rejected([VERSION, target, str(binary), str(out)]), target
        assert 'Missing binary' in rejected([VERSION, TARGET, str(root / 'absent'), str(out)])
        assert not out.exists() or not list(out.iterdir()), 'rejections left artifacts'

        subprocess.run([str(SCRIPT), VERSION, TARGET, str(binary), str(out)], cwd=PROJECT, check=True)
        archive = out / f'octomus-agent-{VERSION}-{TARGET}.tar.gz'
        assert archive.is_file()
        with tarfile.open(archive) as tar:
            names = tar.getnames()
            assert len(names) == len(set(names)), 'archive contains duplicate entries'
            for name in names:
                assert ALLOWED.match(name), f'unexpected archive member: {name}'
                assert not DENIED.search(name), f'private state in archive: {name}'
            member = tar.extractfile('octomus-agent/octomus-agent')
            assert member.read() == binary.read_bytes()
            info = tar.getmember('octomus-agent/octomus-agent')
            assert info.mode & 0o111, 'archived executable lost its execute bit'
            assert any(n.startswith('octomus-agent/docs/') for n in names)
            assert any(n.startswith('octomus-agent/deploy/') for n in names)
        print('PASS package guard: usage/version/target/binary rejections and the exact archive allowlist')


if __name__ == '__main__':
    main()
