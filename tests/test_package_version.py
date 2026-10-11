"""Local packaging uses the same bounded version identity as release and OCI staging."""
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

from test_release_workflow import RELEASE


PROJECT = Path(__file__).resolve().parents[1]


class PackageVersionTests(unittest.TestCase):
    def package(self, version):
        temporary = tempfile.TemporaryDirectory(prefix='octomus-package-policy-')
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        (root / 'scripts').mkdir()
        for name in ['package.sh', 'release.py', 'release-images.py']:
            shutil.copyfile(PROJECT / 'scripts' / name, root / 'scripts' / name)
        (root / 'VERSION').write_text(version + '\n')
        (root / 'scripts/release-files.txt').write_text('VERSION\n')
        (root / 'bin').mkdir()
        (root / 'bin/octomus-agent').write_text('fixture package bytes; executable smoke coverage belongs to distribution.py\n')
        (root / 'bin/go').write_text('#!/bin/sh\n[ "$1" = env ] && [ "$2" = GOARCH ] || exit 1\nprintf "amd64\\n"\n')
        (root / 'bin/go').chmod(0o700)
        env = {**os.environ, 'PATH': str(root / 'bin') + os.pathsep + os.environ['PATH']}
        result = subprocess.run(['sh', 'scripts/package.sh'], cwd=root, env=env, capture_output=True, text=True)
        return root, result

    def test_versions_rejected_consistently_before_writing_artifacts(self):
        for version in ['', 'v1.2.3', '1.2.3+build', '1' * 129, '1_2', '1/2']:
            with self.subTest(version=version):
                with self.assertRaises(ValueError):
                    RELEASE.version_for('v' + version)
                with self.assertRaises(ValueError):
                    RELEASE.IMAGES.validate_version(version)
                root, result = self.package(version)
                self.assertNotEqual(result.returncode, 0, result.stderr)
                self.assertFalse((root / 'dist').exists(), 'invalid identity wrote release artifacts')

    def test_prerelease_and_maximum_length_versions_keep_archive_identity(self):
        for version in ['1.2.3-rc.1', '1' * 128]:
            with self.subTest(version=version):
                self.assertEqual(RELEASE.version_for('v' + version), version)
                root, result = self.package(version)
                self.assertEqual(result.returncode, 0, result.stderr)
                name = 'octomus-agent-v' + version + '-x86_64-unknown-linux-gnu.tar.gz'
                archive = root / 'dist' / name
                with tarfile.open(archive) as package:
                    self.assertEqual(package.extractfile('octomus-agent/VERSION').read(), (version + '\n').encode())
                self.assertIn(name, (root / 'dist/SHA256SUMS').read_text())
