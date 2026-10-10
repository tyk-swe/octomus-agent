"""Exercise the setup script without starting containers or using real credentials.

When Compose is installed (CI), its actual config parser resolves .env quoting and shell precedence. The remaining
Docker calls are recorded stubs. Without Compose, fixture resolutions still exercise the prompt/export contract.
"""
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
import unittest

PROJECT = Path(__file__).resolve().parents[1]

DOCKER = '''#!/usr/bin/env python3
import json, os, subprocess, sys
args = sys.argv[1:]
if args == ['compose', 'version']:
    print('Docker Compose test')
elif args[:1] == ['version']:
    print('28.0.0')
elif '--environment' in args:
    if os.environ.get('SETUP_REAL_DOCKER'):
        raise SystemExit(subprocess.run([os.environ['SETUP_REAL_DOCKER'], *args]).returncode)
    print('OCTOMUS_GITHUB_REPO=' + os.environ['SETUP_EFFECTIVE_REPO'])
elif args[:3] == ['compose', 'config', '--images']:
    print('octomus-agent:setup-test')
else:
    with open(os.environ['SETUP_CALLS'], 'a') as f:
        f.write(json.dumps({'args': args, 'repo': os.environ.get('OCTOMUS_GITHUB_REPO')}) + '\\n')
'''


class SetupRepositoryTests(unittest.TestCase):
    def run_setup(self, env_file, exported, effective, answers):
        directory = tempfile.TemporaryDirectory(prefix='octomus-setup-')
        self.addCleanup(directory.cleanup)
        root = Path(directory.name)
        # A socket inode satisfies setup's filesystem check; no listener or real daemon is needed.
        os.mknod(root / 'docker.sock', stat.S_IFSOCK | 0o600)
        source = (PROJECT / 'deploy/docker/setup.sh').read_text()
        self.assertIn('socket=/var/run/docker.sock', source)
        (root / 'setup.sh').write_text(source.replace('socket=/var/run/docker.sock', 'socket=' + str(root / 'docker.sock')))
        (root / '.env').write_text(env_file)
        (root / 'secrets').mkdir()
        (root / 'secrets/operator_token').write_text('synthetic-operator-token\n')
        (root / 'bin').mkdir()
        (root / 'bin/docker').write_text(DOCKER)
        (root / 'bin/docker').chmod(0o755)
        real = shutil.which('docker')
        if real and subprocess.run([real, 'compose', 'version'], capture_output=True).returncode:
            real = None
        env = {key: value for key, value in os.environ.items() if not key.startswith(('COMPOSE_', 'OCTOMUS_'))}
        env.update(PATH=str(root / 'bin') + os.pathsep + os.environ['PATH'],
                   SETUP_REAL_DOCKER=real or '', SETUP_EFFECTIVE_REPO=effective, SETUP_CALLS=str(root / 'calls'))
        if exported is not None:
            env['OCTOMUS_GITHUB_REPO'] = exported
        result = subprocess.run(['sh', str(root / 'setup.sh')], input=answers, env=env,
                                capture_output=True, text=True, timeout=15)
        calls = [json.loads(line) for line in (root / 'calls').read_text().splitlines()] if (root / 'calls').exists() else []
        return result, calls, (root / '.env').read_text()

    def test_prompt_saved_value_and_all_compose_commands_share_effective_repository(self):
        cases = [
            ('OCTOMUS_GITHUB_REPO=owner/file\n', 'owner/shell', 'owner/shell', '', 'owner/shell'),
            ('OCTOMUS_GITHUB_REPO="owner/quoted"\n', None, 'owner/quoted', '', 'owner/quoted'),
            ("OCTOMUS_GITHUB_REPO='owner/single'\n", None, 'owner/single', '', 'owner/single'),
            ('PREFIX=owner\nOCTOMUS_GITHUB_REPO="${PREFIX}/interpolated"\n', None, 'owner/interpolated', '', 'owner/interpolated'),
            ('OCTOMUS_GITHUB_REPO=owner/file\n', '', '', 'owner/entered\n', 'owner/entered'),
            ('OCTOMUS_GITHUB_REPO=OWNER/REPOSITORY\n', None, 'OWNER/REPOSITORY', 'owner/first\n', 'owner/first'),
        ]
        for file, exported, effective, answer, expected in cases:
            with self.subTest(file=file, exported=exported):
                result, calls, saved = self.run_setup(file, exported, effective, answer + 'synthetic-github-token\n')
                self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
                self.assertIn('GitHub token for ' + expected + ' only', result.stdout)
                self.assertIn('OCTOMUS_GITHUB_REPO=' + expected + '\n', saved)
                self.assertTrue(any(call['args'][:2] == ['compose', 'build'] for call in calls))
                self.assertTrue(any(call['args'][:2] == ['compose', 'up'] for call in calls))
                self.assertTrue(all(call['repo'] == expected for call in calls), calls)

    def test_invalid_effective_repository_fails_before_token_or_build(self):
        result, calls, _ = self.run_setup('OCTOMUS_GITHUB_REPO=owner/file\n', 'owner/other/nested',
                                         'owner/other/nested', 'unused-token\n')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Repository must be OWNER/REPOSITORY', result.stderr)
        self.assertNotIn('GitHub token for', result.stdout)
        self.assertEqual(calls, [])


if __name__ == '__main__':
    unittest.main()
