#!/usr/bin/env python3
"""Check release contents using dirty, Git-free source archive fixtures."""
from pathlib import Path
import shutil
import stat
import subprocess
import tarfile
import tempfile

PROJECT = Path(__file__).resolve().parents[1]
TARGET = 'x86_64-unknown-linux-gnu'
VERSION = 'v9.9.9-guard'
MANIFEST = 'scripts/release-files.txt'

# Keep this independent of the packaging manifest: changing the release contents
# requires an explicit update to both the implementation and its contract.
RELEASE_FILES = (
    'AGENTS.md', 'CHANGELOG.md', 'CONTRIBUTING.md', 'LICENSE', 'README.md', 'SECURITY.md',
    'deploy/docker/Dockerfile', 'deploy/docker/compose.yaml', 'deploy/docker/env.example',
    'deploy/docker/sandbox.Dockerfile', 'deploy/docker/setup.sh', 'deploy/octomus-agent.service',
    'docs/architecture.md', 'docs/configuration.example.json', 'docs/configuration.md',
    'docs/cost.md', 'docs/dashboard.png', 'docs/deployment.md', 'docs/getting-started.md',
    'docs/index.md', 'docs/model-routing.md', 'docs/releasing.md', 'docs/run-evidence.md',
    'docs/sandbox.md', 'docs/threat-model.md',
)
MARKER = b'SYNTHETIC-PRIVATE-PACKAGE-GUARD-MARKER\n'
PRIVATE_FILES = (
    '.env', '.octomus/state.db',
    'deploy/docker/.env', 'deploy/docker/secrets/github_token',
    'deploy/docker/secrets/operator_token', 'deploy/docker/local-notes.md',
    'deploy/local/extra.conf', 'docs/local-notes.md', 'docs/.cache/operator-data',
    'docs/session.jsonl',
)


def source_fixture(root):
    source = root / 'source'
    source.mkdir(parents=True)
    for name in (*RELEASE_FILES, 'scripts/package.sh', MANIFEST):
        original = PROJECT / name
        # This also lets the regression run against the pre-manifest packager.
        if name == MANIFEST and not original.exists():
            continue
        destination = source / name
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(original, destination)
    binary = root / 'octomus-agent'
    binary.write_bytes(b'guard fixture executable\n')
    binary.chmod(0o755)
    assert not (source / '.git').exists()
    return source, binary, root / 'out'


def run(source, args, mask=0o022):
    return subprocess.run(['sh', str(source / 'scripts/package.sh'), *args],
                          cwd=source, capture_output=True, text=True, timeout=30, umask=mask)


def rejected(source, args, message=None):
    result = run(source, args)
    assert result.returncode != 0, (args, result.stdout)
    assert result.stderr, (args, result.returncode)
    if message:
        assert message in result.stderr, result.stderr


def package(source, binary, out, target=TARGET, mask=0o022):
    result = run(source, [VERSION, target, str(binary), str(out)], mask)
    assert result.returncode == 0, result.stderr
    archive = out / f'octomus-agent-{VERSION}-{target}.tar.gz'
    files = {name: source / name for name in RELEASE_FILES}
    files['octomus-agent'] = binary
    directories = {'octomus-agent', 'octomus-agent/docs', 'octomus-agent/deploy',
                   'octomus-agent/deploy/docker'}
    expected = directories | {f'octomus-agent/{name}' for name in files}
    with tarfile.open(archive) as tar:
        names = tar.getnames()
        assert len(names) == len(set(names)), 'archive contains duplicate entries'
        assert set(names) == expected, (f'unexpected: {set(names) - expected}',
                                       f'missing: {expected - set(names)}')
        for member in tar.getmembers():
            if member.name in directories:
                assert member.isdir() and member.mode == 0o755, (member.name, oct(member.mode))
                continue
            original = files[member.name.removeprefix('octomus-agent/')]
            assert member.isfile(), member.name
            assert member.mode == stat.S_IMODE(original.stat().st_mode), (member.name, oct(member.mode))
            data = tar.extractfile(member).read()
            assert data == original.read_bytes(), member.name
            assert MARKER not in data, member.name
    return archive


def main():
    with tempfile.TemporaryDirectory(prefix='octomus-package-guard-') as directory:
        root = Path(directory)
        source, binary, out = source_fixture(root / 'arguments')
        rejected(source, [], 'Usage')
        rejected(source, [VERSION, TARGET, str(binary), str(out), 'extra'], 'Usage')
        for version in ['9.9.9', 'v', 'vx.y.z', 'v1.0/evil', 'v1.0 evil']:
            rejected(source, [version, TARGET, str(binary), str(out)])
        for target in ['amd64', 'x86_64-pc-windows-msvc', 'aarch64-apple-darwin']:
            rejected(source, [VERSION, target, str(binary), str(out)], 'Unsupported release target')
        rejected(source, [VERSION, TARGET, str(root / 'absent'), str(out)], 'Missing binary')
        assert not out.exists() or not list(out.iterdir()), 'rejections left artifacts'
        package(source, binary, out)
        for name in PRIVATE_FILES:
            private = source / name
            private.parent.mkdir(parents=True, exist_ok=True)
            private.write_bytes(MARKER)
        # Unlisted symlinks must also be ignored, without following their targets.
        (source / 'docs/local-link').symlink_to(root)
        for mask in (0o022, 0o077):
            for target in (TARGET, 'aarch64-unknown-linux-gnu'):
                package(source, binary, root / f'dirty-{mask:o}-{target}', target, mask)
        result = run(source, [VERSION, TARGET, str(binary)])
        assert result.returncode == 0, result.stderr
        assert (source / 'dist' / f'octomus-agent-{VERSION}-{TARGET}.tar.gz').is_file()

        # Listed files, their parent directories, and the manifest itself and its
        # parent must not redirect the package inputs outside the source tree.
        symlinks = ('README.md', 'docs/configuration.md', 'deploy/docker/env.example',
                    'docs', 'deploy', 'deploy/docker', MANIFEST, 'scripts')
        for index, name in enumerate(symlinks):
            fixture = root / f'symlink-{index}'
            source, binary, out = source_fixture(fixture)
            original = source / name
            moved = fixture / 'redirected-input'
            original.rename(moved)
            original.symlink_to(moved, target_is_directory=moved.is_dir())
            rejected(source, [VERSION, TARGET, str(binary), str(out)], 'Symlink release input')
            assert not out.exists() or not list(out.iterdir()), f'{name} left an archive'

        for index, mutation in enumerate(('missing', 'directory', 'dangling')):
            source, binary, out = source_fixture(root / f'invalid-file-{index}')
            listed = source / 'README.md'
            listed.unlink()
            if mutation == 'directory':
                listed.mkdir()
            elif mutation == 'dangling':
                listed.symlink_to(root / 'absent-input')
            rejected(source, [VERSION, TARGET, str(binary), str(out)])
            assert not out.exists() or not list(out.iterdir())

        invalid_manifests = ('', '\n', '../outside\n', '/etc/passwd\n', 'docs/../README.md\n',
                             'docs//configuration.md\n', 'docs\n', 'README.md\nREADME.md\n',
                             'docs/configuration.md extra\n')
        for index, contents in enumerate(invalid_manifests):
            source, binary, out = source_fixture(root / f'invalid-manifest-{index}')
            (source / MANIFEST).write_text(contents)
            rejected(source, [VERSION, TARGET, str(binary), str(out)], 'Invalid release manifest')
            assert not out.exists() or not list(out.iterdir())
    print('PASS package guard: argument validation, exact 30-member archive, dirty Git-free source, '
          'both targets and umasks, bytes/modes, eight symlink cases and invalid inputs/manifests')


if __name__ == '__main__':
    main()
